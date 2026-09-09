package repository

import (
	"context"
	"database/sql"
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
	exec(`INSERT INTO dynamic_quota_pools(account_id,state) VALUES(4,'{"cycle":1}'),(5,'{"cycle":1}');
 INSERT INTO dynamic_subscription_policies(subscription_id,account_id,enabled,max_limit_usd,used_standard_usd,allocated_standard_usd) VALUES(11,4,true,100,20,100);`)
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
	var daily, weekly, monthly, standard float64
	require.NoError(t, db.QueryRow(`SELECT daily_usage_usd,weekly_usage_usd,monthly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&daily, &weekly, &monthly))
	require.Equal(t, 8.0, daily)
	require.Equal(t, 23.0, weekly)
	require.Equal(t, 33.0, monthly)
	require.NoError(t, db.QueryRow(`SELECT used_standard_usd FROM dynamic_subscription_policies WHERE subscription_id=11`).Scan(&standard))
	require.Equal(t, 22.0, standard)
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
	// A failure after quota settlement rolls back that settlement and dedup key.
	failed := command
	failed.RequestID = "synthetic-request-2"
	failed.DynamicQuotaReservationID = reserve()
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
}
