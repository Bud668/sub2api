package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type DynamicResetResult struct {
	AccountID int64  `json:"account_id"`
	Cycle     int64  `json:"cycle"`
	Status    string `json:"status"`
	Members   int    `json:"members"`
}

// The manual path runs the same identity, independent-sample and accounting
// checks as the timer. The client cannot supply an upstream identity or reset
// timestamp, and retrying an already completed cycle never grants quota again.
func (s *DynamicSubscriptionService) SyncReset(ctx context.Context, id, accountID, cycle int64) (*DynamicResetResult, error) {
	if s == nil || s.disabled {
		return nil, ErrDynamicQuotaUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if _, err := s.ResetPreview(ctx, id); err != nil {
		return nil, err
	}
	q, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if q == nil || !q.Enabled || q.AccountID != accountID || cycle <= 0 || cycle > q.Cycle {
		return nil, ErrDynamicQuotaChanged
	}
	// Shared with the minute worker: repeated clicks are not independent proof.
	if q.Cycle == cycle && time.Since(q.pool.Health.LastAttemptAt) >= dynamicQuotaConfirmDelay {
		if err = s.Refresh(ctx, accountID); err != nil {
			return nil, err
		}
		q, err = s.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		if q == nil || !q.Enabled || q.AccountID != accountID {
			return nil, ErrDynamicQuotaChanged
		}
	}
	out := &DynamicResetResult{AccountID: accountID, Cycle: q.Cycle, Status: "unchanged"}
	if q.Cycle > cycle {
		out.Status = "reset"
	} else if q.Status == "confirming" || q.Status == "settling" {
		out.Status = "confirming"
	} else if q.Status != "active" && q.Status != "learning" {
		out.Status = "unconfirmed"
	}
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM dynamic_subscription_policies p WHERE p.account_id=$1 AND `+dynamicActiveMemberSQL, accountID).Scan(&out.Members)
	return out, err
}

// Preview is local/read-only; opening a dialog never resets quota or fetches upstream.
func (s *DynamicSubscriptionService) ResetPreview(ctx context.Context, id int64) (*DynamicResetResult, error) {
	var accountID int64
	err := s.db.QueryRowContext(ctx, `SELECT p.account_id FROM dynamic_subscription_policies p WHERE p.subscription_id=$1 AND `+dynamicActiveMemberSQL, id).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, infraerrors.Conflict("DYNAMIC_QUOTA_INACTIVE", "This subscription does not have an active dynamic cycle")
	}
	if err != nil {
		return nil, err
	}
	out := &DynamicResetResult{AccountID: accountID}
	err = s.db.QueryRowContext(ctx, `SELECT (state->>'cycle')::bigint,state->>'status',
 (SELECT count(*) FROM dynamic_subscription_policies p WHERE p.account_id=$1 AND `+dynamicActiveMemberSQL+`)
 FROM dynamic_quota_pools WHERE account_id=$1`, accountID).Scan(&out.Cycle, &out.Status, &out.Members)
	return out, err
}
