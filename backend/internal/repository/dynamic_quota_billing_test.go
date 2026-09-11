package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Exercise the real canonical billing transaction (not just the quota helper).
func TestDynamicQuotaBillingPostgres(t *testing.T) {
	dsn := os.Getenv("SUB2API_MODEL_QUOTA_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated sub2api_modelquota_test PostgreSQL")
	}
	require.False(t, strings.Contains(dsn, "://"))
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer admin.Close()
	var name string
	require.NoError(t, admin.QueryRow("SELECT current_database()").Scan(&name))
	require.Equal(t, "sub2api_modelquota_test", name)
	schema := fmt.Sprintf("dynamic_billing_%d", time.Now().UnixNano())
	_, err = admin.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	defer admin.Exec("DROP SCHEMA " + schema + " CASCADE")
	db, err := sql.Open("postgres", dsn+" search_path="+schema)
	require.NoError(t, err)
	defer db.Close()
	exec := func(q string, args ...any) { t.Helper(); _, e := db.Exec(q, args...); require.NoError(t, e) }
	exec(`CREATE TABLE accounts(id BIGINT PRIMARY KEY);
 CREATE TABLE groups(id BIGINT PRIMARY KEY,deleted_at TIMESTAMPTZ);
 CREATE TABLE user_subscriptions(id BIGINT PRIMARY KEY,group_id BIGINT,deleted_at TIMESTAMPTZ,weekly_window_start TIMESTAMPTZ DEFAULT NOW(),
 daily_usage_usd NUMERIC DEFAULT 5,weekly_usage_usd NUMERIC DEFAULT 20,monthly_usage_usd NUMERIC DEFAULT 30,updated_at TIMESTAMPTZ);
 CREATE TABLE usage_billing_dedup(id BIGSERIAL PRIMARY KEY,request_id TEXT,api_key_id BIGINT,request_fingerprint TEXT,UNIQUE(request_id,api_key_id));
 CREATE TABLE usage_billing_dedup_archive(request_id TEXT,api_key_id BIGINT,request_fingerprint TEXT,PRIMARY KEY(request_id,api_key_id));
 INSERT INTO accounts VALUES(4),(5);INSERT INTO groups(id) VALUES(7);INSERT INTO user_subscriptions(id,group_id) VALUES(11,7);`)
	migration, err := os.ReadFile("../../migrations/238_dynamic_subscription_quotas.sql")
	require.NoError(t, err)
	exec(string(migration))
	recoveryMigration, err := os.ReadFile("../../migrations/239_dynamic_quota_billing_recovery.sql")
	require.NoError(t, err)
	exec(string(recoveryMigration))
	v2Migration, err := os.ReadFile("../../migrations/240_dynamic_quota_v2.sql")
	require.NoError(t, err)
	exec(string(v2Migration))
	exec(`INSERT INTO dynamic_quota_pools(account_id,state) VALUES(4,'{"cycle":1}'),(5,'{"cycle":1}');
 INSERT INTO dynamic_subscription_policies(subscription_id,account_id,enabled,max_limit_usd,floor_limit_usd,used_standard_usd,cycle_used_usd,allocated_standard_usd) VALUES(11,4,true,100,10,20,20,100);`)
	reserve := func() string {
		t.Helper()
		id := uuid.NewString()
		exec(`INSERT INTO dynamic_quota_requests(id,account_id,cycle,subscription_id,api_key_id,hold_standard_usd) VALUES($1,4,1,11,101,2)`, id)
		return id
	}
	rID := reserve()
	subID := int64(11)
	command := service.UsageBillingCommand{RequestID: "synthetic-request-1", DynamicQuotaReservationID: rID, APIKeyID: 101, UserID: 1, AccountID: 4, AccountType: "oauth", SubscriptionID: &subID, DynamicStandardCost: 2, SubscriptionCost: 3}
	repo := NewUsageBillingRepository(nil, db)
	query, _ := buildUsageLogBatchInsertQuery(nil, nil)
	cols := strings.Split(strings.Split(strings.SplitN(query, "(", 2)[1], ") AS (VALUES")[0], ",")[1:]
	require.Len(t, cols, len(usageLogInsertArgTypes))
	definitions := []string{"id BIGSERIAL PRIMARY KEY"}
	for i, column := range cols {
		definitions = append(definitions, strings.TrimSpace(column)+" "+usageLogInsertArgTypes[i])
	}
	exec("CREATE TABLE usage_logs (" + strings.Join(definitions, ",") + ",UNIQUE(request_id,api_key_id))")
	logFor := func(cmd service.UsageBillingCommand) *service.UsageLog {
		return &service.UsageLog{RequestID: cmd.RequestID, APIKeyID: cmd.APIKeyID, UserID: cmd.UserID, AccountID: cmd.AccountID,
			SubscriptionID: cmd.SubscriptionID, TotalCost: cmd.DynamicStandardCost, ActualCost: cmd.SubscriptionCost,
			Model: "synthetic-model", CreatedAt: time.Now().UTC()}
	}
	command.UsageLog = logFor(command)
	privateAgent, privateIP, privateSession := "synthetic-private-agent", "192.0.2.1", "synthetic-private-session"
	command.UsageLog.UserAgent, command.UsageLog.IPAddress, command.UsageLog.SessionID = &privateAgent, &privateIP, &privateSession
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan *service.UsageBillingApplyResult, 10)
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); cmd := command; r, e := repo.Apply(ctx, &cmd); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	applied := 0
	for e := range errs {
		require.NoError(t, e)
	}
	for r := range results {
		if r.Applied {
			applied++
		}
	}
	require.Equal(t, 1, applied)
	var receipt []byte
	require.NoError(t, db.QueryRow(`SELECT billing_receipt FROM dynamic_quota_requests WHERE id=$1`, rID).Scan(&receipt))
	for _, private := range []string{privateAgent, privateIP, privateSession} {
		require.NotContains(t, string(receipt), private, "durable recovery metadata must not duplicate private usage fields")
	}
	var loggedAgent, loggedIP, loggedSession string
	require.NoError(t, db.QueryRow(`SELECT user_agent,ip_address,session_id FROM usage_logs WHERE request_id=$1`, command.RequestID).Scan(&loggedAgent, &loggedIP, &loggedSession))
	require.Equal(t, privateAgent, loggedAgent)
	require.Equal(t, privateIP, loggedIP)
	require.Equal(t, privateSession, loggedSession)
	var daily, weekly, monthly, standard float64
	require.NoError(t, db.QueryRow(`SELECT daily_usage_usd,weekly_usage_usd,monthly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&daily, &weekly, &monthly))
	require.Equal(t, 8.0, daily)
	require.Equal(t, 23.0, weekly)
	require.Equal(t, 33.0, monthly)
	require.NoError(t, db.QueryRow(`SELECT used_standard_usd FROM dynamic_subscription_policies WHERE subscription_id=11`).Scan(&standard))
	require.Equal(t, 22.0, standard)
	t.Run("receipt_subscription_matches_its_charge", func(t *testing.T) {
		bill := command
		bill.RequestID, bill.DynamicQuotaReservationID = "wrong-subscription-log", reserve()
		bill.UsageLog = logFor(bill)
		otherSub := int64(12)
		bill.UsageLog.SubscriptionID = &otherSub
		_, err := repo.Apply(ctx, &bill)
		require.ErrorIs(t, err, service.ErrDynamicQuotaBinding)
		var receiptMissing bool
		require.NoError(t, db.QueryRow(`SELECT billing_receipt IS NULL FROM dynamic_quota_requests WHERE id=$1`, bill.DynamicQuotaReservationID).Scan(&receiptMissing))
		require.True(t, receiptMissing, "reject mismatched ownership before saving or charging a receipt")
	})
	t.Run("operator_absorbed_request_cannot_be_rebilled", func(t *testing.T) {
		bill := command
		bill.RequestID, bill.DynamicQuotaReservationID = "operator-absorbed", reserve()
		bill.UsageLog = logFor(bill)
		exec(`UPDATE dynamic_quota_requests SET operator_absorbed_at=NOW() WHERE id=$1`, bill.DynamicQuotaReservationID)
		_, err := repo.Apply(ctx, &bill)
		require.ErrorIs(t, err, service.ErrDynamicQuotaBinding)
		var receiptMissing bool
		require.NoError(t, db.QueryRow(`SELECT billing_receipt IS NULL FROM dynamic_quota_requests WHERE id=$1`, bill.DynamicQuotaReservationID).Scan(&receiptMissing))
		require.True(t, receiptMissing)
		var late []byte
		var covered float64
		require.NoError(t, db.QueryRow(`SELECT late_billing_receipt,operator_absorbed_standard_usd FROM dynamic_quota_requests WHERE id=$1`, bill.DynamicQuotaReservationID).Scan(&late, &covered))
		require.NotEmpty(t, late)
		require.Equal(t, bill.DynamicStandardCost, covered)
		_, err = repo.Apply(ctx, &bill)
		require.ErrorIs(t, err, service.ErrDynamicQuotaBinding)
		var claims int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, bill.RequestID).Scan(&claims))
		require.Zero(t, claims, "a late receipt for reporting is not a customer charge")
	})
	// A new reservation for an already-billed upstream request is released,
	// while the settled reservation and native dollar counters remain unchanged.
	duplicate := command
	duplicate.DynamicQuotaReservationID = reserve()
	r, err := repo.Apply(ctx, &duplicate)
	require.NoError(t, err)
	require.False(t, r.Applied)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, duplicate.DynamicQuotaReservationID).Scan(&status))
	require.Equal(t, "rejected", status)
	// Archived canonical keys are just as authoritative as live dedup keys.
	exec(`INSERT INTO usage_billing_dedup_archive SELECT request_id,api_key_id,request_fingerprint FROM usage_billing_dedup WHERE request_id=$1`, command.RequestID)
	exec(`DELETE FROM usage_billing_dedup WHERE request_id=$1`, command.RequestID)
	archived := command
	archived.DynamicQuotaReservationID = reserve()
	r, err = repo.Apply(ctx, &archived)
	require.NoError(t, err)
	require.False(t, r.Applied)
	require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, archived.DynamicQuotaReservationID).Scan(&status))
	require.Equal(t, "rejected", status)
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&weekly))
	require.Equal(t, 23.0, weekly, "an archived bill is never charged again")
	// A failure after quota settlement rolls back that settlement and dedup key.
	failed := command
	failed.RequestID = "synthetic-request-2"
	failed.DynamicQuotaReservationID = reserve()
	failed.UsageLog = logFor(failed)
	exec(`UPDATE user_subscriptions SET deleted_at=NOW() WHERE id=11`)
	_, err = repo.Apply(ctx, &failed)
	require.Error(t, err)
	require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, failed.DynamicQuotaReservationID).Scan(&status))
	require.Equal(t, "pending", status)
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, failed.RequestID).Scan(&n))
	require.Zero(t, n)
	require.NoError(t, db.QueryRow(`SELECT used_standard_usd FROM dynamic_subscription_policies WHERE subscription_id=11`).Scan(&standard))
	require.Equal(t, 22.0, standard)
	// Neither a different account nor a new cycle may receive a late charge.
	exec(`UPDATE user_subscriptions SET deleted_at=NULL WHERE id=11`)
	wrong := failed
	wrong.AccountID = 5
	_, err = repo.Apply(ctx, &wrong)
	require.ErrorIs(t, err, service.ErrDynamicQuotaBinding)
	exec(`UPDATE dynamic_quota_pools SET state='{"cycle":2}' WHERE account_id=4`)
	_, err = repo.Apply(ctx, &failed)
	require.ErrorIs(t, err, service.ErrDynamicQuotaBinding)

	t.Run("durable_receipt_atomic_log_and_replay", func(t *testing.T) {
		exec(`UPDATE dynamic_quota_pools SET state='{"cycle":1}' WHERE account_id=4`)
		bill := command
		bill.RequestID, bill.DynamicQuotaReservationID = "frozen-receipt", reserve()
		bill.SubscriptionCost = 0.000078125
		bill.UsageLog = &service.UsageLog{RequestID: bill.RequestID, APIKeyID: bill.APIKeyID, UserID: bill.UserID,
			AccountID: bill.AccountID, SubscriptionID: &subID, Model: "synthetic-model", InputTokens: 10,
			TotalCost: bill.DynamicStandardCost, ActualCost: service.QuantizeUsageBillingAmount(bill.SubscriptionCost), CreatedAt: time.Now().UTC()}
		// Inject failure AFTER monetary effects but before commit.
		exec(`ALTER TABLE usage_logs ADD CONSTRAINT synthetic_fail CHECK(request_id<>'frozen-receipt')`)
		_, err := repo.Apply(ctx, &bill)
		require.Error(t, err)
		var raw []byte
		require.NoError(t, db.QueryRow(`SELECT status,billing_receipt FROM dynamic_quota_requests WHERE id=$1`, bill.DynamicQuotaReservationID).Scan(&status, &raw))
		require.Equal(t, "pending", status)
		require.NotEmpty(t, raw, "receipt survives transaction rollback")
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, bill.RequestID).Scan(&n))
		require.Zero(t, n)
		exec(`ALTER TABLE usage_logs DROP CONSTRAINT synthetic_fail`)
		// A fresh repo/process replays the frozen command, not a recalculated price.
		var replay service.UsageBillingCommand
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, bill.RequestFingerprint, replay.RequestFingerprint)
		var before, after float64
		require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&before))
		for i := 0; i < 2; i++ {
			result, err := NewUsageBillingRepository(nil, db).Apply(ctx, &replay)
			require.NoError(t, err)
			require.Equal(t, i == 0, result.Applied)
		}
		require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&after))
		require.InDelta(t, replay.SubscriptionCost, after-before, 1e-12)
		var logCost, ledgerCost float64
		require.NoError(t, db.QueryRow(`SELECT l.actual_cost,d.actual_cost_usd FROM usage_logs l JOIN dynamic_quota_requests d ON d.id=$1
 WHERE l.request_id=$2 AND l.api_key_id=$3`, replay.DynamicQuotaReservationID, replay.RequestID, replay.APIKeyID).Scan(&logCost, &ledgerCost))
		require.Equal(t, replay.SubscriptionCost, logCost)
		require.Equal(t, logCost, ledgerCost)
		replay.SubscriptionCost++
		_, err = repo.Apply(ctx, &replay)
		require.Error(t, err, "a persisted receipt cannot be silently repriced")
	})
	t.Run("native_reset_does_not_receive_an_old_bill", func(t *testing.T) {
		exec(`UPDATE dynamic_subscription_policies SET enabled=false WHERE subscription_id=11`)
		bill := command
		bill.RequestID, bill.DynamicQuotaReservationID = "native-period-bound", reserve()
		bill.UsageLog = logFor(bill)
		exec(`UPDATE user_subscriptions SET deleted_at=NOW() WHERE id=11`)
		_, err := repo.Apply(ctx, &bill)
		require.Error(t, err)
		exec(`UPDATE user_subscriptions SET deleted_at=NULL,weekly_window_start=NOW()+INTERVAL '1 day' WHERE id=11`)
		_, err = repo.Apply(ctx, &bill)
		require.ErrorIs(t, err, service.ErrDynamicQuotaBinding)
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, bill.RequestID).Scan(&n))
		require.Zero(t, n)
	})
}
