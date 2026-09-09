package service

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"
)

const (
	dynamicQuotaDefaultCeiling = 98.0
	dynamicQuotaSafety         = 0.01 // Additional uncertainty reserve; 98% is not an exact upper-bound SLA.
	dynamicQuotaFreshness      = 10 * time.Minute
	dynamicQuotaConfirmDelay   = 30 * time.Second
	dynamicQuotaResetTolerance = 2 * time.Minute
)

// DynamicQuotaObservation is a fresh, account-identity-checked quota query, not
// a cached account card. A missing field is an error, never a synthetic zero.
type DynamicQuotaObservation struct {
	Identity           string    `json:"identity"`
	UsedPercent        float64   `json:"used_percent"`
	ResetAt            time.Time `json:"reset_at"`
	WindowSeconds      int64     `json:"window_seconds"`
	FetchedAt          time.Time `json:"fetched_at"`
	LocalStandardTotal float64   `json:"local_standard_total"`
}

type DynamicQuotaPoolState struct {
	Settings         DynamicQuotaPoolSettings `json:"-"` // Separate persisted configuration, not overwritten by observations.
	Cycle            int64                    `json:"cycle"`
	StartedAt        time.Time                `json:"started_at"`
	ConfirmedAt      *time.Time               `json:"confirmed_at,omitempty"`
	Snapshot         *DynamicQuotaObservation `json:"snapshot,omitempty"`
	Candidate        *DynamicQuotaObservation `json:"candidate,omitempty"`
	Status           string                   `json:"status"`
	CapacityUSD      float64                  `json:"capacity_usd"`
	Samples          []float64                `json:"samples,omitempty"`
	SampleAnchor     *DynamicQuotaObservation `json:"sample_anchor,omitempty"`
	LastAllocationAt time.Time                `json:"last_allocation_at"`
}

type DynamicQuotaPoolSettings struct {
	Revision            int64   `json:"revision"`
	UsageCeilingPercent float64 `json:"usage_ceiling_percent"`
}

func (p *DynamicQuotaPoolState) stopPercent() float64 {
	ceiling := p.Settings.UsageCeilingPercent
	if ceiling == 0 {
		ceiling = dynamicQuotaDefaultCeiling
	}
	return math.Max(0, ceiling-dynamicQuotaSafety*100)
}

