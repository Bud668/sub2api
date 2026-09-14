package service

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

const (
	dynamicQuotaFreshness      = 10 * time.Minute
	dynamicQuotaConfirmDelay   = 30 * time.Second
	dynamicQuotaResetTolerance = 2 * time.Minute
	dynamicQuotaGuardChecks    = 3
)

type dynamicQuotaHealth struct {
	Failures       int       `json:"failures"`
	Recoveries     int       `json:"recoveries"`
	LastAttemptAt  time.Time `json:"last_attempt_at"`
	LastFailureAt  time.Time `json:"last_failure_at"`
	LastRecoveryAt time.Time `json:"last_recovery_at"`
}

// DynamicQuotaObservation is a fresh, account-identity-checked quota query, not
// a cached account card. A missing field is an error, never a synthetic zero.
type DynamicQuotaObservation struct {
	Identity           string    `json:"identity"`
	UsedPercent        float64   `json:"used_percent"`
	ResetAt            time.Time `json:"reset_at"`
	WindowSeconds      int64     `json:"window_seconds"`
	FetchedAt          time.Time `json:"fetched_at"`
	LocalStandardTotal float64   `json:"local_standard_total"`
	WindowCostUSD      float64   `json:"window_cost_usd,omitempty"`
	WindowStandardUSD  float64   `json:"window_standard_usd,omitempty"`
}

type DynamicQuotaPoolState struct {
	V2                 *DynamicQuotaV2State     `json:"v2,omitempty"`
	ModelMaxRequestUSD map[string]float64       `json:"model_max_request_usd,omitempty"`
	ceilingPercent     float64                  // Read from native account/global 7d auto-pause settings; never persisted here.
	Cycle              int64                    `json:"cycle"`
	StartedAt          time.Time                `json:"started_at"`
	ConfirmedAt        *time.Time               `json:"confirmed_at,omitempty"`
	Snapshot           *DynamicQuotaObservation `json:"snapshot,omitempty"`
	Candidate          *DynamicQuotaObservation `json:"candidate,omitempty"`
	Status             string                   `json:"status"`
	CapacityUSD        float64                  `json:"capacity_usd"`
	LastAllocationAt   time.Time                `json:"last_allocation_at"`
	Health             dynamicQuotaHealth       `json:"health,omitempty"`
	GuardSignal        string                   `json:"guard_signal,omitempty"`
}

func dynamicQuotaModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || len(model) > 128 {
		return ""
	}
	return model
}

func (p *DynamicQuotaPoolState) requestMaxUSD(model string, fallback float64) float64 {
	model = dynamicQuotaModel(model)
	if value := p.ModelMaxRequestUSD[model]; model != "" && value > 0 && validDynamicAmount(value) {
		return value
	}
	return fallback
}

func (p *DynamicQuotaPoolState) observeRequestCost(model string, cost float64) bool {
	model = dynamicQuotaModel(model)
	if model == "" || cost <= 0 || !validDynamicAmount(cost) || p.ModelMaxRequestUSD[model] >= cost {
		return false
	}
	if p.ModelMaxRequestUSD == nil {
		p.ModelMaxRequestUSD = make(map[string]float64)
	}
	p.ModelMaxRequestUSD[model] = cost
	return true
}

func (p *DynamicQuotaPoolState) growthFrozen(now time.Time) bool {
	return p.growthFreezeReason(now) != ""
}

func (p *DynamicQuotaPoolState) growthFreezeReason(now time.Time) string {
	if p.Health.Failures > 0 || p.Snapshot == nil || !p.Snapshot.Valid(now) {
		return "sync_recovery"
	}
	if p.V2 != nil && p.V2.CandidateSamples > 0 {
		return "estimate_anomaly"
	}
	return ""
}

// A delayed quota query may spend the last verified budget in the same window.
// Freshness is still required for growth/approval; identity and reset conflicts
// still block admission. Available deducts all later settlements and holds.
func (p *DynamicQuotaPoolState) trustedSnapshot(now time.Time) bool {
	return p.Snapshot != nil && p.Snapshot.Valid(p.Snapshot.FetchedAt) &&
		!p.Snapshot.FetchedAt.After(now.Add(5*time.Second)) && now.Before(p.Snapshot.ResetAt)
}

func (p *DynamicQuotaPoolState) accessStatus(now time.Time) string {
	if !p.trustedSnapshot(now) {
		return "quota_unavailable"
	}
	return p.Status
}

func (p *DynamicQuotaPoolState) recordFailure(at time.Time) {
	if (p.Snapshot != nil && !at.After(p.Snapshot.FetchedAt)) || !at.After(p.Health.LastAttemptAt) {
		return // A late failed query cannot invalidate a newer verified result.
	}
	p.Health.LastAttemptAt, p.Health.Recoveries = at, 0
	if p.Health.Failures == 0 || at.Sub(p.Health.LastFailureAt) >= dynamicQuotaConfirmDelay {
		p.Health.Failures++
		p.Health.LastFailureAt = at
	}
}

