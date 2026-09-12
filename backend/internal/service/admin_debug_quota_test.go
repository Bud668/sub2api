package service

import (
	"context"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaAdminDebugIndependentLimitAndConcurrency(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	require.NoError(t, s.ConvertToAdminDebug(ctx, 11, 1))
	dynamicExec(t, db, `UPDATE user_subscriptions SET weekly_usage_usd=20 WHERE id=11;
 INSERT INTO account_groups VALUES(5,7);
 INSERT INTO accounts(id,type,credentials) VALUES(6,'apikey','{}'); INSERT INTO account_groups VALUES(6,7);
 INSERT INTO dynamic_quota_pools(account_id,max_request_usd) VALUES(5,2),(6,2);
 UPDATE dynamic_quota_pools SET max_request_usd=2 WHERE account_id=4`)
	q, err := loadAdminDebugQuota(ctx, db, 11, time.Now())
	require.NoError(t, err)
	require.Equal(t, 700.0, q.WeeklyLimitUSD)
	require.NotNil(t, q.RemainingUSD)
	require.Equal(t, 680.0, *q.RemainingUSD)
	require.True(t, q.FollowReset)
	require.NoError(t, s.SaveAdminDebugQuota(ctx, 11, q.Revision, 23))
	require.ErrorIs(t, s.SaveAdminDebugQuota(ctx, 11, 0, 999), ErrDynamicQuotaChanged)
	require.ErrorIs(t, s.SaveAdminDebugQuota(ctx, 11, q.Revision, 999), ErrDynamicQuotaChanged)
	for _, invalid := range []float64{-1, math.NaN(), math.Inf(1), 1e10, 1e-12} {
		require.Error(t, s.SaveAdminDebugQuota(ctx, 11, 2, invalid))
	}
	require.Error(t, s.SaveAdminDebugQuota(ctx, 12, 0, 99), "a normal subscriber cannot get the bypass")
	dynamicExec(t, db, `UPDATE groups SET weekly_limit_usd=1; UPDATE user_subscriptions SET weekly_window_start=NOW()-INTERVAL '9 days',weekly_usage_usd=0 WHERE id=11`)
	sub := &UserSubscription{ID: 11, AdminDebug: true, ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, s.Hydrate(ctx, sub))
	require.Equal(t, 20.0, sub.WeeklyUsageUSD, "local clock/manual reset must not erase a source-bound debug week")
	require.Equal(t, 23.0, *sub.EffectiveWeeklyLimit(nil))
	one := 1.0
	g := &Group{DailyLimitUSD: &one, WeeklyLimitUSD: &one, MonthlyLimitUSD: &one}
	sub.DailyUsageUSD, sub.MonthlyUsageUSD = 999, 999
	require.True(t, sub.CheckDailyLimit(g, 1))
	require.True(t, sub.CheckWeeklyLimit(g, 1))
	require.True(t, sub.CheckMonthlyLimit(g, 1))
	require.False(t, sub.NeedsWeeklyReset())
	// Three upstream choices share one subscription lock and one weekly ledger.
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *DynamicQuotaReservation, 3)
	errs := make(chan error, 3)
	for _, source := range []int64{4, 5, 6} {
		wg.Add(1)
		go func(account int64) {
			defer wg.Done()
			<-start
			r, e := s.Begin(ctx, 101, account)
			results <- r
			errs <- e
		}(source)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	var winner *DynamicQuotaReservation
	for r := range results {
		if r != nil {
			require.Nil(t, winner)
			winner = r
		}
	}
	require.NotNil(t, winner)
	rejected := 0
	for e := range errs {
		if e != nil {
			require.ErrorIs(t, e, ErrWeeklyLimitExceeded)
			rejected++
		}
	}
	require.Equal(t, 2, rejected)
	q, err = loadAdminDebugQuota(ctx, db, 11, time.Now())
	require.NoError(t, err)
	require.Equal(t, 2.0, q.ReservedUSD)
	require.Equal(t, 1.0, *q.RemainingUSD, "display follows independent cap minus actual usage and in-flight holds")
	dynamicTestSettle(t, db, winner, 101, 11, 2, 2)
	_, err = s.Begin(ctx, 101, 4)
	require.ErrorIs(t, err, ErrWeeklyLimitExceeded)
	// Replaying the migration cannot overwrite the independent cap or usage.
	migration, err := os.ReadFile("../../migrations/243_admin_debug_weekly_quota.sql")
	require.NoError(t, err)
	dynamicExec(t, db, string(migration))
	q, err = loadAdminDebugQuota(ctx, db, 11, time.Now())
	require.NoError(t, err)
	require.Equal(t, 23.0, q.WeeklyLimitUSD)
	require.Equal(t, 22.0, q.used)
	require.Zero(t, q.ReservedUSD)
	require.Equal(t, 1.0, *q.RemainingUSD, "settlement does not deduct the same hold twice")
	require.NoError(t, s.SaveAdminDebugQuota(ctx, 11, 2, 0))
	q, err = loadAdminDebugQuota(ctx, db, 11, time.Now())
	require.NoError(t, err)
	require.Nil(t, q.RemainingUSD, "unlimited is not a fabricated dollar amount")
	r, err := s.Begin(ctx, 101, 6)
	require.NoError(t, err)
	require.NotNil(t, r)
	r.RejectBeforeForward()
	dynamicExec(t, db, `UPDATE users SET role='user' WHERE id=1`)
	require.Error(t, s.SaveAdminDebugQuota(ctx, 11, 3, 0))
	q, err = loadAdminDebugQuota(ctx, db, 11, time.Now())
	require.NoError(t, err)
	require.Nil(t, q)
}

func TestDynamicQuotaAdminDebugResetSourceAndCrossSourceSettlement(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	in := fixedSeatInput(2)
	in.AccountID = 5
	require.NoError(t, s.SaveGroup(ctx, 8, in))
	require.NoError(t, s.Refresh(ctx, 5))
	require.NoError(t, s.ConvertToAdminDebug(ctx, 11, 1))
	require.NoError(t, s.ConvertToAdminDebug(ctx, 21, 1))
	dynamicExec(t, db, `INSERT INTO account_groups VALUES(5,7);
 UPDATE user_subscriptions SET weekly_usage_usd=20,monthly_usage_usd=30 WHERE id IN (11,21)`)
	cross, err := s.Begin(ctx, 101, 5)
	require.NoError(t, err)
	require.NotNil(t, cross)
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	p, err := lockDynamicPool(ctx, tx, 4)
	require.NoError(t, err)
	p.Snapshot.FetchedAt, p.Snapshot.UsedPercent, p.Snapshot.ResetAt = now.Add(-2*time.Minute), 80, now.Add(time.Hour)
	require.NoError(t, writeDynamicPool(ctx, tx, 4, p))
	require.NoError(t, tx.Commit())
	reset := now.Add(7*24*time.Hour - time.Minute)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 0, reset, now.Add(-time.Minute)), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	q, err := loadAdminDebugQuota(ctx, db, 11, now)
	require.NoError(t, err)
	require.False(t, q.ResetPending)
	require.Equal(t, 20.0, q.used)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 0, reset, now), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = loadAdminDebugQuota(ctx, db, 11, now)
	require.NoError(t, err)
	require.True(t, q.ResetPending)
	require.NotNil(t, q.RemainingUSD)
	require.Zero(t, *q.RemainingUSD, "pending reset cannot promise spendable quota")
	require.Equal(t, 20.0, q.used)
	for _, source := range []int64{4, 5} {
		_, err = s.Begin(ctx, 101, source)
		require.Equal(t, "ADMIN_DEBUG_RESET_PENDING", infraerrors.Reason(err))
	}
	// A different source's bill still lands in the old debug week, exactly once.
	dynamicTestSettle(t, db, cross, 101, 11, 5, 5)
	q, err = loadAdminDebugQuota(ctx, db, 11, now)
	require.NoError(t, err)
	require.Equal(t, 25.0, q.used)
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = loadAdminDebugQuota(ctx, db, 11, now)
	require.NoError(t, err)
	require.False(t, q.ResetPending)
	require.Zero(t, q.used)
	require.Equal(t, q.WeeklyLimitUSD, *q.RemainingUSD)
	other, err := loadAdminDebugQuota(ctx, db, 21, now)
	require.NoError(t, err)
	require.Equal(t, 20.0, other.used)
	var month float64
	require.NoError(t, db.QueryRow(`SELECT monthly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&month))
	require.Equal(t, 35.0, month)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicTestSettle(t, db, r, 101, 11, 1, 1)
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = loadAdminDebugQuota(ctx, db, 11, now)
	require.NoError(t, err)
	require.Equal(t, 1.0, q.used, "repeated observations never clear the new week")
}

func TestDynamicQuotaAdminDebugPreflightCacheAndProgress(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	svc := newTestSubscriptionService()
	svc.now = func() time.Time { return now }
	one := 1.0
	g := &Group{DailyLimitUSD: &one, WeeklyLimitUSD: &one, MonthlyLimitUSD: &one}
	q := &AdminDebugQuota{WeeklyLimitUSD: 60, FollowReset: true, used: 30, rate: 1}
	sub := &UserSubscription{AdminDebug: true, AdminDebugQuota: q, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour), DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now, DailyUsageUSD: 30, WeeklyUsageUSD: 30, MonthlyUsageUSD: 999}
	cache := &dynamicLimitsCache{data: SubscriptionCacheData{Status: sub.Status, ExpiresAt: sub.ExpiresAt, DailyUsage: 30, WeeklyUsage: 999, MonthlyUsage: 999}}
	billing := &BillingCacheService{cache: cache}
	require.NoError(t, svc.CheckUsageLimits(ctx, sub, g, 0))
	maintenance, err := svc.ValidateAndCheckLimits(sub, g)
	require.NoError(t, err)
	require.False(t, maintenance)
	require.NoError(t, billing.checkSubscriptionEligibility(ctx, 1, g, sub), "fresh debug usage replaces stale group usage")
	progress := svc.calculateProgress(sub, g)
	require.Nil(t, progress.Daily)
	require.Nil(t, progress.Monthly)
	require.Equal(t, 60.0, progress.Weekly.LimitUSD)
	q.WeeklyLimitUSD = 30
	require.ErrorIs(t, svc.CheckUsageLimits(ctx, sub, g, 0), ErrWeeklyLimitExceeded)
	require.ErrorIs(t, billing.checkSubscriptionEligibility(ctx, 1, g, sub), ErrWeeklyLimitExceeded)
	q.ResetPending = true
	_, err = svc.ValidateAndCheckLimits(sub, g)
	require.Equal(t, "ADMIN_DEBUG_RESET_PENDING", infraerrors.Reason(err))
}
