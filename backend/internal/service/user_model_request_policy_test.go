package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	_ "github.com/lib/pq"
)

func TestUserModelRequestRules(t *testing.T) {
	rules, err := NormalizeUserModelRequestRules([]UserModelRequestRule{{Model: " GPT-5.6 ", Mode: "limited", RequestLimit: 3, WindowHours: 6}})
	if err != nil {
		t.Fatal(err)
	}
	p := &UserModelRequestPolicy{Enabled: true, Rules: rules}
	for _, name := range []string{"gpt-5.6", "gpt-5.6-sol", "gpt-5.6-sol-high", "models/gpt-5.6-sol", "openai/gpt-5.6-sol-low"} {
		if !p.Allows(name) {
			t.Errorf("equivalent rejected: %s", name)
		}
	}
	for _, name := range []string{"gpt-5.6-luna", "gpt-unknown-low", "codex-auto-review", "gpt-5.6-sol-future"} {
		if !p.Allows(name) {
			t.Errorf("unconfigured model was restricted: %s", name)
		}
	}
	p.Rules = append(p.Rules, UserModelRequestRule{Model: "gpt-5.6-luna", Mode: "deny"})
	if p.Allows("gpt-5.6-luna-high") {
		t.Fatal("explicit model ban bypassed by reasoning alias")
	}
	for _, rule := range []UserModelRequestRule{
		{Model: "gpt-*", Mode: "unlimited"}, {Model: "sol", Mode: "limited", RequestLimit: 0, WindowHours: 1},
		{Model: "sol", Mode: "limited", RequestLimit: 1, WindowHours: 0}, {Model: "sol", Mode: "limited", RequestLimit: 1, WindowHours: 721}, {Model: "sol", Mode: "inherit"},
		{Model: "sol", Mode: "limited", RequestLimit: 1, WindowMode: "weekly", WindowHours: 24},
		{Model: "sol", Mode: "limited", RequestLimit: 0, WindowMode: "daily"},
	} {
		if _, err := NormalizeUserModelRequestRules([]UserModelRequestRule{rule}); err == nil {
			t.Errorf("invalid rule accepted: %+v", rule)
		}
	}
	daily, err := NormalizeUserModelRequestRules([]UserModelRequestRule{{Model: "gpt-5.6-luna", Mode: "limited", RequestLimit: 5, WindowMode: "daily", WindowHours: 24}})
	if err != nil || daily[0].WindowHours != 0 || daily[0].WindowMode != "daily" || rules[0].WindowMode != "hours" {
		t.Fatalf("daily normalization or legacy hourly compatibility failed: %v", err)
	}
	if _, err := NormalizeUserModelRequestRules([]UserModelRequestRule{{Model: "gpt-5.6", Mode: "unlimited"}, {Model: "gpt-5.6-sol", Mode: "unlimited"}}); err == nil {
		t.Fatal("duplicate canonical rule accepted")
	}
	if !(&UserModelRequestPolicy{Enabled: true}).Allows("gpt-5.6-sol") {
		t.Fatal("empty policy should impose no model restrictions")
	}
	list := p.Allowlist().FilterForListing([]string{"gpt-5.6-sol", "gpt-5.6-sol-high", "gpt-5.6-luna"})
	if len(list) != 2 {
		t.Fatalf("listing disagrees with admission: %v", list)
	}
}

func TestUserModelRequestEvidence(t *testing.T) {
	t.Run("pre-dispatch failure", func(t *testing.T) {
		e := &UserModelRequestEvidence{}
		if !e.RefundHTTP(403, nil) {
			t.Fatal("local rejection charged")
		}
	})
	t.Run("unknown disconnect", func(t *testing.T) {
		e := &UserModelRequestEvidence{}
		if e.RefundHTTP(200, context.Canceled) {
			t.Fatal("unknown cancellation free")
		}
	})
	t.Run("retry then success", func(t *testing.T) {
		e := &UserModelRequestEvidence{}
		e.Observe(nil, &UpstreamFailoverError{StatusCode: 503}, false)
		if !e.RefundHTTP(503, nil) {
			t.Fatal("unexecuted failure charged")
		}
		e.Observe(&OpenAIForwardResult{}, nil, true)
		if e.RefundHTTP(200, nil) {
			t.Fatal("successful retry refunded")
		}
	})
	t.Run("partial usage", func(t *testing.T) {
		e := &UserModelRequestEvidence{}
		e.Observe(&OpenAIForwardResult{Usage: OpenAIUsage{InputTokens: 10}}, context.Canceled, false)
		if e.RefundHTTP(200, context.Canceled) {
			t.Fatal("consumed partial response refunded")
		}
	})
	t.Run("rejection followed by unknown attempt", func(t *testing.T) {
		e := &UserModelRequestEvidence{}
		e.Observe(nil, &UpstreamFailoverError{StatusCode: 503}, false)
		e.Observe(nil, context.Canceled, false)
		if e.RefundHTTP(200, context.Canceled) {
			t.Fatal("prior rejection made an unknown new attempt free")
		}
	})
	t.Run("200 is not success", func(t *testing.T) {
		e := &UserModelRequestEvidence{}
		e.Observe(&OpenAIForwardResult{OpenAIWSMode: true, UpstreamTerminalEvent: "response.failed"}, nil, true)
		if !e.RefundHTTP(200, nil) {
			t.Fatal("empty failed terminal charged as HTTP success")
		}
	})
}

