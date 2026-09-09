package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaCapacityGuard(t *testing.T) {
	now := time.Now().UTC()
	p := DynamicQuotaPoolState{Cycle: 1, CapacityUSD: 2000, Status: "active",
		Snapshot: &DynamicQuotaObservation{Identity: "a", UsedPercent: 40, ResetAt: now.Add(time.Hour), WindowSeconds: 604800, FetchedAt: now}}
	p.considerCapacity(2100, now)
	require.Equal(t, 2000.0, p.CapacityUSD)
	p.considerCapacity(2100, now.Add(time.Second))
	require.Equal(t, 1, p.CapacityReview.Observations, "rapid repeated queries are not independent confirmations")
	p.considerCapacity(2100, now.Add(time.Minute))
	p.considerCapacity(2100, now.Add(2*time.Minute))
	require.Equal(t, 2100.0, p.CapacityUSD)
	require.Nil(t, p.CapacityReview)
	p.considerCapacity(9000, now.Add(3*time.Minute))
	initialID := p.CapacityReview.ID
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &p))
	require.Equal(t, initialID, p.CapacityReview.ID, "restart cannot authorize or lose a pending review")
	p.considerCapacity(9000, now.Add(4*time.Minute))
	p.considerCapacity(9000, now.Add(5*time.Minute))
	require.Equal(t, 2100.0, p.CapacityUSD)
	require.True(t, p.CapacityReview.ManualRequired)
	require.Equal(t, "active", p.accessStatus(now.Add(5*time.Minute)))
	require.True(t, p.growthFrozen(now))
	p.considerCapacity(10000, now.Add(6*time.Minute))
	require.NotEqual(t, initialID, p.CapacityReview.ID, "changed proposal invalidates the old approval")
	require.Equal(t, "active", p.accessStatus(now.Add(6*time.Minute)), "volatile spikes must not block the trusted budget")
	require.True(t, p.growthFrozen(now))
	p.considerCapacity(1500, now.Add(7*time.Minute))
	require.Equal(t, 1500.0, p.CapacityUSD, "downward safety changes do not wait")
	require.Nil(t, p.CapacityReview)
	require.Equal(t, "active", p.accessStatus(now.Add(7*time.Minute)))
	require.True(t, p.growthFrozen(now), "one recovered query must not grant increases")

	// A smaller positive proposal also needs recovery before granting increases.
	p.Health = dynamicQuotaHealth{}
	for i := 0; i < 3; i++ {
		p.considerCapacity(9000, now.Add(time.Duration(i+8)*time.Minute))
	}
	p.considerCapacity(1600, now.Add(11*time.Minute))
	require.Equal(t, dynamicQuotaGuardChecks, p.Health.Failures)
	require.True(t, p.growthFrozen(now))
	for i := 0; i < 3; i++ {
		at := now.Add(time.Duration(i+12) * time.Minute)
		p.recordHealthy(at)
		p.considerCapacity(1600, at)
		if i < 2 {
			require.Equal(t, 1500.0, p.CapacityUSD)
			require.True(t, p.growthFrozen(now))
		}
	}
	require.Equal(t, 1600.0, p.CapacityUSD)
	require.False(t, p.growthFrozen(now))
}

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

