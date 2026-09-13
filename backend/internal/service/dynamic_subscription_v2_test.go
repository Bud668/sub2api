package service

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaV2CumulativeAllocation(t *testing.T) {
	members := []dynamicQuotaV2Member{
		{ID: 1, Weight: 1, Used: 300, Floor: 200, Cap: 600},
		{ID: 2, Weight: 1, Used: 100, Floor: 200, Cap: 600},
		{ID: 3, Weight: 1, Floor: 200, Cap: 600},
		{ID: 4, Weight: 1, Floor: 200, Cap: 600},
	}
	first, err := allocateDynamicQuotaV2(members, 1280)
	require.NoError(t, err)
	for _, limit := range first {
		require.InDelta(t, 420, limit, 1e-8)
	}
	members[0].Used += 100
	again, err := allocateDynamicQuotaV2(members, 1180)
	require.NoError(t, err)
	for id, limit := range first {
		require.InDelta(t, limit, again[id], 1e-8, "usage is not another grant")
	}
	members[0].Cap = 300 // Spending above a new cap is retained, never refunded.
	capped, err := allocateDynamicQuotaV2(members, 1180)
	require.NoError(t, err)
	require.Equal(t, 400.0, capped[1])
	for _, id := range []int64{2, 3, 4} {
		require.InDelta(t, 1280.0/3, capped[id], 1e-8)
	}
}

func TestDynamicQuotaV2BoundsAndBudget(t *testing.T) {
	for _, remaining := range []float64{0, 1, 199.99, 200, 800, 10000} {
		members := []dynamicQuotaV2Member{{ID: 1, Weight: 1, Floor: 100, Cap: 200}, {ID: 2, Weight: 3, Floor: 100, Cap: 600}}
		out, err := allocateDynamicQuotaV2(members, remaining)
		if remaining < 200 {
			require.ErrorIs(t, err, errDynamicQuotaV2Budget)
			require.Nil(t, out, "a conflict must not silently lower protection")
			continue
		}
		require.NoError(t, err)
		total := 0.0
		for _, m := range members {
			require.GreaterOrEqual(t, out[m.ID], m.Floor)
			require.LessOrEqual(t, out[m.ID], m.Cap)
			total += out[m.ID] - m.Used
		}
		require.LessOrEqual(t, total, remaining+1e-8)
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		_, err := allocateDynamicQuotaV2(nil, bad)
		require.Error(t, err)
		_, err = allocateDynamicQuotaV2([]dynamicQuotaV2Member{{ID: 1, Weight: bad, Cap: 600}}, 100)
		require.Error(t, err)
	}
	_, err := allocateDynamicQuotaV2([]dynamicQuotaV2Member{{ID: 1, Weight: 1, Floor: 601, Cap: 600}}, 1000)
	require.Error(t, err)
	_, err = allocateDynamicQuotaV2([]dynamicQuotaV2Member{{ID: 1, Weight: 1}, {ID: 1, Weight: 1}}, 1000)
	require.Error(t, err)
}

func TestDynamicQuotaV2NodesUseCumulativeWindow(t *testing.T) {
	now := time.Now().UTC().Add(-5 * time.Minute)
	o := dynamicTestObservation(4, 19, now.Add(6*24*time.Hour), now)
	p := DynamicQuotaPoolState{}
	require.False(t, p.Observe(o, now))
	p.startV2()
	require.Equal(t, 15, p.V2.LastNode)
	o.FetchedAt = now.Add(time.Minute)
	o.UsedPercent, o.LocalStandardTotal = 19.96, 9.6
	require.False(t, p.Observe(o, o.FetchedAt))
	require.False(t, p.v2AllocationDue(o.FetchedAt))
	o.FetchedAt = now.Add(2 * time.Minute)
	o.UsedPercent, o.LocalStandardTotal = 32, 130
	o.WindowCostUSD, o.WindowStandardUSD = 320, 320
	require.False(t, p.Observe(o, o.FetchedAt))
	require.InDelta(t, 1000, p.CapacityUSD, 1e-8)
	require.True(t, p.v2AllocationDue(o.FetchedAt))
	p.V2.LastNode, p.LastAllocationAt = dynamicQuotaNode(o.UsedPercent), o.FetchedAt
	for i := 3; i < 6; i++ {
		o.FetchedAt = now.Add(time.Duration(i) * time.Minute)
		p.Observe(o, o.FetchedAt)
		require.False(t, p.v2AllocationDue(o.FetchedAt), "repeat queries do not reallocate")
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	var restored DynamicQuotaPoolState
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, 30, restored.V2.LastNode)
	require.False(t, restored.v2AllocationDue(o.FetchedAt))
	other := DynamicQuotaPoolState{}
	require.Nil(t, other.V2, "a different source stays independent")
}

