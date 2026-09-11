package service

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func saveDynamicV2(t *testing.T, s *DynamicSubscriptionService, id, source int64, enabled bool, cap, floor float64) {
	t.Helper()
	q, err := s.Load(context.Background(), id)
	require.NoError(t, err)
	var revision int64
	if q != nil {
		revision = q.Revision
	}
	require.NoError(t, s.Save(context.Background(), id, DynamicSubscriptionInput{Enabled: enabled, Revision: revision,
		AccountID: source, Weight: 1, MaxLimitUSD: cap, FloorLimitUSD: &floor}))
}

func TestDynamicQuotaV2SaveDoesNotWaitForNetworkOrOldRequests(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	old, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	networkCalls := 0
	s.fetch = func(context.Context, int64) (DynamicQuotaObservation, error) {
		networkCalls++
		return DynamicQuotaObservation{}, errors.New("offline")
	}
	saveDynamicV2(t, s, 11, 4, true, 600, 200)
	require.Zero(t, networkCalls)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.True(t, q.RequestedEnabled)
	require.True(t, q.ActivationPending)
	require.False(t, q.Enabled, "native rules remain effective until activation")
	require.Equal(t, 600.0, q.LimitUSD)
	require.Equal(t, 20.0, q.UsedUSD)
	during, err := s.Begin(ctx, 102, 4)
	require.NoError(t, err, "pending setup must not block unrelated native users")
	during.RejectBeforeForward()
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 63, time.Now().Add(6*24*time.Hour), time.Now().UTC()), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.True(t, q.Enabled)
	require.False(t, q.ActivationPending)
	require.Equal(t, 70, q.NextAdjustmentPercent)
	require.Equal(t, 600.0, q.LimitUSD, "initial allocation is the actual cap, not 80%")
	require.Equal(t, 0.01, q.ReservedUSD, "the pre-activation request remains reserved")
	dynamicTestSettle(t, db, old, 101, 11, 1, 1)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 21.0, q.UsedUSD)
	require.Equal(t, 21.0, q.usedStandard, "the fixed owner receives the late accounting exactly once")
	require.Zero(t, q.ReservedUSD)
	saveDynamicV2(t, s, 11, 4, false, 600, 200)
	dynamicExec(t, db, `UPDATE user_subscriptions SET weekly_usage_usd=0 WHERE id=11`)
	saveDynamicV2(t, s, 11, 4, true, 600, 200)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 21.0, q.UsedUSD, "off/on cannot use a native clock reset to refill the source cycle")
	require.Equal(t, 600.0, q.LimitUSD)
	cap := 300.0
	saveDynamicV2(t, s, 11, 4, true, cap, 200)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, cap, q.LimitUSD, "manual bounds constrain the same saved allowance")
	require.Equal(t, 21.0, q.UsedUSD)
}

func TestDynamicQuotaV2MigrationPreservesDisabledSettings(t *testing.T) {
	s, db := dynamicTestStore(t)
	dynamicExec(t, db, `ALTER TABLE dynamic_subscription_policies DROP COLUMN floor_limit_usd;
 ALTER TABLE dynamic_subscription_policies ADD COLUMN increase_threshold_usd NUMERIC DEFAULT 10;
 INSERT INTO dynamic_quota_pools(account_id) VALUES(4);
 INSERT INTO dynamic_subscription_policies(subscription_id,account_id,enabled,max_limit_usd,applied_limit_usd,used_standard_usd)
 VALUES(11,4,true,200,160,20)`)
	migration, err := os.ReadFile("../../migrations/240_dynamic_quota_v2.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.Error(t, err, "an active policy cannot migrate without explicit protection")
	dynamicExec(t, db, `UPDATE dynamic_subscription_policies SET enabled=false WHERE subscription_id=11`)
	for range 2 {
		dynamicExec(t, db, string(migration))
		q, err := s.Load(context.Background(), 11)
		require.NoError(t, err)
		require.False(t, q.Enabled)
		require.Nil(t, q.FloorLimitUSD)
		require.Equal(t, 160.0, q.LimitUSD)
		require.Equal(t, 20.0, q.UsedUSD)
	}
	_, err = db.Exec(`UPDATE dynamic_subscription_policies SET enabled=true WHERE subscription_id=11`)
	require.Error(t, err, "the database also prevents enabling incomplete settings")
	saveDynamicV2(t, s, 11, 4, true, 600, 200)
	q, err := s.Load(context.Background(), 11)
	require.NoError(t, err)
	require.Equal(t, 600.0, q.LimitUSD, "the first explicit V2 setup starts at the chosen cap")
	require.Equal(t, 20.0, q.UsedUSD)
	saveDynamicV2(t, s, 11, 4, false, 600, 200)
	saveDynamicV2(t, s, 11, 4, true, 600, 200)
	q, err = s.Load(context.Background(), 11)
	require.NoError(t, err)
	require.Equal(t, 600.0, q.LimitUSD)
	require.Equal(t, 20.0, q.UsedUSD)
}

func TestDynamicQuotaV2NodePublishesOnceAndResetIsSourceScoped(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	for _, pair := range [][2]int64{{11, 4}, {12, 4}, {21, 5}} {
		saveDynamicV2(t, s, pair[0], pair[1], true, 600, 100)
	}
	now := time.Now().UTC()
	reset := now.Add(6 * 24 * time.Hour)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 20, reset, now.Add(-2*time.Minute)), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	require.NoError(t, s.Refresh(ctx, 5))
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET standard_total_usd=100 WHERE account_id=4`)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 30, reset, now.Add(-time.Minute)), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	for _, id := range []int64{11, 12} {
		q, err := s.Load(ctx, id)
		require.NoError(t, err)
		require.InDelta(t, 335, q.LimitUSD, 1e-7)
		require.Equal(t, 20.0, q.UsedUSD)
		require.Equal(t, 40, q.NextAdjustmentPercent)
		require.Equal(t, "upstream_node", q.LastChange.Reason)
		require.Equal(t, 30, q.LastChange.Node)
		require.Equal(t, 600.0, q.LastChange.PreviousUSD)
	}
	require.NoError(t, s.Refresh(ctx, 4))
	var published int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE account_id=4 AND kind='allocation_node'`).Scan(&published))
	require.Equal(t, 1, published)
	otherBefore, err := s.Load(ctx, 21)
	require.NoError(t, err)
	newReset := now.Add(7 * 24 * time.Hour)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 0, newReset, now.Add(-30*time.Second)), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 0, newReset, now), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, int64(2), q.Cycle)
	require.Zero(t, q.UsedUSD)
	require.Equal(t, 600.0, q.LimitUSD)
	require.Equal(t, "reset", q.LastChange.Reason)
	otherAfter, err := s.Load(ctx, 21)
	require.NoError(t, err)
	require.Equal(t, otherBefore.Cycle, otherAfter.Cycle)
	require.Equal(t, otherBefore.UsedUSD, otherAfter.UsedUSD)
	require.Equal(t, otherBefore.LimitUSD, otherAfter.LimitUSD)
}

