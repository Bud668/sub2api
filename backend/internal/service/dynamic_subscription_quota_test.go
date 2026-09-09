package service

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestDynamicQuotaConfirmation(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	old := DynamicQuotaObservation{Identity: "account-a", UsedPercent: 80, ResetAt: now.Add(5 * 24 * time.Hour), WindowSeconds: 604800, FetchedAt: now}
	newObservation := old
	newObservation.FetchedAt = now.Add(time.Minute)
	newObservation.ResetAt = newObservation.FetchedAt.Add(7 * 24 * time.Hour)
	newObservation.UsedPercent = 0
	for _, tc := range []struct {
		name   string
		change func(*DynamicQuotaObservation)
		want   string
	}{
		{"percentage-only", func(o *DynamicQuotaObservation) { o.ResetAt = old.ResetAt }, "reset_unconfirmed"},
		{"identity", func(o *DynamicQuotaObservation) { o.Identity = "account-b" }, "identity_changed"},
		{"deadline-only", func(o *DynamicQuotaObservation) { o.UsedPercent = 80 }, "reset_unconfirmed"},
		{"five-hour", func(o *DynamicQuotaObservation) { o.WindowSeconds = 18000 }, "quota_unavailable"},
		{"nan", func(o *DynamicQuotaObservation) { o.UsedPercent = math.NaN() }, "quota_unavailable"},
		{"missing-time", func(o *DynamicQuotaObservation) { o.ResetAt = time.Time{} }, "quota_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DynamicQuotaPoolState{}
			if p.Observe(old, now) {
				t.Fatal("baseline reset")
			}
			o := newObservation
			tc.change(&o)
			if p.Observe(o, o.FetchedAt) || p.Cycle != 1 || p.Status != tc.want {
				t.Fatalf("false reset/status: %+v", p)
			}
		})
	}
	p := DynamicQuotaPoolState{}
	if p.Observe(old, now) {
		t.Fatal("baseline reset")
	}
	if p.Observe(newObservation, newObservation.FetchedAt) || p.Status != "confirming" {
		t.Fatal("single sample confirmed")
	}
	if p.Observe(newObservation, newObservation.FetchedAt) || p.Cycle != 1 {
		t.Fatal("cached sample confirmed")
	}
	second := newObservation
	second.FetchedAt = second.FetchedAt.Add(time.Minute)
	if !p.Observe(second, second.FetchedAt) || p.Cycle != 1 {
		t.Fatal("early reset not confirmed, or committed before settlement")
	}
	p.Confirm(second.FetchedAt)
	if p.Cycle != 2 || p.ConfirmedAt == nil {
		t.Fatal("cycle not advanced")
	}
	third := second
	third.FetchedAt = third.FetchedAt.Add(time.Minute)
	if p.Observe(third, third.FetchedAt) || p.Cycle != 2 {
		t.Fatal("same reset counted twice")
	}
	// A second genuine early reset within the same calendar week is independent.
	third.UsedPercent = 60
	third.FetchedAt = third.FetchedAt.Add(time.Hour)
	p.Observe(third, third.FetchedAt)
	fourth := third
	fourth.FetchedAt = fourth.FetchedAt.Add(time.Minute)
	fourth.UsedPercent = 0
	fourth.ResetAt = fourth.FetchedAt.Add(7 * 24 * time.Hour)
	if p.Observe(fourth, fourth.FetchedAt) {
		t.Fatal("second reset confirmed with one sample")
	}
	fourth.FetchedAt = fourth.FetchedAt.Add(time.Minute)
	if !p.Observe(fourth, fourth.FetchedAt) {
		t.Fatal("second early reset lost to calendar-week dedup")
	}
	// Persistence round-trip retains the pending event, not a timer-derived reset.
	raw, _ := json.Marshal(p)
	var restored DynamicQuotaPoolState
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Confirm(fourth.FetchedAt)
	if restored.Cycle != 3 {
		t.Fatal("restart lost cycle")
	}
}

func TestDynamicQuotaAllocation(t *testing.T) {
	// Two equally weighted active subscriptions, including an administrator.
	members := []DynamicQuotaMember{{ID: 1, Weight: 1, Cap: 1000}, {ID: 2, Weight: 1, Cap: 1000}}
	a := allocateDynamicQuota(members, 100)
	if a[1] != 40 || a[2] != 40 {
		t.Fatalf("role-neutral base allocation: %v", a)
	}
	members[0].Used = 35
	b := allocateDynamicQuota(members, 65)
	if b[1] != 60 || b[2] != 40 {
		t.Fatalf("bounded surplus: %v", b)
	}
	// A subsequent scan cannot create another grant: consumption reduces remainder.
	members[0].Used = 45
	c := allocateDynamicQuota(members, 55)
	if c[1] != 60 || c[2] != 40 {
		t.Fatalf("periodic scan refilled spent money: %v", c)
	}
	members[0].Cap = 50
	d := allocateDynamicQuota(members, 55)
	if d[1] > 50 || d[2] != 40 {
		t.Fatalf("individual cap lost: %v", d)
	}
	for _, remaining := range []float64{0, 1, 10, 100, 1000} {
		allocated := allocateDynamicQuota(members, remaining)
		var extra float64
		for _, m := range members {
			if allocated[m.ID] < m.Used {
				t.Fatal("spent money clawed back")
			}
			extra += allocated[m.ID] - m.Used
		}
		if extra > remaining+1e-8 {
			t.Fatalf("overcommitted %v with %v", extra, remaining)
		}
	}
}