func TestDynamicQuotaV2SpikeKeepsLastCapacity(t *testing.T) {
	now := time.Now().UTC().Add(-5 * time.Minute)
	o := dynamicTestObservation(4, 10, now.Add(6*24*time.Hour), now)
	p := DynamicQuotaPoolState{}
	p.Observe(o, now)
	p.startV2()
	p.CapacityUSD, p.Status = 1000, "active"
	o.FetchedAt = now.Add(time.Minute)
	o.UsedPercent, o.LocalStandardTotal = 20, 1000
	o.WindowCostUSD, o.WindowStandardUSD = 1800, 1800
	p.Observe(o, o.FetchedAt)
	require.Equal(t, 1000.0, p.CapacityUSD)
	require.Equal(t, 1, p.V2.CandidateSamples)
	require.True(t, p.growthFrozen(o.FetchedAt))
	for i := 2; i <= 4; i++ {
		o.FetchedAt = now.Add(time.Duration(i) * time.Minute)
		p.Observe(o, o.FetchedAt)
	}
	require.Equal(t, 1, p.V2.CandidateSamples, "polling is not independent consumption evidence")
	require.Equal(t, 1000.0, p.CapacityUSD)
	require.Equal(t, "active", p.accessStatus(o.FetchedAt), "trusted remaining usage stays available")
	require.False(t, p.v2AllocationDue(o.FetchedAt))
	// Reset commitment remains the shared, separately verified store operation.
	o.UsedPercent, o.LocalStandardTotal = 0, 1000
	p.Candidate = &o
	p.Confirm(o.FetchedAt)
	require.Equal(t, int64(2), p.Cycle)
	require.Zero(t, p.CapacityUSD)
	require.Zero(t, p.V2.LastNode)
	require.Zero(t, p.V2.CandidateSamples)
}

func TestDynamicQuotaV2UnreservedAllocationStillExcludesNativeThreshold(t *testing.T) {
	now := time.Now().UTC()
	o := dynamicTestObservation(4, 0, now.Add(6*24*time.Hour), now)
	p := DynamicQuotaPoolState{ceilingPercent: 99}
	p.Observe(o, now)
	o.FetchedAt, o.UsedPercent, o.LocalStandardTotal = now.Add(time.Minute), 10, 150
	o.WindowCostUSD, o.WindowStandardUSD = 150, 150
	p.Observe(o, o.FetchedAt)
	require.InDelta(t, 1500, p.CapacityUSD, 1e-8, "no additional ten-percent reserve")
	members := []dynamicQuotaV2Member{{ID: 1, Weight: 1, Used: 75, Floor: 100, Cap: 1000}, {ID: 2, Weight: 1, Used: 75, Floor: 100, Cap: 1000}}
	grants, err := allocateDynamicQuotaV2(members, p.Available(o.FetchedAt, 150, 0))
	require.NoError(t, err)
	require.InDelta(t, 742.5, grants[1], 1e-8)
	require.InDelta(t, 1485, grants[1]+grants[2], 1e-8, "the protected final one percent is never allocated")
	grants, err = allocateDynamicQuotaV2(members, p.Available(o.FetchedAt, 160, 15))
	require.NoError(t, err)
	require.InDelta(t, 1460, grants[1]+grants[2], 1e-8, "later settlements and in-flight costs still reduce the budget")
	p.Snapshot.UsedPercent = 99
	require.Zero(t, p.Available(o.FetchedAt, 150, 0))
}
