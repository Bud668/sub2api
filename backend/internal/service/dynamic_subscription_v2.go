package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"
)

// Persisted learning/node metadata. V2 is the only allocation algorithm.
type DynamicQuotaV2State struct {
	LastNode           int       `json:"last_node"`
	SampleAt           time.Time `json:"sample_at"`
	CandidateUSD       float64   `json:"candidate_usd,omitempty"`
	CandidateSamples   int       `json:"candidate_samples,omitempty"`
	BudgetConflict     bool      `json:"budget_conflict,omitempty"`
	UnreservedCapacity bool      `json:"unreserved_capacity,omitempty"`
}

type DynamicQuotaChange struct {
	PreviousUSD float64   `json:"previous_usd"`
	CurrentUSD  float64   `json:"current_usd"`
	Node        int       `json:"node"`
	Reason      string    `json:"reason"`
	At          time.Time `json:"at"`
}

func dynamicQuotaNode(percent float64) int {
	return int(math.Floor(percent / 10))
}

func (p *DynamicQuotaPoolState) startV2() {
	p.V2 = &DynamicQuotaV2State{UnreservedCapacity: true}
	if p.Snapshot != nil {
		p.V2.LastNode = dynamicQuotaNode(p.Snapshot.UsedPercent)
	}
	p.SampleAnchor = p.Snapshot
	p.Samples = nil
	p.LastAllocationAt = time.Time{}
	if p.Status == "active" && p.CapacityUSD == 0 {
		p.Status = "learning"
	}
}

// Older V2 evidence included a 0.9 factor. Convert the whole evidence set once,
// preserving its ratios, freshness and guard state. The normal locked write
// persists the marker; read-only hydration never changes grants or the ledger.
func (p *DynamicQuotaPoolState) restoreUnreservedCapacity() error {
	if p.V2 == nil || p.V2.UnreservedCapacity {
		return nil
	}
	p.CapacityUSD /= 0.9
	p.V2.CandidateUSD /= 0.9
	if !validDynamicAmount(p.CapacityUSD) || !validDynamicAmount(p.V2.CandidateUSD) {
		return ErrDynamicQuotaUnavailable
	}
	for i := range p.Samples {
		p.Samples[i] /= 0.9
		if !validDynamicAmount(p.Samples[i]) {
			return ErrDynamicQuotaUnavailable
		}
	}
	p.V2.UnreservedCapacity = true
	return nil
}

// Only a new, non-overlapping consumption interval is learning evidence.
// Reading one cached percentage repeatedly cannot approve a capacity increase.
func (p *DynamicQuotaPoolState) observeV2Capacity(o DynamicQuotaObservation) {
	anchor := p.SampleAnchor
	if anchor == nil {
		p.SampleAnchor = &o
		return
	}
	delta := o.UsedPercent - anchor.UsedPercent
	if delta < 10 || o.FetchedAt.Sub(anchor.FetchedAt) < time.Minute {
		return
	}
	p.SampleAnchor = &o
	spent := o.LocalStandardTotal - anchor.LocalStandardTotal
	if !validDynamicAmount(spent) || spent <= 0 {
		return
	}
	sample := spent / (delta / 100)
	if !validDynamicAmount(sample) || sample <= 0 {
		return
	}
	v := p.V2
	if p.CapacityUSD > 0 && (sample < p.CapacityUSD*0.8 || sample > p.CapacityUSD*1.2) {
		if v.CandidateSamples == 0 || math.Abs(sample-v.CandidateUSD) > v.CandidateUSD*0.05 {
			v.CandidateUSD, v.CandidateSamples = sample, 1
		} else {
			v.CandidateUSD = math.Min(v.CandidateUSD, sample)
			v.CandidateSamples++
		}
		if v.CandidateSamples < dynamicQuotaGuardChecks {
			return // Retain the last verified capacity and every published allowance.
		}
		sample = v.CandidateUSD
		p.Samples = nil // Confirmed capacity changes must not mix incompatible samples.
	}
	v.CandidateUSD, v.CandidateSamples = 0, 0
	p.Samples = append(p.Samples, sample)
	if len(p.Samples) > 3 {
		p.Samples = p.Samples[len(p.Samples)-3:]
	}
	sorted := append([]float64(nil), p.Samples...)
	sort.Float64s(sorted)
	p.CapacityUSD = sorted[(len(sorted)-1)/2]
	v.SampleAt = o.FetchedAt
}

