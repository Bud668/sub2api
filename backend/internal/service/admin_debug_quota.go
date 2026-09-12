package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type AdminDebugQuota struct {
	WeeklyLimitUSD   float64    `json:"weekly_limit_usd"` // Zero means explicitly unlimited.
	Revision         int64      `json:"revision"`
	FollowReset      bool       `json:"follow_reset"`
	ResetPending     bool       `json:"reset_pending"`
	ExpectedResetAt  *time.Time `json:"expected_reset_at,omitempty"`
	RemainingUSD     *float64   `json:"remaining_usd"` // Nil means no weekly dollar cap; not a promise of upstream capacity.
	ReservedUSD      float64    `json:"reserved_usd,omitempty"`
	used, held, rate float64
}

func initializeAdminDebugQuota(ctx context.Context, tx dynamicSeatTx, id int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO admin_debug_quotas(subscription_id,weekly_limit_usd,reset_account_id,reset_cycle)
 SELECT us.id,COALESCE(g.weekly_limit_usd,0),gp.account_id,COALESCE((pool.state->>'cycle')::bigint,0)
 FROM user_subscriptions us JOIN users u ON u.id=us.user_id JOIN groups g ON g.id=us.group_id
 LEFT JOIN dynamic_group_policies gp ON gp.group_id=us.group_id
 LEFT JOIN dynamic_quota_pools pool ON pool.account_id=gp.account_id
 WHERE us.id=$1 AND us.admin_debug AND u.role='admin' ON CONFLICT(subscription_id) DO NOTHING`, id)
	return err
}

func loadAdminDebugQuota(ctx context.Context, db dynamicQuotaQuerier, id int64, now time.Time) (*AdminDebugQuota, error) {
	q := &AdminDebugQuota{}
	var source sql.NullInt64
	var cycle int64
	var raw []byte
	var peak Group
	err := db.QueryRowContext(ctx, `SELECT q.weekly_limit_usd,q.revision,q.reset_account_id,q.reset_cycle,
 COALESCE(pool.state,'{}'::jsonb),us.weekly_usage_usd,COALESCE(r.rate_multiplier,g.rate_multiplier),
 g.peak_rate_enabled,g.peak_start,g.peak_end,g.peak_rate_multiplier,
 COALESCE((SELECT sum(d.hold_standard_usd) FROM dynamic_quota_requests d WHERE d.owner_subscription_id=us.id
 AND d.status IN ('pending','uncertain') AND d.operator_absorbed_at IS NULL AND d.review_required_at IS NULL AND d.source_closed_at IS NULL),0)
 FROM admin_debug_quotas q JOIN user_subscriptions us ON us.id=q.subscription_id JOIN users u ON u.id=us.user_id
 JOIN groups g ON g.id=us.group_id LEFT JOIN dynamic_quota_pools pool ON pool.account_id=q.reset_account_id
 LEFT JOIN user_group_rate_multipliers r ON r.user_id=us.user_id AND r.group_id=us.group_id
 WHERE us.id=$1 AND us.admin_debug AND u.role='admin' AND us.deleted_at IS NULL`, id).
		Scan(&q.WeeklyLimitUSD, &q.Revision, &source, &cycle, &raw, &q.used, &q.rate, &peak.PeakRateEnabled, &peak.PeakStart, &peak.PeakEnd, &peak.PeakRateMultiplier, &q.held)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := &DynamicQuotaPoolState{}
	if err = json.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	q.rate *= peak.PeakMultiplierAt(now)
	if !validDynamicAmount(q.rate) || q.rate <= 0 {
		return nil, ErrDynamicQuotaUnavailable
	}
	q.FollowReset = source.Valid
	q.ResetPending = source.Valid && p.ConfirmedAt != nil && p.Cycle > cycle
	q.ReservedUSD = QuantizeUsageBillingAmount(q.held * q.rate)
	if q.WeeklyLimitUSD > 0 || q.ResetPending {
		remaining := QuantizeUsageBillingAmount(math.Max(0, q.WeeklyLimitUSD-q.used-q.held*q.rate))
		if q.ResetPending {
			remaining = 0
		}
		q.RemainingUSD = &remaining
	}
	if p.Snapshot != nil {
		t := p.Snapshot.ResetAt
		q.ExpectedResetAt = &t
	}
	return q, nil
}

func (q *AdminDebugQuota) check(holdStandard float64) error {
	if q == nil {
		return nil
	}
	if q.ResetPending {
		return infraerrors.ServiceUnavailable("ADMIN_DEBUG_RESET_PENDING", "Waiting for this debug subscription's prior requests to settle before following its group's upstream reset")
	}
	if !validDynamicAmount(holdStandard) {
		return ErrDynamicQuotaUnavailable
	}
	if q.WeeklyLimitUSD > 0 && (q.used >= q.WeeklyLimitUSD || q.used+(q.held+holdStandard)*q.rate > q.WeeklyLimitUSD+1e-8) {
		return ErrWeeklyLimitExceeded
	}
	return nil
}

func (s *DynamicSubscriptionService) SaveAdminDebugQuota(ctx context.Context, id, revision int64, limit float64) error {
	if !validDynamicAmount(limit) || limit > 1e9 || revision < 0 || (limit > 0 && QuantizeUsageBillingAmount(limit) == 0) {
		return infraerrors.BadRequest("INVALID_ADMIN_DEBUG_QUOTA", "Choose a non-negative weekly limit; zero means unlimited")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var admin bool
	if err = tx.QueryRowContext(ctx, `SELECT us.admin_debug AND u.role='admin' FROM user_subscriptions us JOIN users u ON u.id=us.user_id
 WHERE us.id=$1 AND us.deleted_at IS NULL FOR UPDATE OF us`, id).Scan(&admin); errors.Is(err, sql.ErrNoRows) {
		return ErrSubscriptionNotFound
	} else if err != nil {
		return err
	}
	if !admin {
		return infraerrors.BadRequest("ADMIN_DEBUG_SUBSCRIPTION", "An administrator debug subscription is required")
	}
	var current int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM admin_debug_quotas WHERE subscription_id=$1`, id).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if current != revision {
		return ErrDynamicQuotaChanged
	}
	if current == 0 {
		if err = initializeAdminDebugQuota(ctx, tx, id); err != nil {
			return err
		}
		revision = 1
	}
	res, err := tx.ExecContext(ctx, `UPDATE admin_debug_quotas SET weekly_limit_usd=$3,revision=revision+1,updated_at=NOW()
 WHERE subscription_id=$1 AND revision=$2`, id, revision, QuantizeUsageBillingAmount(limit))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrDynamicQuotaChanged
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.invalidate(ctx, id)
}

