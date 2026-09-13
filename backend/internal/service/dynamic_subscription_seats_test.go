package service

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func fixedSeatInput(slots int) DynamicSubscriptionInput {
	return DynamicSubscriptionInput{Enabled: true, AccountID: 4, Weight: 1, MaxLimitUSD: 600, FixedSlots: slots}
}

func fixedSeatStore(t *testing.T, slots int) (*DynamicSubscriptionService, *sql.DB) {
	t.Helper()
	s, db := dynamicTestStore(t)
	dynamicExec(t, db, `UPDATE user_subscriptions SET weekly_usage_usd=0; UPDATE user_subscriptions SET status='expired' WHERE id IN (12,13)`)
	reset := time.Now().UTC().Add(6 * 24 * time.Hour)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 0, reset, time.Now().UTC()), nil
	}
	require.NoError(t, s.SaveGroup(context.Background(), 7, fixedSeatInput(slots)))
	require.NoError(t, s.Refresh(context.Background(), 4))
	return s, db
}

// Supply synthetic already-verified evidence. Observation validation itself is
// covered separately below; this exercises real SQL allocation and admission.
func fixedSeatEvidence(t *testing.T, s *DynamicSubscriptionService, capacity, percent float64, advance bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	p, err := lockDynamicPool(ctx, tx, 4)
	require.NoError(t, err)
	p.CapacityUSD, p.Status = capacity, "active"
	p.Snapshot.FetchedAt, p.Snapshot.UsedPercent = time.Now().UTC(), percent
	p.Snapshot.WindowCostUSD, p.Snapshot.WindowStandardUSD = capacity*percent/100, capacity*percent/100
	p.Health = dynamicQuotaHealth{}
	if advance {
		p.V2.LastNode = 0
		p.LastAllocationAt = time.Now().Add(-time.Second)
	}
	require.NoError(t, s.reallocateFixedSeats(ctx, tx, 4, p, time.Now().UTC(), false))
	require.NoError(t, writeDynamicPool(ctx, tx, 4, p))
	require.NoError(t, tx.Commit())
}

func TestDynamicQuotaFixedSeatsReserveVacanciesAndJoinMidCycle(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 60.0, q.LimitUSD, "never release the full 600 cap before learning")
	require.Equal(t, 4, q.FixedSlots)
	require.Equal(t, 2, q.NextAdjustmentPercent)
	fixedSeatEvidence(t, s, 1600, 20, true)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 320, q.LimitUSD, 1e-7, "one member still receives only one of four reserved shares")
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicTestSettle(t, db, r, 101, 11, 100, 100)
	// Existing inactive user joins halfway through the cycle; no new cap grant.
	dynamicExec(t, db, `UPDATE user_subscriptions SET status='active' WHERE id=12`)
	joined, err := s.Load(ctx, 12)
	require.NoError(t, err)
	require.InDelta(t, 320, joined.LimitUSD, 1e-7)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 320, q.LimitUSD, 1e-7)
	require.Equal(t, 100.0, q.UsedUSD)
	state, err := s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 4, state.EffectiveSlots)
	require.Equal(t, 2, state.OccupiedSlots)
	var reserved float64
	require.NoError(t, db.QueryRow(`SELECT sum(allocated_standard_usd) FROM dynamic_quota_seats WHERE subscription_id IS NULL AND account_id=4 AND cycle=1`).Scan(&reserved))
	require.InDelta(t, 640, reserved, 1e-7, "empty shares must not be lent to active subscribers")
	for range 3 {
		require.NoError(t, s.Refresh(ctx, 4))
	}
}

func TestDynamicQuotaFixedSeatsUsageHoldsAndImmediateSafety(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	fixedSeatEvidence(t, s, 1600, 20, true)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 320-0.16, q.RemainingUSD, 1e-7)
	fixedSeatEvidence(t, s, 1600, 20, false)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 320, q.LimitUSD, 1e-7, "a pending hold is included once, not subtracted from the published cap")
	dynamicTestSettle(t, db, r, 101, 11, 5, 5)
	fixedSeatEvidence(t, s, 1590, 20, false)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 318, q.LimitUSD, 1e-7, "verified $2 safety cut does not wait for $20 or a node")
	require.Equal(t, 5.0, q.UsedUSD)
	require.Equal(t, "budget_safety", q.LastChange.Reason)
	fixedSeatEvidence(t, s, 1660, 20, true)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 332, q.LimitUSD, 1e-7, "every valid node applies even a small increase")
	fixedSeatEvidence(t, s, 1710, 20, true)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 342, q.LimitUSD, 1e-7, "apply the calculated value, not a rounded multiple of 20")
}

