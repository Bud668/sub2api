package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func updateDynamicGuardPool(t *testing.T, db *sql.DB, id int64, change func(*DynamicQuotaPoolState)) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	p, err := lockDynamicPool(context.Background(), tx, id)
	require.NoError(t, err)
	change(p)
	require.NoError(t, writeDynamicPool(context.Background(), tx, id, p))
	require.NoError(t, tx.Commit())
}

func TestDynamicQuotaFrozenTrustedBudgetAdmission(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	dynamicTestSave(t, s, 12, 4, true)
	dynamicExec(t, db, `UPDATE dynamic_subscription_policies SET applied_limit_usd=100,allocated_standard_usd=100 WHERE subscription_id=11;
 UPDATE dynamic_subscription_policies SET applied_limit_usd=20,allocated_standard_usd=20 WHERE subscription_id=12;
 UPDATE accounts SET extra='{"auto_pause_7d_threshold":0.98}' WHERE id=4`)
	now := time.Now().UTC()
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		o := dynamicTestObservation(4, 40, now.Add(6*24*time.Hour), now.Add(-time.Hour))
		*p = DynamicQuotaPoolState{Cycle: 1, StartedAt: now.Add(-24 * time.Hour), Status: "active", CapacityUSD: 2000,
			Snapshot: &o, Health: dynamicQuotaHealth{Failures: 8},
			V2: &DynamicQuotaV2State{UnreservedCapacity: true, CandidateUSD: 9000, CandidateSamples: 1}}
	})
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.True(t, q.GrowthFrozen)
	require.Equal(t, "sync_recovery", q.GrowthFrozenReason)
	require.Equal(t, "active", q.Status)
	require.Equal(t, 80.0, q.RemainingUSD)
	allowed, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err, "persistent anomalies cannot block a trusted unspent allowance")
	_, err = s.Begin(ctx, 102, 4)
	require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
	off, err := s.Begin(ctx, 103, 4)
	require.NoError(t, err, "OFF traffic uses the same physical source budget without taking a member share")
	// 2000 * (98%-40%) = 1160. Later source consumption and the two
	// 0.20 holds exhaust that budget; the unapproved 9000 never grants more.
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET standard_total_usd=1159.6 WHERE account_id=4`)
	for _, key := range []int64{101, 103} {
		_, err = s.Begin(ctx, key, 4)
		require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
	}
	dynamicTestSettle(t, db, allowed, 101, 11, 0.2, 0.2)
	dynamicTestSettle(t, db, off, 103, 13, 0.2, 0.2)
	var total float64
	require.NoError(t, db.QueryRow(`SELECT standard_total_usd FROM dynamic_quota_pools WHERE account_id=4`).Scan(&total))
	require.InDelta(t, 1160, total, 1e-8)
	_, err = s.Begin(ctx, 101, 4)
	require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
	// Native thresholds, changed credentials and expired windows remain gates.
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET standard_total_usd=0 WHERE account_id=4`)
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) { p.Snapshot.UsedPercent = 98 })
	_, err = s.Begin(ctx, 101, 4)
	require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) { p.Snapshot.UsedPercent = 40 })
	dynamicExec(t, db, `UPDATE accounts SET credentials='{"chatgpt_account_id":"replacement"}' WHERE id=4`)
	_, err = s.Begin(ctx, 101, 4)
	require.ErrorIs(t, err, ErrDynamicQuotaBinding)
	dynamicExec(t, db, `UPDATE accounts SET credentials='{"chatgpt_account_id":"4"}' WHERE id=4`)
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) { p.Snapshot.ResetAt = now.Add(-time.Minute) })
	_, err = s.Begin(ctx, 101, 4)
	require.ErrorIs(t, err, ErrDynamicQuotaUnavailable)
	after, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, q.UsedUSD+0.2, after.UsedUSD)
	require.Equal(t, q.Cycle, after.Cycle)
	require.Equal(t, q.StartedAt, after.StartedAt)
}

func TestDynamicQuotaInitialEstimateUsesCumulativeWindow(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicExec(t, db, `INSERT INTO dynamic_quota_pools(account_id) VALUES(4);
 INSERT INTO usage_logs(account_id,total_cost) VALUES(4,1000)`)
	now := time.Now().UTC().Add(time.Second)
	reset := now.Add(6 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		observed := now.Add(time.Duration(i-4) * 30 * time.Second)
		s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
			return dynamicTestObservation(id, 50, reset, observed), nil
		}
		// Include synthetic history in the first observation's actual window.
		dynamicExec(t, db, `UPDATE usage_logs SET created_at=$1`, now.Add(-time.Hour))
		require.NoError(t, s.Refresh(ctx, 4))
	}
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		require.InDelta(t, 2000, p.CapacityUSD, 1e-8)
		require.Zero(t, p.V2.CandidateSamples, "a cumulative estimate needs no independent learning intervals")
	})
}

