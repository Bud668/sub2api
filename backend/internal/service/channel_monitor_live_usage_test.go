//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestQuotaFetcher_LocalUsageCacheIsSharedAndIndependentOfProbes(t *testing.T) {
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[4] = &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	usage.usage = &UsageInfo{FiveHour: &UsageProgress{Utilization: 25, WindowStats: &WindowStats{Requests: 10}}}
	usage.block = make(chan struct{})
	probeDone := make(chan *domain.MonitorQuotaSnapshot, 1)
	go func() { probeDone <- fetcher.Fetch(context.Background(), 4) }()
	require.Eventually(t, func() bool { return usage.getCalls() == 1 }, time.Second, time.Millisecond)
	t.Cleanup(func() { close(usage.block); <-probeDone })

	// Even while an actual probe is blocked, concurrent viewers can read locally.
	var wg sync.WaitGroup
	snapshots := make([]*domain.MonitorQuotaSnapshot, 20)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := range snapshots {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snapshots[i] = fetcher.FetchLocalOpenAIUsage(ctx, 4)
		}(i)
	}
	wg.Wait()
	for _, snapshot := range snapshots {
		require.True(t, snapshot.Success)
		require.Same(t, snapshots[0], snapshot)
		require.EqualValues(t, 10, snapshot.Tiers[0].WindowStats.Requests)
	}
	require.Equal(t, 1, usage.localCalls)
	require.Equal(t, 1, usage.getCalls())

	key := monitorQuotaCacheKey{accountID: 4, local: true}
	fetcher.mu.Lock()
	entry := fetcher.cache[key]
	require.WithinDuration(t, entry.snapshot.FetchedAt.Add(30*time.Second), entry.expiry, time.Second)
	entry.expiry = time.Now().Add(-time.Second)
	fetcher.cache[key] = entry
	fetcher.mu.Unlock()

	// Expiry reloads the account; it cannot fall back to an old successful probe.
	accounts.err = errors.New("private database detail")
	snapshot := fetcher.FetchLocalOpenAIUsage(ctx, 4)
	require.False(t, snapshot.Success)
	require.Empty(t, snapshot.Tiers)
	require.NotContains(t, snapshot.Error, "private")
}

func TestQuotaFetcher_LocalUsageRejectsUnsupportedAccountsAndSanitizesErrors(t *testing.T) {
	fetcher, usage, quota, balance, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[1] = &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	accounts.accounts[2] = &Account{ID: 2, Platform: PlatformKimi, Type: AccountTypeOAuth}
	accounts.accounts[3] = &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	usage.err = errors.New("SQL failed with private credentials")
	for _, id := range []int64{1, 2, 3, 99} {
		snapshot := fetcher.FetchLocalOpenAIUsage(context.Background(), id)
		require.False(t, snapshot.Success)
		require.Empty(t, snapshot.Tiers)
		require.NotContains(t, snapshot.Error, "private")
	}
	require.Equal(t, 1, usage.localCalls)
	require.Zero(t, usage.getCalls())
	require.Zero(t, quota.calls)
	require.Zero(t, balance.calls)
}

type liveUsageMonitorRepo struct {
	ChannelMonitorRepository
	monitors []*ChannelMonitor
	latest   map[int64][]*ChannelMonitorLatest
}

func (r *liveUsageMonitorRepo) ListEnabled(context.Context) ([]*ChannelMonitor, error) {
	return r.monitors, nil
}
func (r *liveUsageMonitorRepo) ListLatestForMonitorIDs(context.Context, []int64) (map[int64][]*ChannelMonitorLatest, error) {
	return r.latest, nil
}
func (r *liveUsageMonitorRepo) ComputeAvailabilityForMonitors(context.Context, []int64, int) (map[int64][]*ChannelMonitorAvailability, error) {
	return nil, nil
}
func (r *liveUsageMonitorRepo) ListRecentHistoryForMonitors(context.Context, []int64, map[int64]string, int) (map[int64][]*ChannelMonitorHistoryEntry, error) {
	return nil, nil
}

func TestChannelMonitorUserView_UsesCurrentAccountWithoutChangingProbeHistory(t *testing.T) {
	old := &domain.MonitorQuotaSnapshot{Success: true, Tiers: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 10}}}
	repo := &liveUsageMonitorRepo{
		monitors: []*ChannelMonitor{{ID: 1, Enabled: true, Provider: MonitorProviderOpenAI, PrimaryModel: "test", AccountID: int64Ptr(4), CheckMode: MonitorCheckModeQuotaProbe}},
		latest:   map[int64][]*ChannelMonitorLatest{1: {{Model: "test", Status: MonitorStatusOperational, Quota: old}}},
	}
	fetcher, usage, _, _, accounts := newQuotaFetcherTestSetup(t)
	accounts.accounts[4] = &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	accounts.accounts[5] = &Account{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	usage.usage = &UsageInfo{FiveHour: &UsageProgress{Utilization: 25}}
	svc := NewChannelMonitorService(repo, nil)
	svc.SetQuotaFetcher(fetcher)

	views, err := svc.ListUserView(context.Background(), false)
	require.NoError(t, err)
	require.Nil(t, views[0].LatestQuota)
	require.Zero(t, accounts.calls, "hidden quotas must not load account data")
	views, err = svc.ListUserView(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, 25.0, views[0].LatestQuota.Tiers[0].UsedPercent)
	require.Equal(t, MonitorStatusOperational, views[0].PrimaryStatus)
	require.Equal(t, 10.0, old.Tiers[0].UsedPercent, "probe history must remain unchanged")

	repo.monitors[0].AccountID = int64Ptr(5)
	usage.usage = &UsageInfo{FiveHour: &UsageProgress{Utilization: 60}}
	views, err = svc.ListUserView(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, 60.0, views[0].LatestQuota.Tiers[0].UsedPercent)
	require.EqualValues(t, 5, usage.getLastAccount().ID)
	repo.monitors[0].AccountID = nil
	views, err = svc.ListUserView(context.Background(), true)
	require.NoError(t, err)
	require.Nil(t, views[0].LatestQuota)
	require.Zero(t, usage.getCalls())
}
