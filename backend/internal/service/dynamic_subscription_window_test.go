package service

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaWindowReplacesOffsetLearningAndKeepsThreshold(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	dynamicExec(t, db, `INSERT INTO users(id) VALUES(4);
 INSERT INTO user_subscriptions(id,user_id,group_id,weekly_usage_usd) VALUES(14,4,7,180);
 UPDATE user_subscriptions SET status='active',weekly_usage_usd=180 WHERE id IN (11,12,13);
 UPDATE dynamic_subscription_policies SET used_standard_usd=180,cycle_used_usd=180 WHERE subscription_id=11;
 INSERT INTO usage_logs(account_id,subscription_id,total_cost) SELECT 4,n,180 FROM generate_series(11,14) n;
 UPDATE dynamic_quota_pools SET standard_total_usd=720 WHERE account_id=4;
 UPDATE accounts SET extra='{"auto_pause_7d_threshold":0.99}' WHERE id=4;`)
	var reset time.Time
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		p.Status, p.CapacityUSD = "active", 1571
		p.Snapshot.UsedPercent, p.Snapshot.LocalStandardTotal = 37, 666
		p.Snapshot.FetchedAt = time.Now().UTC().Add(-time.Minute)
		p.V2.LastNode, p.V2.CumulativeEstimate = 30, false // Real pre-upgrade shape.
		p.LastAllocationAt = p.Snapshot.FetchedAt
		reset = p.Snapshot.ResetAt
	})
	percent := 40.0
	s.fetch = func(context.Context, int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(4, percent, reset, time.Now().UTC()), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	for _, id := range []int64{11, 12, 13, 14} {
		q, err := s.Load(ctx, id)
		require.NoError(t, err)
		require.InDelta(t, 1800*.99/4, q.LimitUSD, 1e-7, "40% allocates now, not at 47%; threshold excluded exactly once")
		require.Equal(t, 180.0, q.UsedUSD)
		require.Zero(t, q.PendingAdjustmentPercent)
		require.Equal(t, 45, q.NextAdjustmentPercent)
	}
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	at := *q.LastAllocationAt
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE account_id=4 AND kind='allocation_node'`).Scan(&count))
	for range 3 {
		require.NoError(t, s.Refresh(ctx, 4))
	}
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, at, *q.LastAllocationAt)
	var again int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE account_id=4 AND kind='allocation_node'`).Scan(&again))
	require.Equal(t, count, again, "repeated snapshots do not regrant")

	// A jump crosses 45/50/55 but produces one cumulative allocation.
	dynamicExec(t, db, `UPDATE usage_logs SET total_cost=252 WHERE account_id=4;
 UPDATE dynamic_subscription_policies SET used_standard_usd=252,cycle_used_usd=252 WHERE account_id=4;
 UPDATE user_subscriptions SET weekly_usage_usd=252 WHERE id BETWEEN 11 AND 14;
 UPDATE dynamic_quota_pools SET standard_total_usd=1008 WHERE account_id=4;`)
	percent = 56
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 445.5, q.LimitUSD, 1e-7)
	require.Equal(t, 252.0, q.UsedUSD)
	require.Equal(t, 60, q.NextAdjustmentPercent)
	require.Equal(t, 55, q.pool.V2.LastNode)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE account_id=4 AND kind='allocation_node'`).Scan(&again))
	require.Equal(t, count+1, again)

	// Account-native threshold changes protect the very next admission.
	dynamicExec(t, db, `UPDATE accounts SET extra='{"auto_pause_7d_threshold":0.90}' WHERE id=4;`)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	r.RejectBeforeForward()
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 1800*.9/4, q.LimitUSD, 1e-7)
	require.Equal(t, 252.0, q.UsedUSD)
}

func TestDynamicQuotaWindowUnitsAndAbnormalEstimate(t *testing.T) {
	now := time.Now().UTC()
	o := dynamicTestObservation(4, 40, now.Add(time.Hour), now)
	// Same upstream usage: account display uses x2; pool ledger stays standard.
	o.WindowCostUSD, o.WindowStandardUSD = 1440, 720
	p := DynamicQuotaPoolState{ceilingPercent: 95}
	p.Observe(o, now)
	require.InDelta(t, 3600, *usagestats.EstimateWindowTotalCost(o.WindowCostUSD, 40), 1e-8)
	require.InDelta(t, 1800, p.CapacityUSD, 1e-8)
	require.InDelta(t, 990, p.Available(now, 0, 0), 1e-8)
	// Identical inflated observations cannot confirm themselves.
	o.WindowCostUSD, o.WindowStandardUSD = 7200, 3600
	for i := 1; i <= 5; i++ {
		o.FetchedAt = now.Add(time.Duration(i) * time.Minute)
		p.Observe(o, o.FetchedAt)
		require.Equal(t, 1800.0, p.CapacityUSD)
		require.Equal(t, 1, p.V2.CandidateSamples)
		require.False(t, p.v2AllocationDue(o.FetchedAt))
	}
	// Normal data clears the anomaly immediately without an independent learner.
	o.FetchedAt = o.FetchedAt.Add(time.Minute)
	o.WindowCostUSD, o.WindowStandardUSD = 1440, 720
	p.Observe(o, o.FetchedAt)
	require.Zero(t, p.V2.CandidateSamples)
	require.True(t, p.v2AllocationDue(o.FetchedAt))
	// A newer percentage without local window cost is not expansion evidence.
	o.FetchedAt = o.FetchedAt.Add(time.Minute)
	o.UsedPercent, o.WindowCostUSD, o.WindowStandardUSD = 45, 0, 0
	p.Observe(o, o.FetchedAt)
	require.Equal(t, 1800.0, p.CapacityUSD)
	require.False(t, p.v2AllocationDue(o.FetchedAt))
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	for _, obsolete := range []string{`"samples"`, `"sample_anchor"`, `"sample_at"`} {
		require.NotContains(t, string(raw), obsolete)
	}
}

func TestDynamicQuotaClosedHistoryDoesNotHoldOrChangeGrant(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	fixedSeatEvidence(t, s, 1800, 40, true)
	before, err := s.Load(ctx, 11)
	require.NoError(t, err)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET status='uncertain',hold_standard_usd=999,
 operator_absorbed_at=NOW(),finished_at=NOW()-INTERVAL '10 minutes' WHERE id=$1`, r.ID)
	fixedSeatEvidence(t, s, 1800, 40, false)
	after, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, before.LimitUSD, after.LimitUSD)
	require.Equal(t, before.RemainingUSD, after.RemainingUSD)
	_, held, _, pending, err := dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	require.Zero(t, held)
	require.Zero(t, pending)
	var audit int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_requests WHERE id=$1 AND operator_absorbed_at IS NOT NULL`, r.ID).Scan(&audit))
	require.Equal(t, 1, audit)
	// A real bill not yet committed retains exactly one claim, not a new grant.
	live, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET finished_at=NOW(),outcome='usage_received' WHERE id=$1`, live.ID)
	_, held, _, pending, err = dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	require.Equal(t, 1, pending)
	require.InDelta(t, .18, held, 1e-8)
	fixedSeatEvidence(t, s, 1800, 40, false)
	after, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, before.LimitUSD, after.LimitUSD)
	require.InDelta(t, before.RemainingUSD-.18, after.RemainingUSD, 1e-8)
	dynamicTestSettle(t, db, live, 101, 11, .04, .04)
	_, held, _, pending, err = dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	require.Zero(t, held)
	require.Zero(t, pending)
	after, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, .04, after.UsedUSD, "charge actual cost, never the .18 reservation")
}

