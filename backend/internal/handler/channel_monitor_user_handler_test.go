package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorUserWindowStatsRespectQuotaVisibility(t *testing.T) {
	view := &service.UserMonitorView{
		ID: 1, Provider: "openai", PrimaryModel: "quota",
		LatestQuota: &domain.MonitorQuotaSnapshot{
			Source: "usage", Success: true,
			Tiers: []domain.MonitorQuotaTier{{
				Window: "7d", UsedPercent: 74,
				WindowStats: &domain.WindowStats{Requests: 11900, Tokens: 1500000000, Cost: 1615.59, StandardCost: 800, UserCost: 400},
			}},
		},
	}
	visible := userMonitorViewToItem(view, true)
	data, err := json.Marshal(visible)
	require.NoError(t, err)
	var restored channelMonitorUserListItem
	require.NoError(t, json.Unmarshal(data, &restored))
	require.Equal(t, view.LatestQuota.Tiers, restored.LatestQuota.Tiers)

	data, err = json.Marshal(userMonitorViewToItem(view, false))
	require.NoError(t, err)
	require.NotContains(t, string(data), "latest_quota")
	require.NotContains(t, string(data), "window_stats")
	require.False(t, (&ChannelMonitorUserHandler{}).quotaVisible(nil), "missing settings must fail closed")
}
