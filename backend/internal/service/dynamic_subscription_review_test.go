package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaIdleZeroGapRecovery(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	p := DynamicQuotaPoolState{}
	base := DynamicQuotaObservation{Identity: "same-account", UsedPercent: 0, ResetAt: now.Add(7 * 24 * time.Hour), WindowSeconds: 604800, FetchedAt: now}
	require.False(t, p.Observe(base, now))
	for minute := 3; minute <= 8; minute++ {
		next := base
		next.FetchedAt = now.Add(time.Duration(minute) * time.Minute)
		next.ResetAt = next.FetchedAt.Add(7 * 24 * time.Hour)
		require.False(t, p.Observe(next, next.FetchedAt))
	}
	require.Equal(t, int64(1), p.Cycle)
	require.Nil(t, p.ConfirmedAt)
	require.Equal(t, "learning", p.Status)
}

func TestDynamicQuotaDisableAfterSourceRemoved(t *testing.T) {
	s, db := dynamicTestStore(t)
	dynamicTestSave(t, s, 11, 4, true)
	r, err := s.Begin(context.Background(), 101, 4)
	require.NoError(t, err)
	dynamicExec(t, db, `DELETE FROM account_groups WHERE account_id=4 AND group_id=7`)
	dynamicExec(t, db, `UPDATE accounts SET deleted_at=NOW(),credentials='{}' WHERE id=4`)
	s.accounts = nil // Disabling must not need another source lookup.
	in := DynamicSubscriptionInput{Enabled: false, Revision: 1, AccountID: 4, Weight: 1, MaxLimitUSD: 700, FloorLimitUSD: dynamicTestFloor()}
	require.NoError(t, s.Save(context.Background(), 11, in), "live billing no longer blocks off/on")
	dynamicTestSettle(t, db, r, 101, 11, 2, 2)
	q, err := s.Load(context.Background(), 11)
	require.NoError(t, err)
	require.False(t, q.Enabled)
	require.Equal(t, 22.0, q.UsedUSD)
}

func TestDynamicQuotaExpiredMemberDoesNotBlockOffUsers(t *testing.T) {
	s, db := dynamicTestStore(t)
	dynamicTestSave(t, s, 11, 4, true)
	dynamicExec(t, db, `UPDATE user_subscriptions SET status='expired',expires_at=NOW()-INTERVAL '1 day' WHERE id=11`)
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET state=jsonb_set(state,'{snapshot,fetched_at}',to_jsonb($1::text)) WHERE account_id=4`, time.Now().UTC().Add(-11*time.Minute).Format(time.RFC3339Nano))
	r, err := s.Begin(context.Background(), 102, 4)
	if r != nil {
		r.RejectBeforeForward()
	}
	require.NoError(t, err)
}

func TestDynamicQuotaInactiveMembershipDoesNotProtectSharedSource(t *testing.T) {
	for name, mutation := range map[string]string{
		"user suspended":  `UPDATE users SET status='disabled' WHERE id=1`,
		"user deleted":    `UPDATE users SET deleted_at=NOW() WHERE id=1`,
		"group suspended": `UPDATE groups SET status='disabled' WHERE id=7`,
		"group deleted":   `UPDATE groups SET deleted_at=NOW() WHERE id=7`,
		"source removed":  `DELETE FROM account_groups WHERE account_id=4 AND group_id=7`,
	} {
		t.Run(name, func(t *testing.T) {
			s, db := dynamicTestStore(t)
			dynamicTestSave(t, s, 11, 4, true)
			dynamicExec(t, db, `INSERT INTO account_groups VALUES(4,8); INSERT INTO api_keys(id,user_id,group_id) VALUES(202,2,8)`)
			dynamicExec(t, db, mutation)
			updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) { p.Health.Failures = dynamicQuotaGuardChecks })
			r, err := s.Begin(context.Background(), 202, 4)
			require.NoError(t, err)
			r.RejectBeforeForward()
		})
	}
}