func TestDynamicQuotaRefreshPersistsInvalidAndCanceledQueries(t *testing.T) {
	s, db := dynamicTestStore(t)
	dynamicTestSave(t, s, 11, 4, true)
	for _, invalid := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
			p.Snapshot.FetchedAt = time.Now().UTC().Add(-time.Minute)
			p.Health = dynamicQuotaHealth{}
		})
		s.fetch = func(context.Context, int64) (DynamicQuotaObservation, error) {
			if invalid {
				return DynamicQuotaObservation{}, nil
			}
			cancel()
			return DynamicQuotaObservation{}, context.Canceled
		}
		require.ErrorIs(t, s.Refresh(ctx, 4), ErrDynamicQuotaUnavailable)
		cancel()
		q, err := s.Load(context.Background(), 11)
		require.NoError(t, err)
		require.Equal(t, 1, q.pool.Health.Failures)
		require.True(t, q.GrowthFrozen)
		require.Equal(t, "sync_recovery", q.GrowthFrozenReason)
		require.Equal(t, 20.0, q.UsedUSD)
	}
}

func TestDynamicQuotaStaleRecoveryAndNotificationSurviveRestart(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	now := time.Now().UTC()
	reset := now.Add(6 * 24 * time.Hour)
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		o := dynamicTestObservation(4, 50, reset, now.Add(-13*time.Minute))
		p.Snapshot = &o
		p.Health = dynamicQuotaHealth{Failures: 1, LastAttemptAt: o.FetchedAt}
		p.GuardSignal = "warning"
	})
	require.NoError(t, s.recordRefreshFailure(ctx, 4, now.Add(-2*time.Minute)))
	require.NoError(t, s.recordRefreshFailure(ctx, 4, now.Add(-110*time.Second)))
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 2, q.pool.Health.Failures, "test the below-threshold failure count after a long stale gap")
	var freezes int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE kind='quota_guard_frozen'`).Scan(&freezes))
	require.Equal(t, 1, freezes, "clock expiry must create a freeze event once, not zero or one per poll")
	s = NewDynamicSubscriptionService(db, dynamicTestAccounts{}, nil, nil)
	for i := 0; i < 3; i++ {
		observed := now.Add(time.Duration(i-2) * 30 * time.Second)
		s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
			return dynamicTestObservation(id, 50, reset, observed), nil
		}
		require.NoError(t, s.Refresh(ctx, 4))
		q, err = s.Load(ctx, 11)
		require.NoError(t, err)
		require.Equal(t, "learning", q.Status)
		require.Equal(t, i < 2, q.GrowthFrozen)
		allowed, err := s.Begin(ctx, 101, 4)
		require.NoError(t, err, "recovery queries gate growth, not the trusted remaining budget")
		allowed.RejectBeforeForward()
	}
	var recovered int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE kind='quota_guard_recovered'`).Scan(&recovered))
	require.Equal(t, 1, recovered)
	require.Equal(t, 20.0, q.UsedUSD)
	require.Equal(t, int64(1), q.Cycle)
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		p.Snapshot.FetchedAt = now.Add(-11 * time.Minute)
		p.Health = dynamicQuotaHealth{Failures: 3, Recoveries: 2, LastAttemptAt: p.Snapshot.FetchedAt, LastRecoveryAt: p.Snapshot.FetchedAt}
		p.GuardSignal = "frozen"
	})
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, "learning", q.Status)
	require.True(t, q.GrowthFrozen, "two recovery samples from before another stale gap do not authorize growth")
	require.Equal(t, 1, q.pool.Health.Recoveries)
}

func TestDynamicQuotaFailureFreezeAndRecoveryKeepsBilling(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	now := time.Now().UTC()
	reset := now.Add(6 * 24 * time.Hour)
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		o := dynamicTestObservation(4, 50, reset, now.Add(-5*time.Minute))
		p.Snapshot, p.Health = &o, dynamicQuotaHealth{}
	})
	before, err := s.Load(ctx, 11)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		require.NoError(t, s.recordRefreshFailure(ctx, 4, now.Add(time.Duration(i-4)*time.Minute)))
	}
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, "learning", q.Status)
	require.True(t, q.GrowthFrozen)
	require.Equal(t, before.LimitUSD, q.LimitUSD)
	allowed, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	allowed.RejectBeforeForward()
	dynamicTestSettle(t, db, r, 101, 11, 1, 1)
	for i := 0; i < 3; i++ {
		fetched := now.Add(time.Duration(i-2) * 30 * time.Second)
		s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
			return dynamicTestObservation(id, 50, reset, fetched), nil
		}
		require.NoError(t, s.Refresh(ctx, 4))
		q, err = s.Load(ctx, 11)
		require.NoError(t, err)
		require.Equal(t, "active", q.Status, "one valid cumulative window is sufficient")
		require.Equal(t, i < 2, q.GrowthFrozen)
	}
	require.Equal(t, before.UsedUSD+1, q.UsedUSD)
	require.Equal(t, before.Cycle, q.Cycle)
	var settled int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_requests WHERE id=$1 AND status='settled'`, r.ID).Scan(&settled))
	require.Equal(t, 1, settled)
	// A late failed response cannot pause a source whose newer query succeeded.
	require.NoError(t, s.recordRefreshFailure(ctx, 4, now.Add(-time.Minute)))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.False(t, q.GrowthFrozen)
	require.Empty(t, q.GrowthFrozenReason)
}