func TestDynamicQuotaFixedSeatsRetainOwnersAndResetIsolation(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	fixedSeatEvidence(t, s, 1600, 20, true)
	dynamicTestSave(t, s, 21, 5, true)
	otherBefore, err := s.Load(ctx, 21)
	require.NoError(t, err)
	// Deleting/recreating the same user's subscription cannot claim a vacancy.
	dynamicExec(t, db, `UPDATE user_subscriptions SET deleted_at=NOW() WHERE id=11;
 INSERT INTO user_subscriptions(id,user_id,group_id,weekly_usage_usd) VALUES(31,1,7,0)`)
	_, err = s.Load(ctx, 31)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDynamicQuotaSeatRetained)
	dynamicExec(t, db, `UPDATE user_subscriptions SET deleted_at=NOW() WHERE id=31; UPDATE user_subscriptions SET deleted_at=NULL WHERE id=11`)
	in := fixedSeatInput(6)
	in.Revision = 1
	require.NoError(t, s.SaveGroup(ctx, 7, in))
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 6, q.FixedSlots, "explicit safe count changes apply in this cycle")
	state, err := s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 6, state.Policy.FixedSlots)
	require.Equal(t, 6, state.EffectiveSlots)
	// Two coherent, independently fetched observations, not a local seven-day timer.
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	p, err := lockDynamicPool(ctx, tx, 4)
	require.NoError(t, err)
	p.Snapshot.FetchedAt = now.Add(-2 * time.Minute)
	p.Snapshot.UsedPercent = 80
	p.Snapshot.ResetAt = now.Add(time.Hour)
	p.Health = dynamicQuotaHealth{}
	require.NoError(t, writeDynamicPool(ctx, tx, 4, p))
	require.NoError(t, tx.Commit())
	reset := now.Add(7*24*time.Hour - time.Minute)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 0, reset, now.Add(-time.Minute)), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, int64(1), q.Cycle)
	require.Equal(t, 6, q.FixedSlots)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 0, reset, now), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, int64(2), q.Cycle)
	require.Equal(t, 6, q.FixedSlots)
	require.Equal(t, 60.0, q.LimitUSD)
	require.Zero(t, q.CapacityEstimateUSD)
	otherAfter, err := s.Load(ctx, 21)
	require.NoError(t, err)
	require.Equal(t, otherBefore.Cycle, otherAfter.Cycle)
	require.Equal(t, otherBefore.UsedUSD, otherAfter.UsedUSD)
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, int64(2), q.Cycle)
}

func TestDynamicQuotaFixedSeatsMilestonesAndReset(t *testing.T) {
	now := time.Now().UTC().Add(-30 * time.Minute)
	p := &DynamicQuotaPoolState{}
	p.enableFixedSeats()
	o := dynamicTestObservation(4, 0, now.Add(7*24*time.Hour), now)
	require.False(t, p.Observe(o, now))
	for _, percent := range []int{2, 4, 6, 8, 10, 15, 20, 25, 30, 35, 40, 95} {
		o.FetchedAt = o.FetchedAt.Add(time.Minute)
		o.UsedPercent = float64(percent)
		o.LocalStandardTotal = float64(percent) * 16
		o.WindowCostUSD, o.WindowStandardUSD = o.LocalStandardTotal, o.LocalStandardTotal
		require.False(t, p.Observe(o, o.FetchedAt))
		require.InDelta(t, 1600, p.CapacityUSD, 1e-7)
		require.True(t, p.v2AllocationDue(o.FetchedAt))
		p.V2.LastNode, p.LastAllocationAt = percent, o.FetchedAt
		require.False(t, p.v2AllocationDue(o.FetchedAt), "a node is not a repeat grant")
	}
	for _, tc := range []struct {
		percent    float64
		step, node int
	}{{0, 2, 0}, {9.9, 2, 8}, {10, 5, 10}, {29.9, 5, 25}, {30, 5, 30}, {99, 5, 95}} {
		require.Equal(t, tc.step, dynamicQuotaStep(tc.percent))
		require.Equal(t, tc.node, dynamicQuotaNode(tc.percent))
	}
	p.V2.CandidateSamples = 1
	require.Equal(t, 5, dynamicQuotaStep(70), "guard does not secretly change the schedule")
	o.UsedPercent, o.WindowCostUSD, o.WindowStandardUSD = 0, 0, 0
	p.Candidate = &o
	p.Confirm(o.FetchedAt)
	require.Zero(t, p.CapacityUSD)
	require.True(t, p.V2.FixedSeats)
	require.Equal(t, 60.0, dynamicStartupLimit(1e9), "a huge cap must not create huge startup credit")
	tiny := fixedSeatInput(4)
	tiny.MaxLimitUSD = 1e-12
	require.Error(t, validateDynamicInput(&tiny), "rounding must not turn a positive cap into zero")
}