func TestDynamicQuotaFallingWindowEstimateDoesNotCreateAPersonalFloor(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	dynamicExec(t, db, `UPDATE accounts SET extra='{"auto_pause_7d_threshold":0.99}' WHERE id=4;
 UPDATE dynamic_subscription_policies SET floor_limit_usd=400 WHERE subscription_id=11;`)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 60.0, q.LimitUSD, "600 * 10% is initial credit, not a 400/540 floor")
	now := time.Now().UTC()
	var reset time.Time
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		p.Snapshot.FetchedAt = now.Add(-6 * time.Minute)
		reset = p.Snapshot.ResetAt
	})
	for i, spent := range []float64{48, 60, 90, 120} {
		// Cumulative cost remains monotonic: 2% -> 2400; 4/6/8% -> 1500.
		// An abnormal drop is confirmed without changing the 600 personal cap.
		dynamicExec(t, db, `UPDATE dynamic_subscription_policies SET used_standard_usd=$1,cycle_used_usd=$1 WHERE subscription_id=11`, spent)
		dynamicExec(t, db, `UPDATE user_subscriptions SET weekly_usage_usd=$1 WHERE id=11`, spent)
		dynamicExec(t, db, `UPDATE dynamic_quota_pools SET standard_total_usd=$1 WHERE account_id=4`, spent)
		dynamicExec(t, db, `INSERT INTO usage_logs(account_id,subscription_id,total_cost)
 SELECT 4,11,$1-COALESCE(sum(total_cost),0) FROM usage_logs WHERE account_id=4`, spent)
		at := now.Add(time.Duration(i-5) * time.Minute)
		s.fetch = func(context.Context, int64) (DynamicQuotaObservation, error) {
			return dynamicTestObservation(4, float64((i+1)*2), reset, at), nil
		}
		require.NoError(t, s.Refresh(ctx, 4))
		q, err = s.Load(ctx, 11)
		require.NoError(t, err)
		require.Equal(t, 600.0, q.MaxLimitUSD)
		require.Equal(t, spent, q.UsedUSD)
		if i == 0 {
			require.InDelta(t, 594, q.LimitUSD, 1e-7)
		}
	}
	require.InDelta(t, 1500, q.CapacityEstimateUSD, 1e-7)
	require.InDelta(t, 371.25, q.LimitUSD, 1e-7, "an obsolete saved 400 floor is ignored for fixed seats")
	require.InDelta(t, 251.25, q.RemainingUSD, 1e-7)
	require.Zero(t, q.PendingAdjustmentPercent)
}