func TestDynamicQuotaV2FirstCapAndStablePhysicalShare(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicExec(t, db, `UPDATE user_subscriptions SET weekly_usage_usd=180 WHERE id=11`)
	saveDynamicV2(t, s, 11, 4, true, 600, 200)
	require.NoError(t, s.Refresh(ctx, 4))
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 600.0, q.LimitUSD)
	require.Equal(t, 180.0, q.UsedUSD)
	require.Equal(t, 420.0, q.RemainingUSD)
	changedAt := q.LastAllocationAt
	request, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicTestSettle(t, db, request, 101, 11, 10, 1) // Historical pricing is frozen, not recomputed.
	for _, enabled := range []bool{true, false, true} {
		saveDynamicV2(t, s, 11, 4, enabled, 600, 200)
	}
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 600.0, q.allocatedStandard, "save/toggle cannot recreate the spent standard share")
	require.Equal(t, 181.0, q.UsedUSD)
	require.Equal(t, changedAt, q.LastAllocationAt, "a data refresh is not an actual allowance change")
	saveDynamicV2(t, s, 11, 4, true, 100, 50)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 100.0, q.LimitUSD)
	require.Equal(t, 181.0, q.UsedUSD)
	require.Zero(t, q.RemainingUSD)
	require.Equal(t, "bounds", q.LastChange.Reason)
	for _, bad := range []*float64{nil, new(float64)} {
		err = s.Save(ctx, 11, DynamicSubscriptionInput{Enabled: true, AccountID: 4, Revision: q.Revision, Weight: 1, MaxLimitUSD: 100, FloorLimitUSD: bad})
		require.Error(t, err)
	}
}

func TestDynamicQuotaV2SpikeCannotRefillAnExhaustedUser(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	dynamicTestSave(t, s, 12, 4, true)
	dynamicExec(t, db, `UPDATE dynamic_subscription_policies SET applied_limit_usd=20,allocated_standard_usd=20 WHERE subscription_id=11;
 UPDATE dynamic_subscription_policies SET applied_limit_usd=100,allocated_standard_usd=100 WHERE subscription_id=12;
 UPDATE dynamic_quota_pools SET standard_total_usd=1000 WHERE account_id=4`)
	now := time.Now().UTC()
	reset := now.Add(6 * 24 * time.Hour)
	updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
		o := dynamicTestObservation(4, 10, reset, now.Add(-3*time.Minute))
		p.Snapshot, p.SampleAnchor = &o, &o
		p.startV2()
		p.CapacityUSD, p.Status = 1000, "active"
	})
	for i := 0; i < 3; i++ {
		at := now.Add(time.Duration(i-2) * time.Minute)
		s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
			return dynamicTestObservation(id, 20, reset, at), nil
		}
		require.NoError(t, s.Refresh(ctx, 4))
		q, err := s.Load(ctx, 11)
		require.NoError(t, err)
		require.Equal(t, 1000.0, q.CapacityEstimateUSD)
		require.InDelta(t, 9000, q.pool.V2.CandidateUSD, 1e-8)
		require.Equal(t, 1, q.pool.V2.CandidateSamples)
		require.Zero(t, q.RemainingUSD)
		_, err = s.Begin(ctx, 101, 4)
		require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
		allowed, err := s.Begin(ctx, 102, 4)
		require.NoError(t, err, "a trusted unspent allowance still works")
		allowed.RejectBeforeForward()
	}
	saveDynamicV2(t, s, 11, 4, true, 1000, 1)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 20.0, q.LimitUSD, "a larger cap is not a source capacity approval")
	require.Zero(t, q.RemainingUSD)
}