func TestDynamicQuotaSpikeCannotRestoreExhaustedUser(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	for _, member := range [][2]int64{{11, 4}, {12, 4}, {13, 4}, {21, 5}} {
		dynamicTestSave(t, s, member[0], member[1], true)
	}
	dynamicExec(t, db, `UPDATE dynamic_subscription_policies SET applied_limit_usd=20,allocated_standard_usd=20 WHERE account_id=4;
 UPDATE dynamic_subscription_policies SET applied_limit_usd=100 WHERE subscription_id=13;
 UPDATE dynamic_subscription_policies SET max_limit_usd=20 WHERE subscription_id=12;
 UPDATE dynamic_quota_pools SET standard_total_usd=300 WHERE account_id=4;
 UPDATE accounts SET extra='{"auto_pause_7d_threshold":0.98}' WHERE id=4`)
	now := time.Now().UTC()
	reset := now.Add(6 * 24 * time.Hour)
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		o := dynamicTestObservation(4, 40, reset, now.Add(-3*time.Minute))
		*p = DynamicQuotaPoolState{Cycle: 1, StartedAt: now.Add(-24 * time.Hour), Status: "active", CapacityUSD: 2000,
			Snapshot: &o, SampleAnchor: &o, LastAllocationAt: now.Add(-31 * time.Minute)}
	})
	var reviewID string
	for i := 0; i < 3; i++ {
		observedAt := now.Add(time.Duration(i-2) * time.Minute)
		s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
			return dynamicTestObservation(id, 43, reset, observedAt), nil
		}
		require.NoError(t, s.Refresh(ctx, 4))
		q, err := s.Load(ctx, 11)
		require.NoError(t, err)
		require.Equal(t, 2000.0, q.CapacityEstimateUSD)
		require.InDelta(t, 9000, q.CapacityReview.ProposedUSD, 1e-8)
		require.Equal(t, 20.0, q.LimitUSD)
		require.Equal(t, 20.0, q.UsedUSD)
		require.Zero(t, q.RemainingUSD)
		reviewID = q.CapacityReview.ID
		if i < 2 {
			require.ErrorIs(t, s.ApproveCapacity(ctx, 11, reviewID), ErrDynamicQuotaUnavailable)
		}
		_, err = s.Begin(ctx, 101, 4)
		require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
		require.Equal(t, "active", q.Status)
		standardExhausted, err := s.Load(ctx, 13)
		require.NoError(t, err)
		require.Zero(t, standardExhausted.RemainingUSD, "freeze must cover standard-cost shares, even with a larger displayed dollar limit")
	}
	// Saving a larger personal ceiling must not implicitly approve the source.
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.NoError(t, s.Save(ctx, 11, DynamicSubscriptionInput{Enabled: true, Revision: q.Revision, AccountID: 4, Weight: 1, MaxLimitUSD: 1000}))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 20.0, q.LimitUSD)
	require.Equal(t, reviewID, q.CapacityReview.ID)
	other, err := s.Begin(ctx, 201, 5)
	require.NoError(t, err)
	other.RejectBeforeForward()
	require.ErrorIs(t, s.ApproveCapacity(ctx, 21, reviewID), ErrDynamicQuotaChanged)
	require.ErrorIs(t, s.ApproveCapacity(ctx, 11, uuid.NewString()), ErrDynamicQuotaChanged)
	require.NoError(t, s.ApproveCapacity(ctx, 11, reviewID))
	require.ErrorIs(t, s.ApproveCapacity(ctx, 11, reviewID), ErrDynamicQuotaChanged, "approval cannot be replayed")
	after, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 9000, after.CapacityEstimateUSD, 1e-8)
	require.Equal(t, q.UsedUSD, after.UsedUSD)
	require.Equal(t, q.Cycle, after.Cycle)
	require.True(t, q.StartedAt.Equal(after.StartedAt))
	require.Greater(t, after.RemainingUSD, 0.0)
	_, err = s.Begin(ctx, 102, 4)
	require.ErrorIs(t, err, ErrDynamicQuotaExhausted, "manual capacity approval cannot bypass personal hard ceilings")
	var events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE account_id=4 AND kind='quota_capacity_approved'`).Scan(&events))
	require.Equal(t, 1, events)
}

func TestDynamicQuotaApprovalRequiresCurrentEvidence(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	now := time.Now().UTC()
	installReview := func() string {
		id := uuid.NewString()
		updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
			o := dynamicTestObservation(4, 50, now.Add(6*24*time.Hour), now)
			*p = DynamicQuotaPoolState{Cycle: 1, StartedAt: now, Status: "learning", Snapshot: &o,
				CapacityReview: &DynamicQuotaCapacityReview{ID: id, ProposedUSD: 2000, Observations: 3, ManualRequired: true, LastObservedAt: now}}
		})
		return id
	}
	id := installReview()
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) { p.Snapshot.FetchedAt = now.Add(-11 * time.Minute) })
	require.ErrorIs(t, s.ApproveCapacity(ctx, 11, id), ErrDynamicQuotaUnavailable)
	id = installReview()
	dynamicExec(t, db, `UPDATE accounts SET credentials='{"chatgpt_account_id":"replacement"}' WHERE id=4`)
	require.ErrorIs(t, s.ApproveCapacity(ctx, 11, id), ErrDynamicQuotaBinding)
	dynamicExec(t, db, `UPDATE accounts SET credentials='{"chatgpt_account_id":"4"}' WHERE id=4`)
	dynamicExec(t, db, `DELETE FROM account_groups WHERE account_id=4`)
	require.ErrorIs(t, s.ApproveCapacity(ctx, 11, id), ErrDynamicQuotaBinding)
	dynamicExec(t, db, `INSERT INTO account_groups VALUES(4,7)`)
	dynamicTestSave(t, s, 11, 4, false)
	require.ErrorIs(t, s.ApproveCapacity(ctx, 11, id), ErrDynamicQuotaBinding)
	dynamicTestSave(t, s, 11, 4, true)
	id = installReview()
	// A reset candidate invalidates the review even before any usage is reset.
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		o := dynamicTestObservation(4, 1, now.Add(7*24*time.Hour), now.Add(time.Second))
		require.False(t, p.Observe(o, o.FetchedAt))
		require.Nil(t, p.CapacityReview)
	})
	require.ErrorIs(t, s.ApproveCapacity(ctx, 11, id), ErrDynamicQuotaChanged)
	id = installReview()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = s.ApproveCapacity(ctx, 11, id) }(i)
	}
	wg.Wait()
	accepted, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrDynamicQuotaChanged) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	require.Equal(t, 1, accepted)
	require.Equal(t, 1, conflicts)
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
	reviewID := uuid.NewString()
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		o := dynamicTestObservation(4, 40, now.Add(6*24*time.Hour), now.Add(-time.Hour))
		*p = DynamicQuotaPoolState{Cycle: 1, StartedAt: now.Add(-24 * time.Hour), Status: "active", CapacityUSD: 2000,
			Snapshot: &o, SampleAnchor: &o, Health: dynamicQuotaHealth{Failures: 8},
			CapacityReview: &DynamicQuotaCapacityReview{ID: reviewID, ProposedUSD: 9000, Observations: 3,
				AnomalyChecks: 3, ManualRequired: true, LastObservedAt: o.FetchedAt}}
	})
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.True(t, q.GrowthFrozen)
	require.Equal(t, "active", q.Status)
	require.Equal(t, 80.0, q.RemainingUSD)
	require.ErrorIs(t, s.ApproveCapacity(ctx, 11, reviewID), ErrDynamicQuotaUnavailable)
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

func TestDynamicQuotaInitialEstimateNeverAutoApproves(t *testing.T) {
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
		require.Zero(t, p.CapacityUSD)
		require.Equal(t, 1600.0, p.CapacityReview.ProposedUSD)
		require.True(t, p.CapacityReview.ManualRequired)
		require.Equal(t, 5, p.CapacityReview.Observations)
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
		p.Snapshot, p.SampleAnchor = &o, &o
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
		p.Snapshot, p.SampleAnchor, p.Health = &o, &o, dynamicQuotaHealth{}
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
		require.Equal(t, "learning", q.Status)
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
}