func TestDynamicQuotaNewCycleDoesNotInheritLastCycleCapacity(t *testing.T) {
	for _, capacity := range []float64{2400, 1500, 1000, 3000} {
		now := time.Now().UTC()
		old := dynamicTestObservation(4, 50, now.Add(time.Hour), now.Add(-2*time.Minute))
		old.WindowCostUSD, old.WindowStandardUSD = 1200, 1200
		p := DynamicQuotaPoolState{ceilingPercent: 99}
		p.Observe(old, old.FetchedAt)
		require.Equal(t, 2400.0, p.CapacityUSD)
		next := dynamicTestObservation(4, 2, now.Add(7*24*time.Hour), now)
		next.WindowCostUSD, next.WindowStandardUSD = capacity*.02, capacity*.02
		require.False(t, p.Observe(next, now), "the first reset signal must not reset a cycle")
		require.Equal(t, 2400.0, p.CapacityUSD)
		next.FetchedAt = now.Add(30 * time.Second)
		require.True(t, p.Observe(next, next.FetchedAt))
		p.Confirm(next.FetchedAt)
		require.InDelta(t, capacity, p.CapacityUSD, 1e-8)
		require.Zero(t, p.V2.CandidateSamples, "a new cycle is not compared against last cycle capacity")
		members := []dynamicQuotaV2Member{{ID: 1, Weight: 1, Used: capacity * .02, Cap: 600}, {ID: 2, Weight: 1, Cap: 600}, {ID: 3, Weight: 1, Cap: 600}, {ID: 4, Weight: 1, Cap: 600}}
		grants, err := allocateDynamicQuotaV2(members, p.Available(next.FetchedAt, 0, 0))
		require.NoError(t, err)
		for _, limit := range grants {
			require.InDelta(t, math.Min(600, capacity*.99/4), limit, 1e-8)
		}
		if capacity == 3000 {
			var distributed float64
			for _, limit := range grants {
				distributed += limit
			}
			require.InDelta(t, 2400, distributed, 1e-8, "surplus stays with the site, never exceeds personal caps")
		}
	}
}