// Source lock -> subscription lock. A debug request may use another upstream;
// do not reset its week while that other request can still bill the old window.
// Only this subscription waits; normal dynamic members reset independently.
func syncAdminDebugResets(ctx context.Context, tx *sql.Tx, accountID int64, p *DynamicQuotaPoolState, now time.Time) ([]int64, error) {
	if p.ConfirmedAt == nil {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT q.subscription_id FROM admin_debug_quotas q JOIN user_subscriptions us ON us.id=q.subscription_id
 JOIN users u ON u.id=us.user_id WHERE q.reset_account_id=$1 AND q.reset_cycle<$2 AND us.admin_debug AND u.role='admin'
 AND us.deleted_at IS NULL ORDER BY us.id FOR UPDATE OF us`, accountID, p.Cycle)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var reset []int64
	for _, id := range ids {
		var pending bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM dynamic_quota_requests WHERE owner_subscription_id=$1
 AND status IN ('pending','uncertain') AND operator_absorbed_at IS NULL AND source_closed_at IS NULL)`, id).Scan(&pending); err != nil {
			return nil, err
		}
		if pending {
			continue
		}
		if _, err = tx.ExecContext(ctx, `SELECT set_config('sub2api.dynamic_quota_reset',$1,true)`, fmt.Sprint(id)); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE user_subscriptions SET weekly_usage_usd=0,weekly_window_start=$2,updated_at=NOW() WHERE id=$1`, id, now); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE admin_debug_quotas SET reset_cycle=$2,updated_at=NOW() WHERE subscription_id=$1 AND reset_account_id=$3`, id, p.Cycle, accountID); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_events(account_id,cycle,kind,details) VALUES($1,$2,'debug_week_reset',jsonb_build_object('subscription_id',$3::bigint))`, accountID, p.Cycle, id); err != nil {
			return nil, err
		}
		reset = append(reset, id)
	}
	return reset, nil
}