// Run only against the task's isolated database. Production DSNs are rejected.
func TestUserModelRequestQuotaPostgres(t *testing.T) {
	dsn := os.Getenv("SUB2API_MODEL_QUOTA_TEST_DSN")
	if dsn == "" {
		t.Skip("set SUB2API_MODEL_QUOTA_TEST_DSN to an isolated sub2api_modelquota_test database")
	}
	adminDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Close()
	var database string
	if err = adminDB.QueryRow("SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if database != "sub2api_modelquota_test" {
		t.Fatal("refusing to test outside dedicated database")
	}
	schema := fmt.Sprintf("quota_%d", time.Now().UnixNano())
	if _, err = adminDB.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	defer adminDB.Exec("DROP SCHEMA " + schema + " CASCADE")
	if strings.Contains(dsn, "://") {
		t.Fatal("use PostgreSQL keyword DSN for isolated search_path")
	}
	db, err := sql.Open("postgres", dsn+" search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(12)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE users(id BIGINT PRIMARY KEY,status TEXT NOT NULL DEFAULT 'active',deleted_at TIMESTAMPTZ);
	 CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT,updated_at TIMESTAMPTZ);
	 CREATE TABLE groups(id BIGINT PRIMARY KEY,model_allowlist JSONB NOT NULL,updated_at TIMESTAMPTZ,deleted_at TIMESTAMPTZ);
	 INSERT INTO groups(id,model_allowlist) VALUES(7,'{"enabled":true,"models":["legacy-only"]}');
	 INSERT INTO users(id) VALUES(1),(2),(3)`)
	migration, err := os.ReadFile("../../migrations/237_user_model_request_policies.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(migration))
	exec(string(migration))
	s := NewUserModelPolicyService(db)
	ctx := context.Background()
	limited := func(model string, n int64) UserModelRequestRule {
		return UserModelRequestRule{Model: model, Mode: "limited", RequestLimit: n, WindowHours: 6}
	}
	if r, err := s.Reserve(ctx, 1, "anything"); err != nil || r != nil {
		t.Fatalf("legacy mode changed: %v", err)
	}
	if err := s.Save(ctx, 1, 0, []UserModelRequestRule{limited("gpt-5.6-sol", 3), limited("gpt-5.6-terra", 2)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, 2, 0, []UserModelRequestRule{limited("gpt-5.6-sol", 2)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(ctx); err != nil {
		t.Fatalf("unconfigured users must not prevent activation: %v", err)
	}
	var legacyEnabled bool
	if err := db.QueryRow(`SELECT (model_allowlist->>'enabled')::boolean FROM groups WHERE id=7`).Scan(&legacyEnabled); err != nil || legacyEnabled {
		t.Fatalf("group allowlist was not disabled: %v", err)
	}
	if r, err := s.Reserve(ctx, 3, "gpt-5.6-sol"); err != nil || r != nil {
		t.Fatalf("missing policy must be unrestricted: %v", err)
	}
	if err := s.Save(ctx, 3, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Reserve(ctx, 3, "gpt-5.6-sol"); err != nil || r != nil {
		t.Fatalf("empty policy must be unrestricted: %v", err)
	}
	if r, err := s.Reserve(ctx, 1, "gpt-5.6-luna"); err != nil || r != nil {
		t.Fatalf("another unconfigured model must remain unrestricted: %v", err)
	}
	if err := s.Save(ctx, 1, 0, nil); infraerrors.Reason(err) != "MODEL_POLICY_CHANGED" {
		t.Fatalf("stale save accepted: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var accepted []*UserModelRequestReservation
	var failures []error
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := "gpt-5.6-sol"
			if i%2 == 0 {
				model = "gpt-5.6-high"
			}
			r, err := s.Reserve(ctx, 1, model)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				accepted = append(accepted, r)
			} else {
				var q *ModelRequestQuotaError
				if !errors.As(err, &q) {
					failures = append(failures, err)
				}
			}
		}(i)
	}
	wg.Wait()
	if len(failures) > 0 || len(accepted) != 3 {
		t.Fatalf("atomic cap: accepted=%d errors=%v", len(accepted), failures)
	}
	p, err := s.Status(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Windows) != 1 || p.Windows[0].Used != 3 {
		t.Fatalf("incorrect count: %+v", p.Windows)
	}
	resetAt := *p.Windows[0].ResetsAt
	for i := 0; i < 3; i++ {
		if _, err := s.Reserve(ctx, 1, "gpt-5.6"); err == nil {
			t.Fatal("cap bypassed")
		}
	}
	p, _ = s.Status(ctx, 1)
	if !p.Windows[0].ResetsAt.Equal(resetAt) {
		t.Fatal("denied retry extended the window")
	}
	if r, err := s.Reserve(ctx, 1, "gpt-5.6-terra"); err != nil {
		t.Fatal("other model blocked", err)
	} else {
		r.Finish(false)
	}
	if r, err := NewUserModelPolicyService(db).Reserve(ctx, 2, "gpt-5.6-sol"); err != nil {
		t.Fatal("other user blocked", err)
	} else {
		r.Finish(false)
	}
	accepted[0].Finish(true)
	accepted[0].Finish(true)
	p, _ = s.Status(ctx, 1)
	if p.Windows[0].Used != 2 {
		t.Fatal("refund not idempotent")
	}
	if err := s.Reset(ctx, 1, "gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Reserve(ctx, 1, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	r.Finish(false)
	accepted[1].Finish(true)
	p, _ = s.Status(ctx, 1)
	if p.Windows[0].Used != 1 {
		t.Fatal("old refund changed reset window")
	}
	exec(`UPDATE user_model_request_windows SET resets_at=NOW()-INTERVAL '1 second' WHERE user_id=1 AND model='gpt-5.6-sol'`)
	r, err = s.Reserve(ctx, 1, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	r.Finish(false)
	p, _ = s.Status(ctx, 1)
	if p.Windows[0].Used != 1 || !p.Windows[0].ResetsAt.After(resetAt) {
		t.Fatal("expired window failed to restart")
	}

	// A relay retry reuses one reservation, a new client turn spends another.
	if err := s.Reset(ctx, 2, "gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	tracker := NewUserModelRequestWSTracker(s, 2)
	if err := tracker.Begin(ctx, 1, "gpt-5.6"); err != nil {
		t.Fatal(err)
	}
	tracker.After(1, nil, &UpstreamFailoverError{StatusCode: 503})
	if err := tracker.Begin(ctx, 1, "gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	tracker.After(1, &OpenAIForwardResult{OpenAIWSMode: true, UpstreamTerminalEvent: "response.completed"}, nil)
	p, _ = s.Status(ctx, 2)
	if p.Windows[0].Used != 1 {
		t.Fatal("WS internal retry counted twice")
	}
	if err := tracker.Begin(ctx, 2, "gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	tracker.After(2, &OpenAIForwardResult{Usage: OpenAIUsage{OutputTokens: 1}}, context.Canceled)
	if err := tracker.Begin(ctx, 3, "gpt-5.6-sol"); err == nil {
		t.Fatal("WS next-turn cap bypassed")
	}
	if err := s.Save(ctx, 2, 1, []UserModelRequestRule{{Model: "gpt-5.6-sol", Mode: "deny"}}); err != nil {
		t.Fatal(err)
	}
	if enabled, err := tracker.Check(ctx, []string{"gpt-5.6-sol"}); !enabled || err == nil {
		t.Fatal("existing WS missed revocation")
	}
	if enabled, err := tracker.Check(ctx, []string{"gpt-5.6-luna"}); !enabled || err != nil {
		t.Fatal("WS rejected an unconfigured model", err)
	}
	tracker.Close()

	// Daily windows use database time and Beijing midnight, not the host zone or
	// a 24-hour wait. Expiry and late refunds share the same fenced counter path.
	if err := s.Save(ctx, 3, 1, []UserModelRequestRule{{Model: "gpt-5.6-luna", Mode: "limited", RequestLimit: 1, WindowMode: "daily"}}); err != nil {
		t.Fatal(err)
	}
	dailySlot, err := s.Reserve(ctx, 3, "gpt-5.6-luna")
	if err != nil || dailySlot == nil {
		t.Fatalf("daily reservation failed: %v", err)
	}
	p, err = s.Status(ctx, 3)
	if err != nil || len(p.Windows) != 1 || p.Windows[0].Used != 1 || p.Windows[0].ResetsAt == nil {
		t.Fatalf("daily status failed: %+v %v", p, err)
	}
	beijing := p.Windows[0].ResetsAt.In(time.FixedZone("Beijing", 8*3600))
	if beijing.Hour() != 0 || beijing.Minute() != 0 || beijing.Second() != 0 || time.Until(beijing) > 24*time.Hour {
		t.Fatalf("daily reset is not the next Beijing midnight: %v", beijing)
	}
	_, err = s.Reserve(ctx, 3, "gpt-5.6-luna-high")
	var dailyCap *ModelRequestQuotaError
	if !errors.As(err, &dailyCap) || !dailyCap.ResetsAt.Equal(beijing) || dailyCap.RetryAfter < 1 || dailyCap.RetryAfter > 86400 {
		t.Fatalf("daily cap/reset time failed: %v", err)
	}
	exec(`UPDATE user_model_request_windows SET resets_at=NOW()-INTERVAL '1 second' WHERE user_id=3`)
	nextDay, err := s.Reserve(ctx, 3, "gpt-5.6-luna")
	if err != nil {
		t.Fatal("daily expiry did not admit next request", err)
	}
	nextDay.Finish(false)
	dailySlot.Finish(true)
	p, err = s.Status(ctx, 3)
	if err != nil || p.Windows[0].Used != 1 || !p.Windows[0].ResetsAt.Equal(beijing) {
		t.Fatalf("daily rollover/refund fencing failed: %+v %v", p, err)
	}
	// Shadow only this test connection's statement clock, exercising the actual
	// reservation SQL immediately before and exactly at Beijing midnight.
	exec(`INSERT INTO users(id) VALUES(4);
	 CREATE TABLE quota_clock(instant TIMESTAMPTZ);
	 INSERT INTO quota_clock VALUES('2026-09-08T15:59:59Z');
	 CREATE FUNCTION statement_timestamp() RETURNS TIMESTAMPTZ LANGUAGE SQL STABLE AS 'SELECT instant FROM quota_clock'`)
	clockDB, err := sql.Open("postgres", dsn+" search_path="+schema+",pg_catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer clockDB.Close()
	clockService := NewUserModelPolicyService(clockDB)
	if err := clockService.Save(ctx, 4, 0, []UserModelRequestRule{{Model: "gpt-5.6-luna", Mode: "limited", RequestLimit: 1, WindowMode: "daily"}}); err != nil {
		t.Fatal(err)
	}
	beforeMidnight, err := clockService.Reserve(ctx, 4, "gpt-5.6-luna")
	if err != nil {
		t.Fatal(err)
	}
	_, err = clockService.Reserve(ctx, 4, "gpt-5.6-luna")
	if !errors.As(err, &dailyCap) || dailyCap.RetryAfter != 1 || dailyCap.ResetsAt.UTC().Format(time.RFC3339) != "2026-09-08T16:00:00Z" {
		t.Fatalf("23:59:59 must wait only one second: %v", err)
	}
	exec(`UPDATE quota_clock SET instant='2026-09-08T16:00:00Z'`)
	atMidnight, err := clockService.Reserve(ctx, 4, "gpt-5.6-luna")
	if err != nil {
		t.Fatal("00:00 must start the next calendar day", err)
	}
	atMidnight.Finish(false)
	beforeMidnight.Finish(true)
	var used int64
	var nextReset time.Time
	if err := db.QueryRow(`SELECT used,resets_at FROM user_model_request_windows WHERE user_id=4`).Scan(&used, &nextReset); err != nil || used != 1 || nextReset.UTC().Format(time.RFC3339) != "2026-09-09T16:00:00Z" {
		t.Fatalf("midnight counter/refund/reset failed: %d %v %v", used, nextReset, err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Load(cancelCtx, 1); infraerrors.Code(err) != 503 {
		t.Fatalf("DB failure did not fail closed: %v", err)
	}
	exec(`UPDATE user_model_request_policies SET rules='[{"model":"gpt-5.6-sol","mode":"inherit"}]' WHERE user_id=1`)
	if _, err := s.Load(ctx, 1); infraerrors.Code(err) != 503 {
		t.Fatal("corrupt policy failed open")
	}
	exec(`UPDATE settings SET value='invalid' WHERE key='user_model_request_policy_enabled'`)
	if _, err := s.Load(ctx, 2); infraerrors.Code(err) != 503 {
		t.Fatal("invalid authority mode fell back to groups")
	}
}