func TestDynamicQuotaFixedSeatsMilestoneDisplay(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	fixedSeatEvidence(t, s, 1600, 17, true)
	baseline, err := s.Load(ctx, 11)
	require.NoError(t, err)
	for _, tc := range []struct {
		name            string
		candidate, next int
		percent         float64
	}{
		{"normal", 0, 20, 17},
		{"capacity change", 1, 20, 17},
		{"early", 0, 10, 8},
		{"no next node", 0, 0, 98},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updateDynamicGuardPool(t, db, 4, func(p *DynamicQuotaPoolState) {
				p.V2.CandidateSamples = tc.candidate
				p.Snapshot.UsedPercent = tc.percent
				p.V2.LastNode = dynamicQuotaNode(tc.percent)
			})
			q, err := s.Load(ctx, 11)
			require.NoError(t, err)
			require.Equal(t, tc.next, q.NextAdjustmentPercent)
			require.Equal(t, tc.candidate > 0, q.GrowthFrozen)
			require.Equal(t, baseline.LimitUSD, q.LimitUSD, "display reads do not allocate")
			require.Equal(t, baseline.UsedUSD, q.UsedUSD, "display reads do not bill or reset")
			require.Equal(t, baseline.LastAllocationAt, q.LastAllocationAt)
		})
	}
}