func (p *DynamicQuotaPoolState) v2AllocationDue(now time.Time) bool {
	return p.V2 != nil && p.Snapshot != nil && p.Snapshot.Valid(now) &&
		p.Status == "active" && !p.growthFrozen(now) && p.CapacityUSD > 0 &&
		dynamicQuotaNode(p.Snapshot.UsedPercent) > p.V2.LastNode &&
		p.V2.SampleAt.After(p.LastAllocationAt)
}

type dynamicQuotaV2Member struct {
	ID                       int64
	Weight, Used, Floor, Cap float64 // Standard-cost units, including already-spent usage.
}

var errDynamicQuotaV2Budget = errors.New("downward protection exceeds the verified allocation budget")

// Allocate cumulative weighted shares between the operator's bounds. Prior
// usage is never refunded, and capped shares are redistributed automatically.
// A protection/budget conflict returns no grants; callers retain published
// allowances while the independent upstream budget still gates new requests.
func allocateDynamicQuotaV2(members []dynamicQuotaV2Member, remaining float64) (map[int64]float64, error) {
	if !validDynamicAmount(remaining) {
		return nil, ErrDynamicQuotaUnavailable
	}
	seen := make(map[int64]bool, len(members))
	minimum, upper := 0.0, 0.0
	for _, m := range members {
		if m.ID <= 0 || seen[m.ID] || !validDynamicAmount(m.Weight) || m.Weight <= 0 ||
			!validDynamicAmount(m.Used) || !validDynamicAmount(m.Floor) || !validDynamicAmount(m.Cap) || m.Floor > m.Cap {
			return nil, ErrDynamicQuotaUnavailable
		}
		seen[m.ID] = true
		minimum += math.Max(0, m.Floor-m.Used)
		upper = math.Max(upper, m.Cap/m.Weight)
	}
	if !validDynamicAmount(minimum) || !validDynamicAmount(upper) {
		return nil, ErrDynamicQuotaUnavailable
	}
	if minimum > remaining {
		return nil, errDynamicQuotaV2Budget
	}
	target := func(m dynamicQuotaV2Member, level float64) float64 {
		return math.Max(m.Used, math.Min(m.Cap, math.Max(m.Floor, level*m.Weight)))
	}
	// ponytail: bounded monotone bisection, O(64*n); use sorted breakpoints only
	// if measured source membership makes this allocation-time work significant.
	lower := 0.0
	for range 64 {
		mid, required := lower+(upper-lower)/2, 0.0
		for _, m := range members {
			required += target(m, mid) - m.Used
		}
		if required <= remaining {
			lower = mid
		} else {
			upper = mid
		}
	}
	out := make(map[int64]float64, len(members))
	for _, m := range members {
		out[m.ID] = target(m, lower)
	}
	return out, nil
}

