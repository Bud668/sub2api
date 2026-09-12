package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrDynamicQuotaSlotsFull     = infraerrors.Conflict("DYNAMIC_QUOTA_SLOTS_FULL", "All fixed seats in this upstream cycle are occupied")
	ErrDynamicQuotaSeatRetained  = infraerrors.Conflict("DYNAMIC_QUOTA_SEAT_RETAINED", "This user's seat belongs to an earlier subscription in the same upstream cycle")
	ErrDynamicQuotaSeatsSpent    = infraerrors.Conflict("DYNAMIC_QUOTA_SEATS_SPENT", "Existing usage and in-flight reservations exceed the proposed equal shares; this cycle's seats were not changed")
	ErrDynamicQuotaSeatsLearning = infraerrors.Conflict("DYNAMIC_QUOTA_SEATS_LEARNING", "Changing current-cycle seats requires fresh verified capacity; the saved count is unchanged")
)

// Both the gateway's sql.Tx and subscription/payment Ent transactions use the
// same source lock and seat operations, without a second billing transaction.
type dynamicSeatTx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func scanDynamicSeat(ctx context.Context, tx dynamicSeatTx, query string, args []any, dest ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return rows.Scan(dest...)
}

func fixedSeatCycle(p *DynamicQuotaPoolState) int64 {
	if p.Cycle > 0 {
		return p.Cycle
	}
	return 1
}

func (p *DynamicQuotaPoolState) enableFixedSeats() {
	if p.V2 == nil {
		p.startV2()
	}
	if p.V2.FixedSeats {
		return
	}
	p.V2.FixedSeats = true
	p.V2.LastNode *= 10 // Old metadata stored a ten-percent node index.
}

// One schedule for learning, allocation and the next-node card. A new mid-cycle
// source or an unconfirmed anomaly keeps dense checks until evidence is stable.
func (p *DynamicQuotaPoolState) fixedSeatStep(percent float64) int {
	if percent < 10 || len(p.Samples) < 3 || p.V2.CandidateSamples > 0 {
		return 2
	}
	if percent < 30 {
		return 5
	}
	return 10
}

func (p *DynamicQuotaPoolState) fixedSeatNode(percent float64) int {
	step := p.fixedSeatStep(percent)
	return int(math.Floor(percent/float64(step))) * step
}

// A finite learning allowance, not a claim about unknown upstream capacity.
// ponytail: no past-cycle estimate; a source that cannot produce evidence within
// this allowance waits for more same-cycle evidence instead of unlimited credit.
func dynamicStartupLimit(cap float64) float64 {
	return math.Min(cap, math.Max(20, math.Min(60, cap*0.1)))
}

