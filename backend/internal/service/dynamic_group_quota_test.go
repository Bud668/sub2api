package service

import (
	"context"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func groupQuotaInput() DynamicSubscriptionInput {
	floor := 100.0
	return DynamicSubscriptionInput{Enabled: true, AccountID: 4, Weight: 1, MaxLimitUSD: 600, FloorLimitUSD: &floor}
}

func TestDynamicQuotaGroupAdmissionSeesSaveAfterWaitingForSource(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	dynamicExec(t, db, `UPDATE user_subscriptions SET weekly_usage_usd=601 WHERE id=12`)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = lockDynamicPool(ctx, tx, 4)
	require.NoError(t, err)
	finished := make(chan error, 1)
	go func() {
		r, err := s.Begin(ctx, 102, 4)
		if r != nil {
			r.RejectBeforeForward()
		}
		finished <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
 AND wait_event_type='Lock' AND query LIKE 'SELECT state FROM dynamic_quota_pools%')`).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 5*time.Millisecond)
	_, err = tx.Exec(`INSERT INTO dynamic_group_policies(group_id,account_id,enabled,weight,max_limit_usd,floor_limit_usd) VALUES(7,4,true,1,600,100)`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-finished, ErrDynamicQuotaExhausted, "first turn must not use the stale pre-group decision")
}

func TestDynamicQuotaAdminDebugAssignmentRequiresAdminAccount(t *testing.T) {
	client := newPaymentConfigServiceTestClient(t)
	ctx := context.Background()
	u, err := client.User.Create().SetEmail("debug@example.test").SetPasswordHash("synthetic-hash").SetRole(RoleUser).Save(ctx)
	require.NoError(t, err)
	s := &SubscriptionService{entClient: client}
	in := &AssignSubscriptionInput{UserID: u.ID, AssignedBy: 1, AdminDebug: true}
	require.Error(t, s.validateAdminDebug(ctx, in))
	_, err = client.User.UpdateOneID(u.ID).SetRole(RoleAdmin).Save(ctx)
	require.NoError(t, err)
	require.NoError(t, s.validateAdminDebug(ctx, in))
	in.AssignedBy = 0
	require.Error(t, s.validateAdminDebug(ctx, in), "self-service assignments cannot claim debug exemption")
}

func TestDynamicQuotaExistingAdminDebugPreservesAccounting(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.SaveGroup(ctx, 7, groupQuotaInput()))
	require.NoError(t, s.Refresh(ctx, 4))
	dynamicTestSave(t, s, 21, 5, true)
	old, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	ledger := func() string {
		var value string
		require.NoError(t, db.QueryRow(`SELECT jsonb_build_object(
 'policy', (SELECT to_jsonb(p)-'enabled'-'activation_pending'-'revision'-'updated_at' FROM dynamic_subscription_policies p WHERE subscription_id=11),
 'subscription',(SELECT to_jsonb(us)-'admin_debug'-'updated_at' FROM user_subscriptions us WHERE id=11),
 'requests',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM dynamic_quota_requests r),
 'sources',(SELECT jsonb_agg(to_jsonb(p) ORDER BY account_id) FROM dynamic_quota_pools p))::text`).Scan(&value))
		return value
	}
	before := ledger()
	require.Error(t, s.ConvertToAdminDebug(ctx, 11, 2), "ordinary actor cannot grant an exemption")
	require.Error(t, s.ConvertToAdminDebug(ctx, 12, 1), "ordinary target cannot receive an exemption")
	require.ErrorIs(t, s.ConvertToAdminDebug(ctx, 999, 1), ErrSubscriptionNotFound)
	require.Equal(t, before, ledger())
	require.NoError(t, s.ConvertToAdminDebug(ctx, 11, 1))
	require.NoError(t, s.ConvertToAdminDebug(ctx, 11, 1), "safe retry after response/cache failure")
	require.Equal(t, before, ledger(), "conversion must preserve money, holds, cycles and other source state")
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.False(t, q.Enabled)
	state, err := s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 2, state.Members)
	require.Equal(t, 1, state.DebugMembers)
	dynamicTestSettle(t, db, old, 101, 11, 2, 3)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 23.0, q.UsedUSD, "the original in-flight bill still settles")
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	subID := int64(11)
	require.ErrorIs(t, SettleDynamicQuota(ctx, tx, &UsageBillingCommand{DynamicQuotaReservationID: old.ID, AccountID: 4, APIKeyID: 101, SubscriptionID: &subID, DynamicStandardCost: 2, SubscriptionCost: 3}), ErrUsageBillingRequestConflict)
	require.NoError(t, tx.Rollback())
	dynamicExec(t, db, `INSERT INTO account_groups VALUES(5,7)`)
	debug, err := s.Begin(ctx, 101, 5)
	require.NoError(t, err, "debug can use another allowed upstream without rebinding old bills")
	dynamicTestSettle(t, db, debug, 101, 11, 4, 4)
	other, err := s.Load(ctx, 21)
	require.NoError(t, err)
	require.Equal(t, 20.0, other.UsedUSD, "the same admin's other subscription is unaffected")
	in := groupQuotaInput()
	in.Revision = 1
	require.NoError(t, s.SaveGroup(ctx, 7, in))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.False(t, q.Enabled, "later group saves must preserve debug")
	dynamicExec(t, db, `UPDATE users SET role='user' WHERE id=1`)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.True(t, q.Enabled, "loss of administrator role revokes the exemption")
}

func TestDynamicQuotaConvertedDebugRetainsOldSourceOnGroupEnrollment(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicExec(t, db, `INSERT INTO account_groups VALUES(5,7)`)
	dynamicTestSave(t, s, 11, 5, true)
	require.NoError(t, s.ConvertToAdminDebug(ctx, 11, 1))
	require.NoError(t, s.SaveGroup(ctx, 7, groupQuotaInput()))
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.False(t, q.Enabled)
	require.Equal(t, int64(5), q.AccountID, "retain the historical binding for settlement")
	require.Equal(t, 20.0, q.UsedUSD)
}

func TestDynamicQuotaAdminDebugRechecksConcurrentFirstBinding(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicExec(t, db, `INSERT INTO dynamic_quota_pools(account_id) VALUES(4)`)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`SELECT id FROM user_subscriptions WHERE id=11 FOR UPDATE`)
	require.NoError(t, err)
	finished := make(chan error, 1)
	go func() { finished <- s.ConvertToAdminDebug(ctx, 11, 1) }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
 AND wait_event_type='Lock' AND query LIKE 'SELECT u.role=%')`).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 5*time.Millisecond)
	_, err = tx.Exec(`INSERT INTO dynamic_subscription_policies(subscription_id,account_id,enabled,weight,max_limit_usd,floor_limit_usd) VALUES(11,4,true,1,600,100)`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-finished, ErrDynamicQuotaChanged)
	var debug, enabled bool
	require.NoError(t, db.QueryRow(`SELECT us.admin_debug,p.enabled FROM user_subscriptions us JOIN dynamic_subscription_policies p ON p.subscription_id=us.id WHERE us.id=11`).Scan(&debug, &enabled))
	require.False(t, debug)
	require.True(t, enabled, "a conflict leaves the concurrent save intact")
	require.NoError(t, s.ConvertToAdminDebug(ctx, 11, 1), "retry acquires the newly bound source first")
}