func TestDynamicQuotaFixedSeatsSafeCurrentCycleResize(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	in := fixedSeatInput(6)
	in.Revision = 1
	require.ErrorIs(t, s.SaveGroup(ctx, 7, in), ErrDynamicQuotaSeatsLearning)
	fixedSeatEvidence(t, s, 1600, 20, true)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicTestSettle(t, db, r, 101, 11, 250, 250)
	// Six shares of $1,280 are below the $250 already consumed by one owner.
	require.ErrorIs(t, s.SaveGroup(ctx, 7, in), ErrDynamicQuotaSeatsSpent)
	state, err := s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 4, state.Policy.FixedSlots)
	require.Equal(t, int64(1), state.Policy.Revision)
	require.Equal(t, 4, state.EffectiveSlots, "failed reallocation must roll back newly inserted vacancies")
	in.FixedSlots = 5
	require.NoError(t, s.SaveGroup(ctx, 7, in))
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 5, q.FixedSlots)
	require.InDelta(t, 256, q.LimitUSD, 1e-7)
	require.Equal(t, 250.0, q.UsedUSD)
	// Remove only never-occupied vacancies. This explicit change may expand
	// existing allowances even outside an automatic node or its $20 threshold.
	in.FixedSlots = 4
	in.Revision = 2
	require.NoError(t, s.SaveGroup(ctx, 7, in))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.InDelta(t, 320, q.LimitUSD, 1e-7)
	dynamicExec(t, db, `UPDATE user_subscriptions SET status='active' WHERE id=12`)
	_, err = s.Load(ctx, 12)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE user_subscriptions SET status='expired' WHERE id=12`)
	in.FixedSlots = 1
	in.Revision = 3
	require.ErrorIs(t, s.SaveGroup(ctx, 7, in), ErrDynamicQuotaSlotsFull, "expiry does not make an occupied seat removable")
}

func TestDynamicQuotaFixedSeatsOverspentOwnerCannotTakeReservedShares(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	fixedSeatEvidence(t, s, 2400, 20, true)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicTestSettle(t, db, r, 101, 11, 350, 350)
	fixedSeatEvidence(t, s, 800, 20, false)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 350.0, q.UsedUSD)
	require.Zero(t, q.RemainingUSD, "once evidence shrinks, an overspent owner must stop without waiting for a node")
	_, err = s.Begin(ctx, 101, 4)
	require.ErrorIs(t, err, ErrDynamicQuotaExhausted)
	var empty int
	var reserved float64
	require.NoError(t, db.QueryRow(`SELECT count(*),sum(allocated_standard_usd) FROM dynamic_quota_seats WHERE account_id=4 AND cycle=1 AND subscription_id IS NULL`).Scan(&empty, &reserved))
	require.Equal(t, 3, empty)
	require.Positive(t, reserved, "no loans from unoccupied seats")
	dynamicExec(t, db, `UPDATE user_subscriptions SET status='active' WHERE id=12`)
	joined, err := s.Load(ctx, 12)
	require.NoError(t, err)
	require.InDelta(t, reserved/3, joined.LimitUSD, 1e-6)
	require.Less(t, joined.LimitUSD, 600.0, "joining is not another full-cap grant")
}

func TestDynamicQuotaFixedSeatsDebugUsageAndOfficialThreshold(t *testing.T) {
	s, db := fixedSeatStore(t, 4)
	ctx := context.Background()
	fixedSeatEvidence(t, s, 1600, 20, true)
	require.NoError(t, s.ConvertToAdminDebug(ctx, 11, 1))
	state, err := s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 1, state.OccupiedSlots, "a converted owner keeps the original seat this cycle")
	require.Equal(t, 1, state.DebugMembers)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicTestSettle(t, db, r, 101, 11, 10, 10)
	dynamicExec(t, db, `UPDATE accounts SET extra='{"auto_pause_7d_threshold":0.99}' WHERE id=4`)
	fixedSeatEvidence(t, s, 1600, 20, false)
	var total float64
	require.NoError(t, db.QueryRow(`SELECT sum(allocated_standard_usd) FROM dynamic_quota_seats WHERE account_id=4 AND cycle=1`).Scan(&total))
	require.InDelta(t, 1264, total, 1e-7, "official 99% threshold is not allocated, and debug consumption is retained")
	// A brand-new debug subscription is exempt, but regular admins are not.
	dynamicExec(t, db, `INSERT INTO users(id,role) VALUES(44,'admin'); INSERT INTO user_subscriptions(id,user_id,group_id,admin_debug,weekly_usage_usd) VALUES(44,44,7,true,0)`)
	q, err := s.Load(ctx, 44)
	require.NoError(t, err)
	require.Nil(t, q)
	state, err = s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 1, state.OccupiedSlots)
}

// The actual createSubscription path uses the caller's Ent transaction. A tiny
// repository adapter uses the same isolated tables without production fixtures.
type fixedSeatSubscriptionRepo struct {
	UserSubscriptionRepository
	db *sql.DB
}

func (r fixedSeatSubscriptionRepo) Create(ctx context.Context, sub *UserSubscription) error {
	return scanDynamicSeat(ctx, dbent.TxFromContext(ctx), `INSERT INTO user_subscriptions(id,user_id,group_id,weekly_usage_usd,admin_debug)
 VALUES((SELECT COALESCE(max(id),0)+1 FROM user_subscriptions),$1,$2,0,$3) RETURNING id`, []any{sub.UserID, sub.GroupID, sub.AdminDebug}, &sub.ID)
}
func (r fixedSeatSubscriptionRepo) GetByID(ctx context.Context, id int64) (*UserSubscription, error) {
	var db dynamicSeatTx = r.db
	if tx := dbent.TxFromContext(ctx); tx != nil {
		db = tx
	}
	sub := &UserSubscription{ID: id}
	err := scanDynamicSeat(ctx, db, `SELECT user_id,group_id,status,expires_at,admin_debug FROM user_subscriptions WHERE id=$1`, []any{id}, &sub.UserID, &sub.GroupID, &sub.Status, &sub.ExpiresAt, &sub.AdminDebug)
	return sub, err
}

func (r fixedSeatSubscriptionRepo) GetByIDForUpdate(ctx context.Context, id int64) (*UserSubscription, error) {
	if _, err := dbent.TxFromContext(ctx).ExecContext(ctx, `SELECT id FROM user_subscriptions WHERE id=$1 FOR UPDATE`, id); err != nil {
		return nil, err
	}
	return r.GetByID(ctx, id)
}

func (r fixedSeatSubscriptionRepo) Update(ctx context.Context, sub *UserSubscription) error {
	_, err := dbent.TxFromContext(ctx).ExecContext(ctx, `UPDATE user_subscriptions SET expires_at=$2,status=$3,weekly_usage_usd=$4,weekly_window_start=$5 WHERE id=$1`, sub.ID, sub.ExpiresAt, sub.Status, sub.WeeklyUsageUSD, sub.WeeklyWindowStart)
	return err
}

func TestDynamicQuotaFixedSeatsExpiredOwnerRenewsWithoutRegrant(t *testing.T) {
	s, db := fixedSeatStore(t, 1)
	ctx := context.Background()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	ss := &SubscriptionService{DynamicQuotas: s, entClient: client, userSubRepo: fixedSeatSubscriptionRepo{db: db}}
	fixedSeatEvidence(t, s, 1600, 20, true)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicTestSettle(t, db, r, 101, 11, 20, 20)
	dynamicExec(t, db, `UPDATE user_subscriptions SET expires_at=NOW()-INTERVAL '1 day',status='expired' WHERE id=11`)
	require.NoError(t, ss.updateExistingSubscriptionTerm(ctx, 11, 30, "", true))
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 20.0, q.UsedUSD)
	require.Equal(t, int64(1), q.Cycle)
	state, err := s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 1, state.OccupiedSlots)
}

func TestDynamicQuotaAdminDebugCreationSerializesFirstGroupBinding(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	dynamicExec(t, db, `INSERT INTO users(id,role) VALUES(44,'admin')`)
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	txCtx := dbent.NewTxContext(ctx, tx)
	_, _, err = s.prepareFixedAssignment(txCtx, 7, 0, 44, true)
	require.NoError(t, err)
	sub := &UserSubscription{UserID: 44, GroupID: 7, AdminDebug: true}
	require.NoError(t, (fixedSeatSubscriptionRepo{db: db}).Create(txCtx, sub))
	require.NoError(t, initializeAdminDebugQuota(txCtx, tx, sub.ID))
	saved := make(chan error, 1)
	go func() { saved <- s.SaveGroup(ctx, 7, fixedSeatInput(4)) }()
	require.NoError(t, tx.Commit())
	require.NoError(t, <-saved)
	q, err := loadAdminDebugQuota(ctx, db, sub.ID, time.Now())
	require.NoError(t, err)
	require.True(t, q.FollowReset)
	state, err := s.GroupStatus(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 3, state.OccupiedSlots)
	require.Equal(t, 1, state.DebugMembers)
}

func TestDynamicQuotaFixedSeatsAtomicAssignmentAndOuterRollback(t *testing.T) {
	s, db := fixedSeatStore(t, 2)
	ctx := context.Background()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	ss := &SubscriptionService{DynamicQuotas: s, entClient: client, userSubRepo: fixedSeatSubscriptionRepo{db: db}}
	dynamicExec(t, db, `INSERT INTO users(id) VALUES(40),(41),(42); CREATE TABLE synthetic_purchase(id INTEGER);`)
	// A seat claim is rolled back together with any purchase/redeem writes.
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	txCtx := dbent.NewTxContext(ctx, tx)
	_, err = tx.ExecContext(txCtx, `INSERT INTO synthetic_purchase VALUES(1)`)
	require.NoError(t, err)
	_, err = ss.createSubscription(txCtx, &AssignSubscriptionInput{UserID: 40, GroupID: 7})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_subscriptions WHERE user_id=40`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_seats WHERE user_id=40`).Scan(&count))
	require.Zero(t, count)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, user := range []int64{41, 42} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			_, e := ss.createSubscription(ctx, &AssignSubscriptionInput{UserID: id, GroupID: 7})
			results <- e
		}(user)
	}
	wg.Wait()
	close(results)
	success, full := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else {
			require.ErrorIs(t, err, ErrDynamicQuotaSlotsFull)
			full++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, full)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_subscriptions WHERE user_id IN (41,42)`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = ss.createSubscription(ctx, &AssignSubscriptionInput{UserID: 40, GroupID: 7})
	require.ErrorIs(t, err, ErrDynamicQuotaSlotsFull)
}