func ensureFixedSeats(ctx context.Context, tx dynamicSeatTx, p *DynamicQuotaPoolState, group *DynamicGroupPolicy) error {
	if !group.Enabled || group.FixedSlots <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO dynamic_quota_seats(account_id,cycle,group_id,position)
 SELECT $1,$2,$3,n FROM generate_series(1,$4::int) n
 WHERE NOT EXISTS(SELECT 1 FROM dynamic_quota_seats WHERE account_id=$1 AND cycle=$2 AND group_id=$3)`,
		group.AccountID, fixedSeatCycle(p), group.GroupID, group.FixedSlots)
	return err
}

// Only an explicit administrator save can resize current-cycle seats. Empty
// seats are never lent automatically; rows with an accounting owner cannot be
// deleted. All count and grant changes commit with the group revision.
func resizeFixedSeats(ctx context.Context, tx *sql.Tx, p *DynamicQuotaPoolState, group *DynamicGroupPolicy) (bool, error) {
	var current, occupied, source int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE group_id=$3),
 count(*) FILTER(WHERE group_id=$3 AND subscription_id IS NOT NULL),count(*)
 FROM dynamic_quota_seats WHERE account_id=$1 AND cycle=$2`, group.AccountID, fixedSeatCycle(p), group.GroupID).Scan(&current, &occupied, &source); err != nil {
		return false, err
	}
	if current == group.FixedSlots || (current == 0 && !group.Enabled) {
		return false, nil
	}
	if occupied > group.FixedSlots {
		return false, ErrDynamicQuotaSlotsFull
	}
	if source == 0 {
		return false, ensureFixedSeats(ctx, tx, p, group)
	}
	now := time.Now().UTC()
	if p.CapacityUSD <= 0 || !p.trustedSnapshot(now) || p.growthFrozen(now) || p.Status != "active" || len(p.Samples) < 2 {
		return false, ErrDynamicQuotaSeatsLearning
	}
	if current > group.FixedSlots {
		_, err := tx.ExecContext(ctx, `DELETE FROM dynamic_quota_seats WHERE id IN (
 SELECT id FROM dynamic_quota_seats WHERE account_id=$1 AND cycle=$2 AND group_id=$3 AND subscription_id IS NULL
 ORDER BY position DESC LIMIT $4)`, group.AccountID, fixedSeatCycle(p), group.GroupID, current-group.FixedSlots)
		return true, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO dynamic_quota_seats(account_id,cycle,group_id,position)
 SELECT $1,$2,$3,n FROM generate_series(1,1000) n WHERE NOT EXISTS(SELECT 1 FROM dynamic_quota_seats
 WHERE account_id=$1 AND cycle=$2 AND group_id=$3 AND position=n) ORDER BY n LIMIT $4`, group.AccountID, fixedSeatCycle(p), group.GroupID, group.FixedSlots-current)
	return true, err
}

// The caller owns the source lock. A vacancy is reserved even without an owner;
// off/on, expiry, soft deletion and debug conversion never free an occupied seat.
func claimFixedSeat(ctx context.Context, tx dynamicSeatTx, p *DynamicQuotaPoolState, group *DynamicGroupPolicy, subID, userID int64) error {
	if !group.Enabled || group.FixedSlots <= 0 {
		return nil
	}
	if err := ensureFixedSeats(ctx, tx, p, group); err != nil {
		return err
	}
	var id int64
	var owner, user sql.NullInt64
	err := scanDynamicSeat(ctx, tx, `SELECT id,subscription_id,user_id FROM dynamic_quota_seats
 WHERE account_id=$1 AND cycle=$2 AND group_id=$3 AND (subscription_id=$4 OR user_id=$5 OR subscription_id IS NULL)
 ORDER BY subscription_id IS NULL,position LIMIT 1 FOR UPDATE`,
		[]any{group.AccountID, fixedSeatCycle(p), group.GroupID, subID, userID}, &id, &owner, &user)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDynamicQuotaSlotsFull
	}
	if err != nil {
		return err
	}
	if owner.Valid {
		if owner.Int64 != subID || user.Int64 != userID {
			return ErrDynamicQuotaSeatRetained
		}
		var current bool
		if err = scanDynamicSeat(ctx, tx, `SELECT status='active' AND expires_at>NOW() AND deleted_at IS NULL FROM user_subscriptions WHERE id=$1`, []any{subID}, &current); err != nil {
			return err
		}
		if current {
			return nil
		}
	}
	// An expired owner may renew its own seat; it cannot claim a second seat.
	var active int
	if err = scanDynamicSeat(ctx, tx, `SELECT count(*) FROM user_subscriptions us JOIN users u ON u.id=us.user_id
 WHERE us.group_id=$1 AND us.id<>$2 AND us.deleted_at IS NULL AND us.status='active' AND us.expires_at>NOW()
 AND u.deleted_at IS NULL AND u.status='active' AND NOT(us.admin_debug AND u.role='admin')`, []any{group.GroupID, subID}, &active); err != nil {
		return err
	}
	if active >= group.FixedSlots {
		return ErrDynamicQuotaSlotsFull
	}
	if owner.Valid {
		return nil
	}
	if subID <= 0 {
		return nil
	} // Creation checks and claims in the same outer transaction.
	_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_seats SET subscription_id=$2,user_id=$3 WHERE id=$1`, id, subID, userID)
	return err
}