func TestDynamicQuotaGroupEnrollmentDebugAndLateBilling(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicExec(t, db, `UPDATE user_subscriptions SET admin_debug=true WHERE id=11`)
	old, err := s.Begin(ctx, 102, 4)
	require.NoError(t, err)
	in := groupQuotaInput()
	require.NoError(t, s.SaveGroup(ctx, 7, in))
	state, err := s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 2, state.Members)
	require.Equal(t, 1, state.DebugMembers)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Nil(t, q, "an explicitly marked admin has no allocation")
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 12)
	require.NoError(t, err)
	require.True(t, q.Enabled)
	require.True(t, q.GroupManaged)
	require.Equal(t, 600.0, q.LimitUSD)
	require.Equal(t, 20.0, q.UsedUSD)
	dynamicTestSettle(t, db, old, 102, 12, 2, 3)
	q, err = s.Load(ctx, 12)
	require.NoError(t, err)
	require.Equal(t, 23.0, q.UsedUSD, "pre-enrollment usage bills exactly once to its owner")
	allocation := q.allocatedStandard
	for range 3 {
		q, err = s.Load(ctx, 12)
		require.NoError(t, err)
		require.Equal(t, allocation, q.allocatedStandard)
	}
	// A new subscription joins without any individual configuration or page open.
	dynamicExec(t, db, `INSERT INTO users(id) VALUES(4); INSERT INTO user_subscriptions(id,user_id,group_id) VALUES(14,4,7)`)
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 14)
	require.NoError(t, err)
	require.True(t, q.Enabled)
	require.Equal(t, 600.0, q.MaxLimitUSD)
	state, err = s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 3, state.Members)
	// Debug traffic still consumes the source, without receiving a personal share.
	before, _, _, _, err := dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	debugRequest, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicTestSettle(t, db, debugRequest, 101, 11, 5, 5)
	after, _, _, _, err := dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	require.Equal(t, before+5, after)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Nil(t, q)
	dynamicExec(t, db, `INSERT INTO account_groups VALUES(5,7)`)
	debugRequest, err = s.Begin(ctx, 101, 5)
	require.NoError(t, err, "debug can test another account allowed in its group")
	debugRequest.RejectBeforeForward()
	// Role alone is not an exemption; revoking admin role also revokes debug.
	dynamicExec(t, db, `UPDATE users SET role='user' WHERE id=1`)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.True(t, q.Enabled)
	other, err := s.Load(ctx, 21)
	require.NoError(t, err)
	require.Nil(t, other, "group 8 never inherits group 7")
}