func TestDynamicQuotaStrictObservation(t *testing.T) {
	now := time.Unix(1788912000, 0).UTC()
	valid := map[string]any{"account_id": "a", "rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 0, "reset_at": now.Add(7 * 24 * time.Hour).Unix(), "limit_window_seconds": 604800}}}
	raw, _ := json.Marshal(valid)
	if _, err := decodeDynamicQuotaObservation(raw, "a", now); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeDynamicQuotaObservation(raw, "b", now); err == nil {
		t.Fatal("wrong upstream identity accepted")
	}
	delete(valid["rate_limit"].(map[string]any)["primary_window"].(map[string]any), "used_percent")
	raw, _ = json.Marshal(valid)
	if _, err := decodeDynamicQuotaObservation(raw, "a", now); err == nil {
		t.Fatal("missing percentage became zero")
	}
	for _, raw := range []string{`{}`, `{"account_id":"a","rate_limit":{"primary_window":null}}`, `{"account_id":"a","rate_limit":{"primary_window":{"used_percent":0}}}`} {
		if _, err := decodeDynamicQuotaObservation([]byte(raw), "a", now); err == nil {
			t.Fatal("incomplete payload accepted")
		}
	}
}

func TestDynamicQuotaEstimateAndReserve(t *testing.T) {
	now := time.Now().UTC()
	o := DynamicQuotaObservation{Identity: "a", UsedPercent: 40, ResetAt: now.Add(time.Hour), WindowSeconds: 604800, FetchedAt: now, LocalStandardTotal: 100}
	p := DynamicQuotaPoolState{}
	p.Observe(o, now)
	o.FetchedAt = now.Add(time.Minute)
	o.UsedPercent = 50
	o.LocalStandardTotal = 200
	p.Observe(o, o.FetchedAt)
	if math.Abs(p.CapacityUSD-900) > 1e-8 {
		t.Fatalf("bad estimate %v", p.CapacityUSD)
	}
	if got := p.Available(o.FetchedAt, 220, 3); math.Abs(got-400) > 1e-8 {
		t.Fatalf("unreported/in-flight costs not reserved: %v", got)
	}
	if p.Available(o.FetchedAt.Add(11*time.Minute), 220, 3) != 0 {
		t.Fatal("stale quota increased availability")
	}
	if p.Cycle != 1 || p.ConfirmedAt != nil {
		t.Fatal("capacity estimate triggered reset")
	}
}

func TestDynamicQuotaAsymmetricAdjustmentThresholds(t *testing.T) {
	for _, threshold := range []float64{5, 10} {
		applied := 100.03
		for _, delta := range []float64{0.1, 1, threshold - 0.01, threshold} {
			got := dynamicQuotaAppliedLimit(applied, 100.03+delta, threshold, true, false)
			if delta < threshold && got != applied {
				t.Fatalf("small increase moved the anchor: threshold=%v delta=%v got=%v", threshold, delta, got)
			}
			if delta == threshold && got != 100.03+delta {
				t.Fatal("cumulative increase did not reach the original applied anchor")
			}
		}
		for _, delta := range []float64{0.00000001, 0.1, 1, 4.99} {
			if got := dynamicQuotaAppliedLimit(applied, applied-delta, threshold, false, false); got != applied {
				t.Fatal("small decrease moved the applied anchor before accumulating to $5")
			}
		}
		if got := dynamicQuotaAppliedLimit(applied, applied-5, threshold, false, false); got != applied-5 {
			t.Fatal("cumulative $5 decrease waited for the increase timer")
		}
		if got := dynamicQuotaAppliedLimit(applied, applied-0.1, threshold, false, true); math.Abs(got-(applied-0.1)) > 1e-8 {
			t.Fatal("manual allocation change or reset was held by the display threshold")
		}
		if dynamicQuotaAppliedLimit(applied, applied+20, threshold, false, false) != applied {
			t.Fatal("routine increase bypassed the allocation timer")
		}
		if dynamicQuotaAppliedLimit(applied, applied+1, threshold, false, true) != applied+1 {
			t.Fatal("manual change or confirmed reset waited for a threshold")
		}
	}
}

func TestDynamicQuotaConfigurableSourceProtection(t *testing.T) {
	now := time.Now().UTC()
	p := DynamicQuotaPoolState{Status: "active", CapacityUSD: 1000, Snapshot: &DynamicQuotaObservation{Identity: "source", UsedPercent: 50, ResetAt: now.Add(time.Hour), WindowSeconds: 604800, FetchedAt: now}}
	for _, tc := range []struct{ ceiling, available float64 }{{98, 470}, {95, 440}, {55.25, 42.5}, {50, 0}, {1, 0}, {100, 490}} {
		p.Settings.UsageCeilingPercent = tc.ceiling
		if got := p.Available(now, 0, 0); math.Abs(got-tc.available) > 1e-8 {
			t.Fatalf("ceiling=%v available=%v want=%v", tc.ceiling, got, tc.available)
		}
	}
}