// Called before taking the subscription row lock in create/renewal. The parent
// transaction also owns purchases/redeems, so a full group cannot charge first.
func (s *DynamicSubscriptionService) prepareFixedAssignment(ctx context.Context, groupID, subID, userID int64, debug bool) (*DynamicGroupPolicy, *DynamicQuotaPoolState, error) {
	if s == nil || s.disabled {
		return nil, nil, nil
	}
	tx := dbent.TxFromContext(ctx)
	if tx == nil {
		return nil, nil, ErrDynamicQuotaUnavailable
	}
	if debug {
		var admin bool
		if err := scanDynamicSeat(ctx, tx, `SELECT role='admin' FROM users WHERE id=$1`, []any{userID}, &admin); err != nil {
			return nil, nil, err
		}
		if admin {
			// Serialize creation with the group's first binding too. Otherwise a
			// concurrent group save could miss this uncommitted debug subscription.
			_, err := tx.ExecContext(ctx, `SELECT id FROM groups WHERE id=$1 FOR UPDATE`, groupID)
			return nil, nil, err
		}
	}
	group := &DynamicGroupPolicy{GroupID: groupID}
	err := scanDynamicSeat(ctx, tx, `SELECT account_id,enabled,fixed_slots FROM dynamic_group_policies WHERE group_id=$1`, []any{groupID}, &group.AccountID, &group.Enabled, &group.FixedSlots)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !group.Enabled || group.FixedSlots == 0 {
		// Serialize creation with first opt-in without taking a source lock
		// after a group lock. A changed policy must be retried instead.
		if _, err = tx.ExecContext(ctx, `SELECT id FROM groups WHERE id=$1 FOR UPDATE`, groupID); err != nil {
			return nil, nil, err
		}
		var changed bool
		if err = scanDynamicSeat(ctx, tx, `SELECT EXISTS(SELECT 1 FROM dynamic_group_policies WHERE group_id=$1 AND enabled AND fixed_slots>0)`, []any{groupID}, &changed); err != nil {
			return nil, nil, err
		}
		if changed {
			return nil, nil, ErrDynamicQuotaChanged
		}
		return nil, nil, nil
	}
	var raw []byte
	if err = scanDynamicSeat(ctx, tx, `SELECT state FROM dynamic_quota_pools WHERE account_id=$1 FOR UPDATE`, []any{group.AccountID}, &raw); err != nil {
		return nil, nil, err
	}
	p := &DynamicQuotaPoolState{}
	if err = json.Unmarshal(raw, p); err != nil {
		return nil, nil, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT id FROM groups WHERE id=$1 FOR UPDATE`, groupID); err != nil {
		return nil, nil, err
	}
	// Recheck the policy after waiting on a concurrent group save.
	if err = scanDynamicSeat(ctx, tx, `SELECT account_id,enabled,fixed_slots FROM dynamic_group_policies WHERE group_id=$1`, []any{groupID}, &group.AccountID, &group.Enabled, &group.FixedSlots); err != nil {
		return nil, nil, err
	}
	if err = claimFixedSeat(ctx, tx, p, group, subID, userID); err != nil {
		return nil, nil, err
	}
	return group, p, nil
}

// Materialize all groups, including vacancies, before publishing an allocation.
// Overbooked members cannot block a source reset; admission denies them instead.
func (s *DynamicSubscriptionService) syncFixedSeatsTx(ctx context.Context, tx *sql.Tx, accountID int64, p *DynamicQuotaPoolState) error {
	rows, err := tx.QueryContext(ctx, `SELECT group_id FROM dynamic_group_policies WHERE account_id=$1 AND enabled AND fixed_slots>0 ORDER BY group_id`, accountID)
	if err != nil {
		return err
	}
	var groups []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range groups {
		g, err := loadDynamicGroup(ctx, tx, id)
		if err != nil {
			return err
		}
		p.enableFixedSeats()
		if err = ensureFixedSeats(ctx, tx, p, g); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT us.id,us.user_id FROM user_subscriptions us JOIN users u ON u.id=us.user_id
 WHERE us.group_id=$1 AND us.deleted_at IS NULL AND us.status='active' AND us.expires_at>NOW()
 AND u.deleted_at IS NULL AND u.status='active' AND NOT(us.admin_debug AND u.role='admin') ORDER BY us.id`, id)
		if err != nil {
			return err
		}
		var members [][2]int64
		for rows.Next() {
			var m [2]int64
			if err = rows.Scan(&m[0], &m[1]); err != nil {
				rows.Close()
				return err
			}
			members = append(members, m)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, m := range members {
			if err = claimFixedSeat(ctx, tx, p, g, m[0], m[1]); err != nil && !errors.Is(err, ErrDynamicQuotaSlotsFull) && !errors.Is(err, ErrDynamicQuotaSeatRetained) {
				return err
			}
		}
	}
	return nil
}

