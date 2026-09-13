package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type dynamicTestAccounts struct{ AccountRepository }

func (dynamicTestAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": fmt.Sprint(id)}}, nil
}

// Like the model-quota regression, this refuses every non-dedicated database.
// Fixtures contain only synthetic identifiers; each test owns one exact schema.
func dynamicTestStore(t *testing.T) (*DynamicSubscriptionService, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("SUB2API_MODEL_QUOTA_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated sub2api_modelquota_test PostgreSQL")
	}
	require.NotContains(t, dsn, "://")
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })
	var name string
	require.NoError(t, admin.QueryRow("SELECT current_database()").Scan(&name))
	require.Equal(t, "sub2api_modelquota_test", name)
	schema := fmt.Sprintf("dynamic_%d", time.Now().UnixNano())
	_, err = admin.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, e := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, e) })
	db, err := sql.Open("postgres", dsn+" search_path="+schema)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(16)
	dynamicExec(t, db, `CREATE TABLE users(id BIGINT PRIMARY KEY,role TEXT DEFAULT 'user',status TEXT DEFAULT 'active',deleted_at TIMESTAMPTZ);
 CREATE TABLE groups(id BIGINT PRIMARY KEY,platform TEXT DEFAULT 'openai',subscription_type TEXT DEFAULT 'subscription',
 status TEXT DEFAULT 'active',rate_multiplier NUMERIC DEFAULT 1,weekly_limit_usd NUMERIC DEFAULT 700,
 peak_rate_enabled BOOLEAN DEFAULT false,peak_start TEXT DEFAULT '',peak_end TEXT DEFAULT '',peak_rate_multiplier NUMERIC DEFAULT 1,deleted_at TIMESTAMPTZ);
 CREATE TABLE accounts(id BIGINT PRIMARY KEY,name TEXT DEFAULT 'Test source',platform TEXT DEFAULT 'openai',type TEXT DEFAULT 'oauth',credentials JSONB,extra JSONB,deleted_at TIMESTAMPTZ);
 CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT);
 CREATE TABLE account_groups(account_id BIGINT,group_id BIGINT,PRIMARY KEY(account_id,group_id));
 CREATE TABLE user_subscriptions(id BIGINT PRIMARY KEY,user_id BIGINT,group_id BIGINT,status TEXT DEFAULT 'active',expires_at TIMESTAMPTZ DEFAULT NOW()+INTERVAL '30 days',
 daily_usage_usd NUMERIC DEFAULT 5,weekly_usage_usd NUMERIC DEFAULT 20,monthly_usage_usd NUMERIC DEFAULT 30,
 weekly_window_start TIMESTAMPTZ DEFAULT NOW()-INTERVAL '1 day',updated_at TIMESTAMPTZ DEFAULT NOW(),deleted_at TIMESTAMPTZ);
 CREATE TABLE user_group_rate_multipliers(user_id BIGINT,group_id BIGINT,rate_multiplier NUMERIC,PRIMARY KEY(user_id,group_id));
 CREATE TABLE api_keys(id BIGINT PRIMARY KEY,user_id BIGINT,group_id BIGINT,deleted_at TIMESTAMPTZ);
 CREATE TABLE usage_logs(id BIGSERIAL PRIMARY KEY,account_id BIGINT,subscription_id BIGINT,total_cost NUMERIC DEFAULT 0,created_at TIMESTAMPTZ DEFAULT NOW(),
 input_tokens BIGINT DEFAULT 0,output_tokens BIGINT DEFAULT 0,cache_creation_tokens BIGINT DEFAULT 0,cache_read_tokens BIGINT DEFAULT 0,
 account_stats_cost NUMERIC,account_rate_multiplier NUMERIC,actual_cost NUMERIC DEFAULT 0);
 INSERT INTO users(id,role) VALUES(1,'admin'),(2,'user'),(3,'user');
 INSERT INTO groups(id) VALUES(7),(8);
 INSERT INTO accounts(id,credentials) VALUES(4,'{"chatgpt_account_id":"4"}'),(5,'{"chatgpt_account_id":"5"}');
 INSERT INTO account_groups VALUES(4,7),(5,8);
 INSERT INTO user_subscriptions(id,user_id,group_id) VALUES(11,1,7),(12,2,7),(13,3,7),(21,1,8);
 INSERT INTO api_keys(id,user_id,group_id) VALUES(101,1,7),(102,2,7),(103,3,7),(104,1,7),(201,1,8);`)
	migration, err := os.ReadFile("../../migrations/238_dynamic_subscription_quotas.sql")
	require.NoError(t, err)
	dynamicExec(t, db, string(migration))
	dynamicExec(t, db, string(migration))
	recoveryMigration, err := os.ReadFile("../../migrations/239_dynamic_quota_billing_recovery.sql")
	require.NoError(t, err)
	dynamicExec(t, db, string(recoveryMigration))
	dynamicExec(t, db, string(recoveryMigration))
	v2Migration, err := os.ReadFile("../../migrations/240_dynamic_quota_v2.sql")
	require.NoError(t, err)
	dynamicExec(t, db, string(v2Migration))
	dynamicExec(t, db, string(v2Migration))
	groupMigration, err := os.ReadFile("../../migrations/241_dynamic_group_quotas.sql")
	require.NoError(t, err)
	dynamicExec(t, db, string(groupMigration))
	dynamicExec(t, db, string(groupMigration))
	seatMigration, err := os.ReadFile("../../migrations/242_dynamic_quota_fixed_seats.sql")
	require.NoError(t, err)
	dynamicExec(t, db, string(seatMigration))
	dynamicExec(t, db, string(seatMigration))
	debugMigration, err := os.ReadFile("../../migrations/243_admin_debug_weekly_quota.sql")
	require.NoError(t, err)
	dynamicExec(t, db, string(debugMigration))
	dynamicExec(t, db, string(debugMigration))
	clearMigration, err := os.ReadFile("../../migrations/244_dynamic_absorption_display_clear.sql")
	require.NoError(t, err)
	dynamicExec(t, db, string(clearMigration))
	dynamicExec(t, db, string(clearMigration))
	s := NewDynamicSubscriptionService(db, dynamicTestAccounts{}, nil, nil)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 50, time.Now().UTC().Add(6*24*time.Hour), time.Now().UTC()), nil
	}
	return s, db
}

func dynamicExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	_, err := db.Exec(q, args...)
	require.NoError(t, err)
}
func dynamicTestObservation(id int64, percent float64, reset, fetched time.Time) DynamicQuotaObservation {
	return DynamicQuotaObservation{Identity: shortOpenAIAutoResetHash(fmt.Sprint(id)), UsedPercent: percent, ResetAt: reset, WindowSeconds: 604800, FetchedAt: fetched}
}
func dynamicTestFloor() *float64 { v := 1.0; return &v }

func dynamicTestSave(t *testing.T, s *DynamicSubscriptionService, id, account int64, enabled bool) {
	t.Helper()
	q, err := s.Load(context.Background(), id)
	require.NoError(t, err)
	var revision int64
	if q != nil {
		revision = q.Revision
	}
	require.NoError(t, s.Save(context.Background(), id, DynamicSubscriptionInput{Enabled: enabled, AccountID: account, Revision: revision, Weight: 1, MaxLimitUSD: 700, FloorLimitUSD: dynamicTestFloor()}))
	if enabled {
		require.NoError(t, s.Refresh(context.Background(), account))
	}
}

func dynamicTestSettle(t *testing.T, db *sql.DB, r *DynamicQuotaReservation, key, sub int64, standard, actual float64) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	cmd := &UsageBillingCommand{DynamicQuotaReservationID: r.ID, AccountID: r.AccountID, APIKeyID: key, SubscriptionID: &sub, DynamicStandardCost: standard, SubscriptionCost: actual}
	require.NoError(t, SettleDynamicQuota(ctx, tx, cmd))
	_, err = tx.Exec(`UPDATE user_subscriptions SET daily_usage_usd=daily_usage_usd+$2,weekly_usage_usd=weekly_usage_usd+$2,monthly_usage_usd=monthly_usage_usd+$2 WHERE id=$1`, sub, actual)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO usage_logs(account_id,subscription_id,total_cost) VALUES($1,$2,$3)`, r.AccountID, sub, standard)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestDynamicQuotaPostgresOfficial024Migration(t *testing.T) {
	s, db := dynamicTestStore(t)
	dynamicTestSave(t, s, 11, 4, true)
	dynamicExec(t, db, `ALTER TABLE settings ADD COLUMN updated_at TIMESTAMPTZ`)
	modelMigration, err := os.ReadFile("../../migrations/237_user_model_request_policies.sql")
	require.NoError(t, err)
	dynamicExec(t, db, string(modelMigration))
	dynamicExec(t, db, `INSERT INTO user_model_request_policies(user_id) VALUES(1);
 INSERT INTO user_model_request_windows(user_id,model,used,resets_at) VALUES(1,'gpt-5.5',37,NOW()+INTERVAL '1 day')`)
	dynamicExec(t, db, `CREATE TABLE user_platform_quotas(platform TEXT CHECK(platform IN ('openai')));
 CREATE TABLE composite_model_routes(target_platform TEXT CHECK(target_platform IN ('openai')));
 CREATE TABLE channel_monitors(provider TEXT CHECK(provider IN ('openai')));
 CREATE TABLE channel_monitor_request_templates(provider TEXT CHECK(provider IN ('openai')));
 INSERT INTO user_platform_quotas VALUES('openai');
 INSERT INTO composite_model_routes VALUES('openai');
 INSERT INTO channel_monitors VALUES('openai');
 INSERT INTO channel_monitor_request_templates VALUES('openai');
 INSERT INTO dynamic_quota_requests(id,account_id,cycle,subscription_id,hold_standard_usd,status)
 VALUES('00000000-0000-4000-8000-000000000024',4,1,11,2,'uncertain');`)
	ledger := func() string {
		t.Helper()
		var value string
		err := db.QueryRow(`SELECT jsonb_build_object(
 'policies',(SELECT jsonb_agg(to_jsonb(p) ORDER BY subscription_id) FROM dynamic_subscription_policies p),
 'requests',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM dynamic_quota_requests r),
 'subscriptions',(SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM user_subscriptions s),
 'model_policies',(SELECT jsonb_agg(to_jsonb(p) ORDER BY user_id) FROM user_model_request_policies p),
 'model_windows',(SELECT jsonb_agg(to_jsonb(w) ORDER BY user_id,model) FROM user_model_request_windows w))::text`).Scan(&value)
		require.NoError(t, err)
		return value
	}
	before := ledger()
	migration, err := os.ReadFile("../../migrations/237_add_minimax_platform.sql")
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		dynamicExec(t, db, string(migration))
		require.Equal(t, before, ledger(), "official migration must preserve live subscriptions and unknown holds")
	}
	for _, table := range []string{"user_platform_quotas", "composite_model_routes", "channel_monitors", "channel_monitor_request_templates"} {
		dynamicExec(t, db, "INSERT INTO "+table+" VALUES('minimax')")
		_, err := db.Exec("INSERT INTO " + table + " VALUES('invalid-platform')")
		require.Error(t, err, "the platform CHECK must remain enforced")
	}
}

func TestDynamicQuotaPostgresIsolationAndReset(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	for _, p := range [][2]int64{{11, 4}, {12, 4}, {21, 5}} {
		dynamicTestSave(t, s, p[0], p[1], true)
	}
	admin, err := s.Load(ctx, 11)
	require.NoError(t, err)
	user, err := s.Load(ctx, 12)
	require.NoError(t, err)
	require.Equal(t, admin.LimitUSD, user.LimitUSD, "admin ON must count exactly like user ON")
	require.Zero(t, admin.Public().AccountID)
	q, err := s.Load(ctx, 13)
	require.NoError(t, err)
	require.Nil(t, q, "OFF user has no allocation")
	_, err = s.Begin(ctx, 101, 5)
	require.ErrorIs(t, err, ErrDynamicQuotaBinding)
	// Ordinary allocation and legacy timers cannot reset dynamic counters.
	dynamicExec(t, db, `UPDATE user_subscriptions SET weekly_usage_usd=0,weekly_window_start=NOW() WHERE id IN(11,12,13,21)`)
	var used float64
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&used))
	require.Equal(t, 20.0, used)
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=13`).Scan(&used))
	require.Zero(t, used)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	require.NotNil(t, r)
	// A later group reassignment must not make the old source reset a now-
	// unrelated subscription. Rebinding requires a separate reviewed migration.
	dynamicExec(t, db, `UPDATE user_subscriptions SET group_id=8 WHERE id=12`)
	// A genuine early reset: new 7d boundary + recovered percentage + two
	// independent observations. Verified bills settle before the cycle closes.
	now := time.Now().UTC()
	newReset := now.Add(7*24*time.Hour - time.Minute)
	fetched := now.Add(-50 * time.Second)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 0, newReset, fetched), nil
	}
	// The baseline is older than the candidate, without sleeping in tests.
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET state=jsonb_set(state,'{snapshot,fetched_at}',to_jsonb($2::text)) WHERE account_id=$1`, 4, now.Add(-2*time.Minute).Format(time.RFC3339Nano))
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, "confirming", q.Status)
	require.Equal(t, int64(1), q.Cycle)
	require.Equal(t, 20.0, q.UsedUSD)
	_, err = s.Begin(ctx, 104, 4)
	require.ErrorIs(t, err, ErrDynamicQuotaUnavailable)
	dynamicTestSettle(t, db, r, 101, 11, 2, 3)
	// The V2 cycle can retain usage that predates the current native week.
	dynamicExec(t, db, `UPDATE dynamic_subscription_policies SET cycle_used_usd=53 WHERE subscription_id=11`)
	fetched = now
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, int64(2), q.Cycle)
	require.Zero(t, q.UsedUSD)
	require.Positive(t, q.LimitUSD, "confirmed reset must apply its new grant without waiting for an increase threshold")
	q, err = s.Load(ctx, 21)
	require.NoError(t, err)
	require.Equal(t, int64(1), q.Cycle)
	require.Equal(t, 20.0, q.UsedUSD, "same user's other source must not reset")
	q, err = s.Load(ctx, 12)
	require.NoError(t, err)
	require.Equal(t, 20.0, q.UsedUSD, "group reassignment must not follow the old source reset")
	var daily, monthly float64
	require.NoError(t, db.QueryRow(`SELECT daily_usage_usd,monthly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&daily, &monthly))
	require.Equal(t, 8.0, daily)
	require.Equal(t, 33.0, monthly)
	var archived float64
	require.NoError(t, db.QueryRow(`SELECT (details->'previous_usage_usd'->>'11')::numeric FROM dynamic_quota_events WHERE account_id=4 AND kind='reset_confirmed'`).Scan(&archived))
	require.Equal(t, 53.0, archived, "archive the effective V2 cycle usage, not only the native week")
	require.NoError(t, s.Refresh(ctx, 4))
	var events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE kind='reset_confirmed'`).Scan(&events))
	require.Equal(t, 1, events)
}

func TestDynamicQuotaPostgresActivationAndToggle(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	require.NotNil(t, r, "pre-opt-in traffic must be tracked")
	err = s.Save(ctx, 11, DynamicSubscriptionInput{Enabled: true, AccountID: 4, Weight: 1, MaxLimitUSD: 700, FloorLimitUSD: dynamicTestFloor()})
	require.NoError(t, err)
	dynamicTestSettle(t, db, r, 101, 11, 10, 10) // baseline changed from cycle 0 to 1; this is NOT a reset.
	dynamicTestSave(t, s, 11, 4, true)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 30.0, q.UsedUSD)
	dynamicTestSave(t, s, 11, 4, false)
	r, err = s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicTestSettle(t, db, r, 101, 11, 15, 15)
	dynamicTestSave(t, s, 11, 4, true)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 45.0, q.UsedUSD)
	require.GreaterOrEqual(t, q.usedStandard, 45.0)
	require.Equal(t, int64(1), q.Cycle)
	err = s.Save(ctx, 11, DynamicSubscriptionInput{Enabled: true, AccountID: 4, Revision: 1, Weight: 1, MaxLimitUSD: 1000, FloorLimitUSD: dynamicTestFloor()})
	require.ErrorIs(t, err, ErrDynamicQuotaChanged)
	dynamicExec(t, db, `UPDATE account_groups SET account_id=5 WHERE account_id=4`)
	_, err = s.Begin(ctx, 101, 5)
	require.ErrorIs(t, err, ErrDynamicQuotaBinding)
}

func TestDynamicQuotaPostgresAdminReadDoesNotMutateAccounting(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	for _, key := range []int64{101, 104, 102, 201} {
		account := int64(4)
		if key == 201 {
			account = 5
		}
		r, err := s.Begin(ctx, key, account)
		require.NoError(t, err)
		if key == 104 {
			r.MarkDispatched()
			r.Finish(nil, context.Canceled, false)
		}
	}
	status, err := s.AdminStatus(ctx, 11)
	require.NoError(t, err)
	require.False(t, status.Policy.Enabled)
	require.Zero(t, status.Policy.AccountID)
	raw, err := json.Marshal(status)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.NotContains(t, fields, "subscription_pending_requests", "accounting belongs in its separate report")
	require.Nil(t, status.Policy.FloorLimitUSD, "do not invent a protection amount")
	// Read-only diagnostics must not enable a policy or release old accounting.
	var policies, unresolved int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_subscription_policies`).Scan(&policies))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_requests WHERE status IN ('pending','uncertain')`).Scan(&unresolved))
	require.Zero(t, policies)
	require.Equal(t, 4, unresolved)
}

func TestDynamicQuotaPostgresNativeProtectionSettings(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	for _, p := range [][2]int64{{11, 4}, {12, 4}, {21, 5}} {
		dynamicTestSave(t, s, p[0], p[1], true)
	}
	before, err := s.Load(ctx, 21)
	require.NoError(t, err)
	// Legacy pool percentages no longer apply, even after a restart/refresh.
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET usage_ceiling_percent=1,config_revision=7`)
	dynamicExec(t, db, `UPDATE accounts SET extra='{"auto_pause_7d_threshold":0.4925}' WHERE id=4`)
	for _, id := range []int64{11, 12} {
		q, err := s.Load(ctx, id)
		require.NoError(t, err)
		require.Equal(t, 49.25, q.pool.stopPercent())
		require.Equal(t, "upstream_reserve", q.Status)
		require.ErrorIs(t, q.checkReady(), ErrDynamicQuotaExhausted, "reserve is a local limit, not an upstream 503")
		require.Equal(t, 20.0, q.UsedUSD)
		require.Equal(t, int64(1), q.Cycle)
	}
	for _, key := range []int64{101, 103} { // Includes an OFF member's consumption on the protected pool.
		_, err = s.Begin(ctx, key, 4)
		require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
	}
	after, err := s.Load(ctx, 21)
	require.NoError(t, err)
	require.Equal(t, before, after, "source 4 settings must not touch source 5")
	r, err := s.Begin(ctx, 201, 5)
	require.NoError(t, err)
	r.RejectBeforeForward()
	for _, tc := range []struct {
		name, extra, global string
		ceiling             float64
		denied              bool
	}{
		{"global default", `{}`, `{"default_threshold_7d":0.5}`, 50, true},
		{"zero inherits", `{"auto_pause_7d_threshold":0}`, `{"default_threshold_7d":0.5}`, 50, true},
		{"account override", `{"auto_pause_7d_threshold":0.9525}`, `{"default_threshold_7d":0.5}`, 95.25, false},
		{"explicit disable", `{"auto_pause_7d_threshold":0.5,"auto_pause_7d_disabled":true}`, `{"default_threshold_7d":0.5}`, 100, false},
		{"no weekly threshold", `{}`, `{"default_threshold_5h":0.1}`, 100, false},
		{"no hidden one percent reserve", `{"auto_pause_7d_threshold":0.5025}`, `{}`, 50.25, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dynamicExec(t, db, `UPDATE accounts SET extra=$1::jsonb WHERE id=4`, tc.extra)
			dynamicExec(t, db, `INSERT INTO settings VALUES('ops_advanced_settings',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, `{"openai_account_quota_auto_pause":`+tc.global+`}`)
			q, err := s.Load(ctx, 11)
			require.NoError(t, err)
			require.InDelta(t, tc.ceiling, q.pool.stopPercent(), 1e-8)
			r, err := s.Begin(ctx, 101, 4) // Fresh native changes apply before any periodic refresh.
			if tc.denied {
				require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
			} else {
				require.NoError(t, err)
				r.RejectBeforeForward()
			}
		})
	}
	// Refresh and subscriber edits must not rewrite native settings or reset usage.
	require.NoError(t, s.Refresh(ctx, 4))
	dynamicTestSave(t, s, 11, 4, true)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 20.0, q.UsedUSD)
	require.Equal(t, int64(1), q.Cycle)
	require.Nil(t, q.ConfirmedAt)
	require.InDelta(t, 50.25, q.pool.stopPercent(), 1e-8)
	var legacy float64
	require.NoError(t, db.QueryRow(`SELECT usage_ceiling_percent FROM dynamic_quota_pools WHERE account_id=4`).Scan(&legacy))
	require.Equal(t, 1.0, legacy, "leave applied migration and historical settings intact but unused")
	// A settings failure must stop new protected traffic, not lose an already billed request.
	dynamicTestSave(t, s, 21, 5, false)
	r, err = s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE settings SET value='invalid json'`)
	_, err = s.Begin(ctx, 101, 4)
	require.ErrorIs(t, err, ErrDynamicQuotaUnavailable)
	q, err = s.Load(ctx, 21)
	require.NoError(t, err, "disabled subscriptions do not acquire a new settings dependency")
	require.False(t, q.Enabled)
	dynamicTestSettle(t, db, r, 101, 11, 1, 1)
	var resets int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE kind='reset_confirmed'`).Scan(&resets))
	require.Zero(t, resets)
}

func TestDynamicQuotaPostgresValidationAndConcurrency(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	queries := 0
	oldFetch := s.fetch
	s.fetch = func(ctx context.Context, id int64) (DynamicQuotaObservation, error) {
		queries++
		return oldFetch(ctx, id)
	}
	for _, p := range []struct{ id, account int64 }{{999, 4}, {11, 5}} {
		err := s.Save(ctx, p.id, DynamicSubscriptionInput{Enabled: true, AccountID: p.account, Weight: 1, MaxLimitUSD: 700, FloorLimitUSD: dynamicTestFloor()})
		require.Error(t, err)
	}
	require.Zero(t, queries)
	var pools int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_pools`).Scan(&pools))
	require.Zero(t, pools)
	dynamicTestSave(t, s, 11, 4, true)
	dynamicExec(t, db, `UPDATE dynamic_subscription_policies SET allocated_standard_usd=used_standard_usd+0.03 WHERE subscription_id=11`)
	var wg sync.WaitGroup
	results := make(chan *DynamicQuotaReservation, 24)
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := int64(101)
			if i%2 == 1 {
				key = 104
			}
			r, e := s.Begin(ctx, key, 4)
			if e != nil {
				errs <- e
			} else {
				results <- r
			}
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	var accepted []*DynamicQuotaReservation
	for r := range results {
		accepted = append(accepted, r)
	}
	for e := range errs {
		require.ErrorIs(t, e, ErrDynamicQuotaExhausted)
	}
	require.Len(t, accepted, 3, "all API keys share one atomic allowance")
	for _, r := range accepted {
		r.RejectBeforeForward()
	}
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	require.NotNil(t, r)
	r.MarkDispatched()
	r.Finish(nil, context.Canceled, false)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&status))
	require.Equal(t, "uncertain", status)
	// Unknown disconnect is retained, never refunded by a timer or save toggle.
	err = s.Save(ctx, 11, DynamicSubscriptionInput{AccountID: 4, Revision: 1, Weight: 1, MaxLimitUSD: 700, FloorLimitUSD: dynamicTestFloor()})
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&status))
	require.Equal(t, "uncertain", status, "saving must not waive or discard this request")
}