func (s *DynamicSubscriptionService) reallocateV2(ctx context.Context, tx *sql.Tx, accountID int64, p *DynamicQuotaPoolState, now time.Time) error {
	if !p.v2AllocationDue(now) {
		return nil
	}
	var err error
	if p.ceilingPercent, err = loadDynamicNativeCeiling(ctx, tx, accountID); err != nil {
		return err
	}
	if p.Snapshot.UsedPercent >= p.stopPercent() {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.subscription_id FROM dynamic_subscription_policies p
 WHERE p.account_id=$1 AND `+dynamicActiveMemberSQL+` ORDER BY p.subscription_id`, accountID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	var members []dynamicQuotaV2Member
	policies := make(map[int64]*DynamicSubscriptionQuota, len(ids))
	for _, id := range ids {
		q, err := loadDynamicSubscription(ctx, tx, id, now)
		if err != nil {
			return err
		}
		if q == nil || q.FloorLimitUSD == nil || q.rate <= 0 {
			return ErrDynamicQuotaUnavailable
		}
		policies[id] = q
		members = append(members, dynamicQuotaV2Member{ID: id, Weight: q.Weight, Used: q.usedStandard,
			Floor: q.usedStandard + math.Max(0, *q.FloorLimitUSD-q.UsedUSD)/q.rate,
			Cap:   q.usedStandard + math.Max(0, q.MaxLimitUSD-q.UsedUSD)/q.rate})
	}
	total, held, _, _, err := dynamicPoolTotals(ctx, tx, accountID)
	if err != nil {
		return err
	}
	allocations, err := allocateDynamicQuotaV2(members, p.Available(now, total, held))
	if errors.Is(err, errDynamicQuotaV2Budget) {
		if !p.V2.BudgetConflict {
			if err := recordDynamicGuardEvent(ctx, tx, accountID, p, "allocation_budget_conflict"); err != nil {
				return err
			}
		}
		p.V2.BudgetConflict = true
		return nil // This node remains retryable, never an implicit protection cut.
	}
	if err != nil {
		return err
	}
	changes := make(map[int64][2]float64)
	for _, m := range members {
		q := policies[m.ID]
		allocation := allocations[m.ID]
		// Round grants down in standard units so publishing all members cannot
		// create extra upstream budget from independently rounded currencies.
		grant := math.Floor(math.Max(0, allocation-m.Used)*1e8) / 1e8
		limit := QuantizeUsageBillingAmount(math.Min(q.MaxLimitUSD, math.Max(*q.FloorLimitUSD, q.UsedUSD+grant*q.rate)))
		allocation = m.Used + math.Min(grant, math.Max(0, limit-q.UsedUSD)/q.rate)
		if _, err = tx.ExecContext(ctx, `UPDATE dynamic_subscription_policies SET applied_limit_usd=$2,
 last_change=CASE WHEN applied_limit_usd IS DISTINCT FROM $2 THEN jsonb_build_object('previous_usd',applied_limit_usd,
 'current_usd',$2::numeric,'node',$5::int,'reason','upstream_node','at',NOW()) ELSE last_change END,
 allocated_standard_usd=$3,updated_at=NOW() WHERE subscription_id=$1 AND account_id=$4`, m.ID, limit, allocation, accountID, dynamicQuotaNode(p.Snapshot.UsedPercent)*10); err != nil {
			return err
		}
		if limit != q.LimitUSD {
			changes[m.ID] = [2]float64{q.LimitUSD, limit}
		}
	}
	p.V2.LastNode = dynamicQuotaNode(p.Snapshot.UsedPercent)
	p.V2.BudgetConflict = false
	p.LastAllocationAt = now
	details, err := json.Marshal(map[string]any{"node": p.V2.LastNode * 10, "changes": changes})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_events(account_id,cycle,kind,details)
 VALUES($1,$2,'allocation_node',$3::jsonb)`, accountID, p.Cycle, string(details))
	return err
}

// The source lock serializes this handoff with Begin, billing and reset. No
// request is reassigned: old holds keep their fixed owner/source/cycle, and
// settlement updates that owner's durable V2 consumption once via billing dedup.
func activateDynamicV2(ctx context.Context, tx *sql.Tx, accountID int64, reset bool, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT p.subscription_id FROM dynamic_subscription_policies p
 WHERE p.account_id=$1 AND p.enabled AND (p.activation_pending OR $2)
 AND `+dynamicEligibleMemberSQL+` ORDER BY p.subscription_id`, accountID, reset)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range ids {
		q, err := loadDynamicSubscription(ctx, tx, id, now)
		if err != nil {
			return err
		}
		if q == nil || q.rate <= 0 {
			return ErrDynamicQuotaUnavailable
		}
		limit := q.LimitUSD
		if reset {
			limit = q.MaxLimitUSD
		}
		_, err = tx.ExecContext(ctx, `UPDATE dynamic_subscription_policies SET activation_pending=false,
 last_change=CASE WHEN activation_pending AND NOT $5 THEN jsonb_build_object('previous_usd',0,
 'current_usd',$2::numeric,'reason','initial','at',NOW()) ELSE last_change END,
 applied_limit_usd=$2,allocated_standard_usd=used_standard_usd+GREATEST(0,$2::numeric-$3::numeric)/$4::numeric,
 cycle_used_usd=GREATEST(cycle_used_usd,$3),updated_at=NOW() WHERE subscription_id=$1`, id, limit, q.UsedUSD, q.rate, reset)
		if err != nil {
			return err
		}
	}
	return nil
}