func (p *DynamicQuotaPoolState) recordHealthy(at time.Time) {
	if !at.After(p.Health.LastAttemptAt) {
		return
	}
	p.Health.LastAttemptAt = at
	if p.Health.Failures > 0 && (p.Health.Recoveries == 0 || at.Sub(p.Health.LastRecoveryAt) >= dynamicQuotaConfirmDelay) {
		p.Health.Recoveries++
		p.Health.LastRecoveryAt = at
		if p.Health.Recoveries >= dynamicQuotaGuardChecks {
			p.Health.Failures, p.Health.Recoveries = 0, 0
		}
	}
}

func (p *DynamicQuotaPoolState) stopPercent() float64 {
	if p.ceilingPercent > 0 {
		return p.ceilingPercent
	}
	return 100 // Native auto-pause unset/disabled: no separate percentage reserve.
}

func validDynamicAmount(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func (o DynamicQuotaObservation) hasWindowEstimate() bool {
	return o.UsedPercent >= 2 && o.WindowCostUSD > 0 && o.WindowStandardUSD > 0
}

func (o DynamicQuotaObservation) Valid(now time.Time) bool {
	return o.Identity != "" && validDynamicAmount(o.UsedPercent) && o.UsedPercent <= 100 &&
		o.WindowSeconds == 7*24*60*60 && !o.FetchedAt.IsZero() &&
		!o.FetchedAt.After(now.Add(5*time.Second)) && now.Sub(o.FetchedAt) <= dynamicQuotaFreshness &&
		!o.ResetAt.IsZero() && o.ResetAt.After(o.FetchedAt) &&
		o.ResetAt.Sub(o.FetchedAt) <= 7*24*time.Hour+dynamicQuotaResetTolerance &&
		validDynamicAmount(o.LocalStandardTotal) && validDynamicAmount(o.WindowCostUSD) && validDynamicAmount(o.WindowStandardUSD)
}

// Observe never resets a subscription. It produces a candidate that the store
// can commit once all tracked requests have settled. Repeated observations of
// one cycle do not create another event; the local cycle number is not upstream
// proof. Early resets need both a new period boundary and a coherent usage drop.
func (p *DynamicQuotaPoolState) Observe(o DynamicQuotaObservation, now time.Time) bool {
	if p.V2 == nil {
		p.startV2() // Initialize learning metadata only; never regrant a legacy policy.
	}
	if !o.Valid(now) {
		p.Status = "quota_unavailable"
		return false
	}
	if p.Snapshot == nil {
		p.Cycle, p.StartedAt, p.Snapshot = 1, now, &o
		if p.V2 != nil {
			p.V2.LastNode = dynamicQuotaNode(o.UsedPercent)
		}
		p.Status = "learning"
		p.observeWindowEstimate(o)
		if p.CapacityUSD > 0 {
			p.Status = "active"
		}
		p.recordHealthy(o.FetchedAt)
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
	if now.Sub(old.FetchedAt) > dynamicQuotaFreshness {
		p.Health.Failures = max(p.Health.Failures, dynamicQuotaGuardChecks)
		p.Health.Recoveries = 0 // Old recovery observations cannot bridge another stale gap.
	}
	boundaryChanged := math.Abs(o.ResetAt.Sub(old.ResetAt).Seconds()) > dynamicQuotaResetTolerance.Seconds()
	// An unused rolling window moves with the query time. Refresh its baseline
	// after a polling gap only when neither upstream nor local usage advanced.
	if boundaryChanged && p.Candidate == nil && now.Before(old.ResetAt) &&
		old.UsedPercent == 0 && o.UsedPercent == 0 && old.LocalStandardTotal == o.LocalStandardTotal &&
		math.Abs(o.ResetAt.Sub(old.ResetAt).Seconds()-o.FetchedAt.Sub(old.FetchedAt).Seconds()) <= dynamicQuotaResetTolerance.Seconds() {
		boundaryChanged = false
	}
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
		p.recordHealthy(o.FetchedAt)
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
	p.recordHealthy(o.FetchedAt)
	p.observeWindowEstimate(o)
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
	p.Snapshot = p.Candidate
	p.Candidate = nil
	p.LastAllocationAt = time.Time{}
	p.CapacityUSD = 0
	p.startV2()
	if p.Snapshot != nil {
		p.observeWindowEstimate(*p.Snapshot)
	}
	p.Status = "active"
	if p.CapacityUSD <= 0 {
		p.Status = "learning"
	}
}

func (p *DynamicQuotaPoolState) Available(now time.Time, settledTotal, holds float64) float64 {
	if p.CapacityUSD <= 0 || p.accessStatus(now) != "active" {
		return 0
	}
	remaining := p.CapacityUSD * math.Max(0, (p.stopPercent()-p.Snapshot.UsedPercent)/100)
	return math.Max(0, remaining-math.Max(0, settledTotal-p.Snapshot.LocalStandardTotal)-holds)
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
