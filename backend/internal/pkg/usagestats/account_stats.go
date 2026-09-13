package usagestats

import "math"

// AccountWindowStatsSQL is shared by the usage window and dynamic allocation.
// The latter executes it under its existing source lock, alongside its ledger watermark.
const AccountWindowStatsSQL = `SELECT COUNT(*),
 COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens),0),
 COALESCE(SUM(COALESCE(account_stats_cost,total_cost) * COALESCE(account_rate_multiplier,1)),0),
 COALESCE(SUM(total_cost),0),COALESCE(SUM(actual_cost),0)
 FROM usage_logs WHERE account_id=$1 AND created_at >= $2`

// EstimateWindowTotalCost uses cumulative cost, not differences between samples.
// A missing/zero utilization cannot produce an estimate.
func EstimateWindowTotalCost(cost, percent float64) *float64 {
	if cost <= 0 || percent <= 0 || percent > 100 {
		return nil
	}
	v := cost * 100 / percent
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return nil
	}
	return &v
}

// AccountStats 账号使用统计
//
// cost: 账号口径费用（account_stats_cost，无独立统计值时用 total_cost，再乘 account_rate_multiplier）
// standard_cost: 标准费用（使用 total_cost，不含倍率）
// user_cost: 用户/API Key 口径费用（使用 actual_cost，受分组倍率影响）
type AccountStats struct {
	Requests     int64   `json:"requests"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
	StandardCost float64 `json:"standard_cost"`
	UserCost     float64 `json:"user_cost"`
}