type fixedQuotaMember struct {
	seatID, subID                                   int64
	used, usedUSD, held, cap, rate, previous, share float64
	policyShare                                     float64
	allocated, active                               bool
}

func (s *DynamicSubscriptionService) reallocateFixedSeats(ctx context.Context, tx *sql.Tx, accountID int64, p *DynamicQuotaPoolState, now time.Time, seatChange bool) error {
	if !p.trustedSnapshot(now) || (p.Status != "active" && p.Status != "learning") {
		return nil
	}
	var err error
	if p.ceilingPercent, err = loadDynamicNativeCeiling(ctx, tx, accountID); err != nil {
		return err
	}
	total, held, _, _, err := dynamicPoolTotals(ctx, tx, accountID)
	if err != nil {
		return err
	}
	// Include reserved empty seats and retained owners, not just active users.
	rows, err := tx.QueryContext(ctx, `SELECT d.id,d.subscription_id,d.allocated_standard_usd,d.allocated,
 gp.weight,gp.max_limit_usd,COALESCE(r.rate_multiplier,g.rate_multiplier),
 g.peak_rate_enabled,g.peak_start,g.peak_end,g.peak_rate_multiplier,
 COALESCE(q.used_standard_usd,0),CASE WHEN us.admin_debug AND u.role='admin' THEN COALESCE(q.cycle_used_usd,0)
 ELSE GREATEST(COALESCE(q.cycle_used_usd,0),COALESCE(us.weekly_usage_usd,0)) END,
 COALESCE(q.applied_limit_usd,0),COALESCE(q.allocated_standard_usd,0),COALESCE(q.enabled AND NOT q.activation_pending AND us.deleted_at IS NULL
 AND us.status='active' AND us.expires_at>NOW() AND u.deleted_at IS NULL AND u.status='active'
 AND NOT(us.admin_debug AND u.role='admin') AND gp.enabled AND q.account_id=d.account_id
 AND g.deleted_at IS NULL AND g.status='active' AND g.platform='openai' AND g.subscription_type='subscription'
 AND EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=d.account_id AND ag.group_id=d.group_id),false),
 COALESCE((SELECT sum(x.hold_standard_usd) FROM dynamic_quota_requests x WHERE x.account_id=d.account_id
 AND x.owner_subscription_id=d.subscription_id AND x.status IN ('pending','uncertain')
 AND x.operator_absorbed_at IS NULL AND x.review_required_at IS NULL AND x.source_closed_at IS NULL),0)
 FROM dynamic_quota_seats d JOIN dynamic_group_policies gp ON gp.group_id=d.group_id
 JOIN groups g ON g.id=d.group_id LEFT JOIN user_subscriptions us ON us.id=d.subscription_id
 LEFT JOIN users u ON u.id=us.user_id LEFT JOIN dynamic_subscription_policies q ON q.subscription_id=d.subscription_id
 LEFT JOIN user_group_rate_multipliers r ON r.group_id=d.group_id AND r.user_id=d.user_id
 WHERE d.account_id=$1 AND d.cycle=$2 ORDER BY d.id`, accountID, fixedSeatCycle(p))
	if err != nil {
		return err
	}
	var members []fixedQuotaMember
	var inputs []dynamicQuotaV2Member
	for rows.Next() {
		var m fixedQuotaMember
		var sub sql.NullInt64
		var weight float64
		var peak Group
		err = rows.Scan(&m.seatID, &sub, &m.share, &m.allocated, &weight, &m.cap, &m.rate,
			&peak.PeakRateEnabled, &peak.PeakStart, &peak.PeakEnd, &peak.PeakRateMultiplier,
			&m.used, &m.usedUSD, &m.previous, &m.policyShare, &m.active, &m.held)
		if err != nil {
			rows.Close()
			return err
		}
		m.subID = sub.Int64
		m.rate *= peak.PeakMultiplierAt(now)
		if !validDynamicAmount(m.rate) || m.rate <= 0 || !validDynamicAmount(m.cap) || !validDynamicAmount(m.used) || !validDynamicAmount(m.usedUSD) || !validDynamicAmount(m.held) {
			rows.Close()
			return ErrDynamicQuotaUnavailable
		}
		// A new owner adopts the reserved share, never a second startup grant.
		if !m.active {
			m.previous = m.usedUSD + math.Max(0, m.share-m.used)*m.rate
		}
		members = append(members, m)
		inputs = append(inputs, dynamicQuotaV2Member{ID: int64(len(members)), Weight: weight, Used: m.used + m.held,
			Cap: m.used + math.Max(0, m.cap-m.usedUSD)/m.rate})
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	// Old individually configured subscriptions still consume a real share when
	// they share this source; they cannot disappear from the denominator.
	rows, err = tx.QueryContext(ctx, `SELECT q.subscription_id FROM dynamic_subscription_policies q
 WHERE q.account_id=$1 AND `+strings.ReplaceAll(dynamicActiveMemberSQL, "p.", "q.")+`
 AND NOT EXISTS(SELECT 1 FROM dynamic_group_policies gp JOIN user_subscriptions us ON us.group_id=gp.group_id
 WHERE us.id=q.subscription_id AND gp.fixed_slots>0)
 AND NOT EXISTS(SELECT 1 FROM dynamic_quota_seats d
 WHERE d.account_id=q.account_id AND d.cycle=$2 AND d.subscription_id=q.subscription_id) ORDER BY q.subscription_id`, accountID, fixedSeatCycle(p))
	if err != nil {
		return err
	}
	var legacy []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range legacy {
		q, e := loadDynamicSubscription(ctx, tx, id, now)
		if e != nil {
			return e
		}
		if q == nil || q.rate <= 0 {
			return ErrDynamicQuotaUnavailable
		}
		m := fixedQuotaMember{subID: id, used: q.usedStandard, usedUSD: q.UsedUSD, held: q.ReservedUSD / q.rate, cap: q.MaxLimitUSD, rate: q.rate, previous: q.LimitUSD, share: q.allocatedStandard, policyShare: q.allocatedStandard, allocated: true, active: true}
		members = append(members, m)
		inputs = append(inputs, dynamicQuotaV2Member{ID: int64(len(members)), Weight: q.Weight, Used: m.used + m.held, Cap: m.used + math.Max(0, m.cap-m.usedUSD)/m.rate})
	}
	allocations := map[int64]float64{}
	if p.CapacityUSD > 0 {
		if seatChange {
			fair := append([]dynamicQuotaV2Member(nil), inputs...)
			budget := p.Available(now, total, held)
			for i := range fair {
				budget += fair[i].Used
				fair[i].Used = 0
			}
			shares, e := allocateDynamicQuotaV2(fair, budget)
			if e != nil {
				return e
			}
			for _, m := range inputs {
				if m.Used > shares[m.ID]+1e-8 {
					return ErrDynamicQuotaSeatsSpent
				}
			}
		}
		allocations, err = allocateDynamicQuotaV2(inputs, p.Available(now, total, held))
		if err != nil {
			return err
		}
	} else {
		for i, m := range members {
			allocations[int64(i+1)] = math.Max(m.used+m.held, m.used+math.Max(0, dynamicStartupLimit(m.cap)-m.usedUSD)/m.rate)
		}
	}
	node := p.fixedSeatNode(p.Snapshot.UsedPercent)
	fresh := p.Snapshot.Valid(now) && !p.growthFrozen(now)
	advance := fresh && p.CapacityUSD > 0 && node > p.V2.LastNode && p.V2.SampleAt.After(p.LastAllocationAt)
	changed := false
	for i, m := range members {
		allocation := allocations[int64(i+1)]
		limit := QuantizeUsageBillingAmount(math.Min(m.cap, m.usedUSD+math.Max(0, allocation-m.used)*m.rate))
		if limit > m.previous && m.allocated && !seatChange {
			if !advance || limit-m.previous < 20 {
				limit = m.previous
			}
			if len(p.Samples) < 2 {
				limit = math.Min(limit, math.Max(m.previous, dynamicStartupLimit(m.cap))+20)
			}
		}
		if !m.allocated && p.CapacityUSD > 0 && len(p.Samples) < 2 {
			limit = math.Min(limit, dynamicStartupLimit(m.cap)+20)
		}
		if !fresh && limit > m.previous && m.allocated {
			limit = m.previous
		}
		limit = math.Min(limit, m.cap)
		allocation = m.used + math.Max(0, limit-m.usedUSD)/m.rate
		allocation = math.Floor(allocation*1e8) / 1e8
		if m.seatID > 0 && (!m.allocated || math.Abs(allocation-m.share) >= 1e-8) {
			if _, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_seats SET allocated_standard_usd=$2,allocated=true WHERE id=$1 AND (NOT allocated OR allocated_standard_usd IS DISTINCT FROM $2)`, m.seatID, allocation); err != nil {
				return err
			}
		}
		if m.active && (limit != m.previous || math.Abs(allocation-m.policyShare) >= 1e-8) {
			reason := "budget_safety"
			if !m.allocated {
				reason = "initial"
			} else if advance {
				reason = "upstream_node"
			}
			if seatChange {
				reason = "seats"
			}
			if _, err = tx.ExecContext(ctx, `UPDATE dynamic_subscription_policies SET applied_limit_usd=$2,allocated_standard_usd=$3,
 last_change=CASE WHEN applied_limit_usd IS DISTINCT FROM $2 THEN jsonb_build_object('previous_usd',applied_limit_usd,
 'current_usd',$2::numeric,'node',$5::int,'reason',$6::text,'at',NOW()) ELSE last_change END,updated_at=NOW()
 WHERE subscription_id=$1 AND account_id=$4 AND (applied_limit_usd IS DISTINCT FROM $2 OR allocated_standard_usd IS DISTINCT FROM $3)`, m.subID, limit, allocation, accountID, node, reason); err != nil {
				return err
			}
		}
		changed = changed || limit != m.previous
	}
	p.V2.BudgetConflict = false
	if advance {
		p.V2.LastNode = node
		p.LastAllocationAt = now
	}
	if changed || advance {
		_, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_events(account_id,cycle,kind,details) VALUES($1,$2,'allocation_node',jsonb_build_object('node',$3::int,'fixed_seats',true))`, accountID, p.Cycle, node)
	}
	return err
}
