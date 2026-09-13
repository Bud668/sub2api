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
	o.WindowCostUSD, o.WindowStandardUSD = 500, 500
	p.Observe(o, o.FetchedAt)
	if math.Abs(p.CapacityUSD-1000) > 1e-8 {
		t.Fatalf("cumulative usage-window estimate should establish capacity: %+v", p)
	}
	if got := p.Available(o.FetchedAt, 220, 3); math.Abs(got-477) > 1e-8 {
		t.Fatalf("unreported/in-flight costs not reserved: %v", got)
	}
	if got := p.Available(o.FetchedAt.Add(11*time.Minute), 220, 3); math.Abs(got-477) > 1e-8 {
		t.Fatalf("stale query must retain the trusted remaining budget: %v", got)
	}
	if got := p.Available(o.FetchedAt.Add(12*time.Minute), 320, 3); math.Abs(got-377) > 1e-8 {
		t.Fatalf("later settlements must reduce the trusted budget: %v", got)
	}
	if p.Available(o.ResetAt, 320, 3) != 0 {
		t.Fatal("an expired window cannot authorize a new cycle")
	}
	if p.Cycle != 1 || p.ConfirmedAt != nil {
		t.Fatal("capacity estimate triggered reset")
	}
}

func TestDynamicQuotaNativeSourceProtection(t *testing.T) {
	now := time.Now().UTC()
	p := DynamicQuotaPoolState{Status: "active", CapacityUSD: 1000, Snapshot: &DynamicQuotaObservation{Identity: "source", UsedPercent: 50, ResetAt: now.Add(time.Hour), WindowSeconds: 604800, FetchedAt: now}}
	for _, tc := range []struct{ ceiling, available float64 }{{99, 490}, {98, 480}, {95, 450}, {55.25, 52.5}, {50, 0}, {1, 0}, {100, 500}, {0, 500}} {
		p.ceilingPercent = tc.ceiling
		if got := p.Available(now, 0, 0); math.Abs(got-tc.available) > 1e-8 {
			t.Fatalf("ceiling=%v available=%v want=%v", tc.ceiling, got, tc.available)
		}
	}
}