func TestDynamicQuotaGroupSaveIsAtomicAndSourceFixed(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	saveDynamicV2(t, s, 12, 4, true, 500, 100)
	dynamicExec(t, db, `INSERT INTO account_groups VALUES(5,7)`)
	saveDynamicV2(t, s, 13, 5, true, 500, 100)
	in := groupQuotaInput()
	require.ErrorIs(t, s.SaveGroup(ctx, 7, in), ErrDynamicQuotaBinding)
	policies, err := s.GroupPolicies(ctx)
	require.NoError(t, err)
	require.Empty(t, policies, "no partially enabled group on a conflicting source")
	q, err := s.Load(ctx, 12)
	require.NoError(t, err)
	require.Equal(t, 500.0, q.LimitUSD, "an earlier member is rolled back too")
	dynamicExec(t, db, `UPDATE user_subscriptions SET deleted_at=NOW() WHERE id=13`)
	require.NoError(t, s.SaveGroup(ctx, 7, in))
	require.ErrorIs(t, s.SaveGroup(ctx, 7, in), ErrDynamicQuotaChanged)
	in.Revision = 1
	in.AccountID = 5
	require.ErrorIs(t, s.SaveGroup(ctx, 7, in), ErrDynamicQuotaBinding)
	q, err = s.Load(ctx, 12)
	require.NoError(t, err)
	individual := groupQuotaInput()
	individual.Revision = q.Revision
	err = s.Save(ctx, 12, individual)
	require.Equal(t, "DYNAMIC_QUOTA_GROUP_MANAGED", infraerrors.Reason(err))
}

func TestDynamicQuotaGroupConcurrentFirstUseAndTogglePreserveUsage(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	in := groupQuotaInput()
	require.NoError(t, s.SaveGroup(ctx, 7, in))
	require.NoError(t, s.Refresh(ctx, 4))
	dynamicExec(t, db, `INSERT INTO users(id) VALUES(4); INSERT INTO user_subscriptions(id,user_id,group_id) VALUES(14,4,7); INSERT INTO api_keys(id,user_id,group_id) VALUES(114,4,7)`)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Load(ctx, 14); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	q, err := s.Load(ctx, 14)
	require.NoError(t, err)
	require.Equal(t, int64(1), q.Revision, "only one enrollment despite concurrent requests")
	r, err := s.Begin(ctx, 114, 4)
	require.NoError(t, err)
	in.Enabled = false
	in.Revision = 1
	require.NoError(t, s.SaveGroup(ctx, 7, in))
	dynamicTestSettle(t, db, r, 114, 14, 2, 2)
	in.Enabled = true
	in.Revision = 2
	require.NoError(t, s.SaveGroup(ctx, 7, in))
	q, err = s.Load(ctx, 14)
	require.NoError(t, err)
	require.Equal(t, 22.0, q.UsedUSD)
	require.Equal(t, 600.0, q.LimitUSD)
	require.Equal(t, int64(1), q.Cycle)
}