func TestDynamicQuotaPostgresTransactionRollback(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	sub := int64(11)
	cmd := &UsageBillingCommand{DynamicQuotaReservationID: r.ID, AccountID: 4, APIKeyID: 101, SubscriptionID: &sub, DynamicStandardCost: 2, SubscriptionCost: 3}
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, SettleDynamicQuota(ctx, tx, cmd))
	require.NoError(t, tx.Rollback())
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 20.0, q.usedStandard)
	dynamicTestSettle(t, db, r, 101, 11, 2, 3)
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.ErrorIs(t, SettleDynamicQuota(ctx, tx, cmd), ErrUsageBillingRequestConflict)
	require.NoError(t, tx.Rollback())
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 22.0, q.usedStandard)
	require.Equal(t, 23.0, q.UsedUSD)
	// Rejected stale source never calls metadata, and database errors remain errors.
	s.fetch = func(context.Context, int64) (DynamicQuotaObservation, error) {
		return DynamicQuotaObservation{}, errors.New("synthetic metadata failure")
	}
	require.ErrorIs(t, s.Refresh(ctx, 4), ErrDynamicQuotaUnavailable)
	require.False(t, strings.Contains(fmt.Sprint(q.Public()), "credential"))
}

func TestDynamicQuotaHTTPAdmissionPostgres(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	dynamicExec(t, db, `UPDATE dynamic_subscription_policies SET allocated_standard_usd=used_standard_usd WHERE subscription_id=11`)
	gateway := &OpenAIGatewayService{DynamicQuotas: s}
	account := &Account{ID: 4}
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test"}`))
			c.Set("api_key", &APIKey{ID: 101})
			var err error
			switch path {
			case "/v1/responses":
				_, err = gateway.Forward(ctx, c, account, nil)
			case "/v1/chat/completions":
				_, err = gateway.ForwardAsChatCompletions(ctx, c, account, nil, "", "")
			default:
				_, err = gateway.ForwardAsAnthropic(ctx, c, account, nil, "", "")
			}
			require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
			require.Equal(t, 429, w.Code)
			require.Contains(t, w.Body.String(), "DYNAMIC_QUOTA_EXHAUSTED")
			require.True(t, HasOpsClientBusinessLimited(c))
			require.Nil(t, GetOpsCyberPolicy(c))
		})
	}
	var holds int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_requests`).Scan(&holds))
	require.Zero(t, holds)
	// A deterministic local error after admission is not an unknown upstream
	// disconnect and must not leave an unresolvable hold.
	dynamicExec(t, db, `UPDATE dynamic_subscription_policies SET allocated_standard_usd=used_standard_usd+10 WHERE subscription_id=11`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 101})
	_, err := gateway.withDynamicQuotaForward(ctx, c, account, func(context.Context) (*OpenAIForwardResult, error) {
		return nil, errors.New("synthetic pre-dispatch validation failure")
	})
	require.Error(t, err)
	var pending int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_requests WHERE status IN('pending','uncertain')`).Scan(&pending))
	require.Zero(t, pending)
}