func validDynamicAmount(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func (o DynamicQuotaObservation) Valid(now time.Time) bool {
	return o.Identity != "" && validDynamicAmount(o.UsedPercent) && o.UsedPercent <= 100 &&
		o.WindowSeconds == 7*24*60*60 && !o.FetchedAt.IsZero() &&
		!o.FetchedAt.After(now.Add(5*time.Second)) && now.Sub(o.FetchedAt) <= dynamicQuotaFreshness &&
		!o.ResetAt.IsZero() && o.ResetAt.After(o.FetchedAt) &&
		o.ResetAt.Sub(o.FetchedAt) <= 7*24*time.Hour+dynamicQuotaResetTolerance &&
		validDynamicAmount(o.LocalStandardTotal)
}

// Observe never resets a subscription. It produces a candidate that the store
// can commit once all tracked requests have settled. Repeated observations of
// one cycle do not create another event; the local cycle number is not upstream
// proof. Early resets need both a new period boundary and a coherent usage drop.
func (p *DynamicQuotaPoolState) Observe(o DynamicQuotaObservation, now time.Time) bool {
	if !o.Valid(now) {
		p.Status = "quota_unavailable"
		return false
	}
	if p.Snapshot == nil {
		p.Cycle, p.StartedAt, p.Snapshot, p.SampleAnchor = 1, now, &o, &o
		p.Status = "learning"
		return false // First connection is a baseline, never a reset.
	}
	old := p.Snapshot
	if old.Identity != o.Identity {
		p.Status = "identity_changed"
		p.Candidate = nil
		return false // Replacing credentials is not a quota reset.
	}
	if !o.FetchedAt.After(old.FetchedAt) {
		return false
	}
	boundaryChanged := math.Abs(o.ResetAt.Sub(old.ResetAt).Seconds()) > dynamicQuotaResetTolerance.Seconds()
	dropped := o.UsedPercent < old.UsedPercent-0.5
	if boundaryChanged || dropped || p.Candidate != nil {
		// A due clock, percentage-only drop, extension of a deadline or changed
		// window length are insufficient. No automatic fallback to "+7 days".
		newStart := o.ResetAt.Add(-time.Duration(o.WindowSeconds) * time.Second)
		coherent := boundaryChanged && o.ResetAt.After(old.ResetAt) &&
			newStart.After(old.FetchedAt.Add(-dynamicQuotaResetTolerance)) &&
			!newStart.After(o.FetchedAt.Add(dynamicQuotaResetTolerance)) &&
			((dropped && (o.UsedPercent <= 10 || p.Status == "settling")) || (old.UsedPercent == 0 && o.UsedPercent == 0 && !now.Before(old.ResetAt)))
		if !coherent {
			p.Status = "reset_unconfirmed"
			p.Candidate = nil
			return false
		}
		if c := p.Candidate; c != nil && c.Valid(now) && c.Identity == o.Identity &&
			math.Abs(c.ResetAt.Sub(o.ResetAt).Seconds()) <= dynamicQuotaResetTolerance.Seconds() &&
			(o.FetchedAt.Sub(c.FetchedAt) >= dynamicQuotaConfirmDelay || (p.Status == "settling" && o.FetchedAt.After(c.FetchedAt))) && o.UsedPercent >= c.UsedPercent {
			p.Candidate = &o
			p.Status = "settling"
			return true
		}
		p.Candidate = &o
		p.Status = "confirming"
		return false
	}
	if anchor := p.SampleAnchor; anchor != nil {
		deltaPercent, deltaCost := o.UsedPercent-anchor.UsedPercent, o.LocalStandardTotal-anchor.LocalStandardTotal
		if deltaPercent >= 3 && deltaCost > 0 && o.FetchedAt.Sub(anchor.FetchedAt) >= time.Minute {
			capacity := deltaCost / (deltaPercent / 100)
			if validDynamicAmount(capacity) && capacity > 0 {
				p.Samples = append(p.Samples, capacity)
				if len(p.Samples) > 12 {
					p.Samples = p.Samples[len(p.Samples)-12:]
				}
				sorted := append([]float64(nil), p.Samples...)
				sort.Float64s(sorted)
				// ponytail: lower-quartile recent samples assume a reasonably stable
				// model mix; split by model family if measured prediction error requires it.
				p.CapacityUSD = sorted[(len(sorted)-1)/4] * 0.9
			}
			p.SampleAnchor = &o
		}
	}
	p.Snapshot = &o
	p.Status = "active"
	if p.CapacityUSD <= 0 {
		p.Status = "learning"
	}
	return false
}

func (p *DynamicQuotaPoolState) Confirm(now time.Time) {
	p.Cycle++
	p.StartedAt, p.ConfirmedAt = now, &now
	p.Snapshot, p.SampleAnchor = p.Candidate, p.Candidate
	p.Candidate = nil
	p.LastAllocationAt = time.Time{}
	p.Status = "active"
	if p.CapacityUSD <= 0 {
		p.Status = "learning"
	}
}

func (p *DynamicQuotaPoolState) Available(now time.Time, settledTotal, holds float64) float64 {
	if p.Snapshot == nil || !p.Snapshot.Valid(now) || p.CapacityUSD <= 0 || p.Status != "active" {
		return 0
	}
	remaining := p.CapacityUSD * math.Max(0, (p.stopPercent()-p.Snapshot.UsedPercent)/100)
	return math.Max(0, remaining-math.Max(0, settledTotal-p.Snapshot.LocalStandardTotal)-holds)
}

// Compare with the last applied allowance, not the last estimate. Increases
// wait for their threshold and periodic window; decreases accumulate to $5.
// Physical-cost admission remains independent of this published dollar limit.
func dynamicQuotaAppliedLimit(applied, candidate, threshold float64, allowIncrease, force bool) float64 {
	candidate = QuantizeUsageBillingAmount(candidate)
	delta := QuantizeUsageBillingAmount(candidate - applied)
	if force || delta <= -5 || (allowIncrease && delta >= threshold) {
		return candidate
	}
	return applied
}

type DynamicQuotaMember struct {
	ID                int64
	Weight, Used, Cap float64 // Standard-cost units; role is deliberately absent.
}

// Allocation is cumulative within a cycle: used+remaining is the pool budget,
// not a fresh grant on every scan. 80% protects weighted base shares; the bounded
// remainder is available to members who have actually approached their share.
func allocateDynamicQuota(members []DynamicQuotaMember, remaining float64) map[int64]float64 {
	out := make(map[int64]float64, len(members))
	var weight, used float64
	for _, m := range members {
		weight += m.Weight
		used += m.Used
		out[m.ID] = m.Used
	}
	if weight <= 0 || !validDynamicAmount(remaining) {
		return out
	}
	budget, granted := remaining+used, 0.0
	for _, m := range members {
		base := math.Min(m.Cap, budget*0.8*m.Weight/weight)
		grant := math.Max(0, base-m.Used)
		out[m.ID] += grant
		granted += grant
	}
	if granted > remaining && granted > 0 {
		for _, m := range members {
			out[m.ID] = m.Used + (out[m.ID]-m.Used)*remaining/granted
		}
		return out
	}
	left := remaining - granted
	var demandWeight float64
	for _, m := range members {
		if m.Used >= budget*0.4*m.Weight/weight && m.Cap > out[m.ID] {
			demandWeight += m.Weight
		}
	}
	if demandWeight > 0 {
		for _, m := range members {
			if m.Used >= budget*0.4*m.Weight/weight {
				out[m.ID] += math.Max(0, math.Min(m.Cap-out[m.ID], left*m.Weight/demandWeight))
			}
		}
	}
	return out
}

// Strict decoder is separate from the legacy quota-card projection: float64's
// zero value cannot distinguish a missing used_percent from a genuine zero.
func decodeDynamicQuotaObservation(raw []byte, identity string, now time.Time) (DynamicQuotaObservation, error) {
	var body struct {
		AccountID string `json:"account_id"`
		RateLimit *struct {
			Primary   json.RawMessage `json:"primary_window"`
			Secondary json.RawMessage `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if json.Unmarshal(raw, &body) != nil || body.RateLimit == nil || body.AccountID == "" || body.AccountID != identity {
		return DynamicQuotaObservation{}, errors.New("quota identity or envelope is missing")
	}
	var found *DynamicQuotaObservation
	for _, rawWindow := range []json.RawMessage{body.RateLimit.Primary, body.RateLimit.Secondary} {
		var w struct {
			Used   *float64 `json:"used_percent"`
			Reset  *int64   `json:"reset_at"`
			Window *int64   `json:"limit_window_seconds"`
		}
		if len(rawWindow) == 0 || string(rawWindow) == "null" {
			continue
		}
		if json.Unmarshal(rawWindow, &w) != nil || w.Window == nil {
			return DynamicQuotaObservation{}, errors.New("quota window metadata is missing")
		}
		if *w.Window != 7*24*60*60 {
			continue
		}
		if found != nil || w.Used == nil || w.Reset == nil {
			return DynamicQuotaObservation{}, errors.New("weekly quota is missing or ambiguous")
		}
		o := DynamicQuotaObservation{Identity: identity, UsedPercent: *w.Used, ResetAt: time.Unix(*w.Reset, 0).UTC(), WindowSeconds: *w.Window, FetchedAt: now}
		if !o.Valid(now) {
			return DynamicQuotaObservation{}, errors.New("invalid weekly quota values")
		}
		found = &o
	}
	if found == nil {
		return DynamicQuotaObservation{}, errors.New("explicit weekly quota is unavailable")
	}
	return *found, nil
}
