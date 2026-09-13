package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// A receipt is the only source of a monetary total; holds are never prices.
// This also validates the owner before using a receipt to inspect canonical dedup.
func dynamicAbsorptionReceipt(raw []byte, id string, accountID, keyID, userID int64) *UsageBillingCommand {
	var c UsageBillingCommand
	if json.Unmarshal(raw, &c) != nil || c.UsageLog == nil || c.DynamicQuotaReservationID != id || c.AccountID != accountID || c.APIKeyID != keyID || c.UserID != userID || c.RequestID == "" || c.RequestFingerprint == "" {
		return nil
	}
	l := c.UsageLog
	if (c.SubscriptionID == nil) != (l.SubscriptionID == nil) || (c.SubscriptionID != nil && *c.SubscriptionID != *l.SubscriptionID) {
		return nil
	}
	if !validDynamicAmount(c.DynamicStandardCost) || !validDynamicAmount(c.SubscriptionCost) || !validDynamicAmount(c.BalanceCost) || l.TotalCost != c.DynamicStandardCost || l.ActualCost != c.SubscriptionCost+c.BalanceCost || l.RequestID != c.RequestID || l.APIKeyID != keyID || l.AccountID != accountID || l.UserID != userID {
		return nil
	}
	return &c
}

// Caller holds the source lock, just like settlement and reset. Short recovery
// failure closes personal liability and the temporary hold, not an actual bill. A VERIFIED
// reset archives the old cycle. Healthy cross-boundary turns retain only their
// live execution hold until Finish/lease expiry; no old customer bill is replayed.
func absorbDynamicRequests(ctx context.Context, tx *sql.Tx, accountID, cycle int64, closing bool) error {
	if !closing {
		return classifyDynamicV2Accounting(ctx, tx, accountID, cycle)
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.id,d.billing_receipt,COALESCE(d.api_key_id,0),COALESCE(d.owner_user_id,k.user_id,0)
 FROM dynamic_quota_requests d LEFT JOIN api_keys k ON k.id=d.api_key_id
 WHERE d.account_id=$1 AND d.cycle<=$2 AND d.source_closed_at IS NULL AND d.status IN ('pending','uncertain')
 AND (d.owner_subscription_id IS NOT NULL OR d.subscription_id IS NOT NULL OR EXISTS(
   SELECT 1 FROM user_subscriptions s WHERE s.user_id=k.user_id AND s.group_id=k.group_id))
 ORDER BY d.id FOR UPDATE OF d`, accountID, cycle)
	if err != nil {
		return err
	}
	type request struct {
		id        string
		raw       []byte
		key, user int64
	}
	var requests []request
	for rows.Next() {
		var r request
		if err = rows.Scan(&r.id, &r.raw, &r.key, &r.user); err != nil {
			rows.Close()
			return err
		}
		requests = append(requests, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		return nil
	}
	ids := make([]string, 0, len(requests))
	for _, r := range requests {
		reason := "cycle_closed"
		var known *float64
		if c := dynamicAbsorptionReceipt(r.raw, r.id, accountID, r.key, r.user); c != nil {
			var billed bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2
 UNION ALL SELECT 1 FROM usage_billing_dedup_archive WHERE request_id=$1 AND api_key_id=$2)`, c.RequestID, r.key).Scan(&billed); err != nil {
				return err
			}
			amount := QuantizeUsageBillingAmount(c.DynamicStandardCost)
			if billed {
				amount = 0
				reason = "already_billed"
			}
			known = &amount
		}
		_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_requests SET operator_absorbed_at=COALESCE(operator_absorbed_at,NOW()),
 operator_absorption_reason=COALESCE(operator_absorption_reason,$2),operator_absorbed_standard_usd=COALESCE(operator_absorbed_standard_usd,$3),
 source_closed_at=CASE WHEN $4 THEN NOW() ELSE source_closed_at END,billing_retry_at=NULL WHERE id=$1`, r.id, reason, known, closing)
		if err != nil {
			return err
		}
		ids = append(ids, r.id)
	}
	raw, err := json.Marshal(map[string]any{"request_ids": ids, "count": len(ids), "source_closed": closing})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_events(account_id,cycle,kind,details) VALUES($1,$2,'operator_absorbed',$3::jsonb)`, accountID, cycle, string(raw))
	return err
}

func (s *DynamicSubscriptionService) absorbExpiredEvidence(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT account_id FROM dynamic_quota_requests
 WHERE source_closed_at IS NULL AND operator_absorbed_at IS NULL AND worker_id IS NOT NULL
 AND review_required_at IS NULL
 AND owner_subscription_id IS NOT NULL
 AND status IN ('pending','uncertain') AND COALESCE(finished_at,lease_until)<NOW()-INTERVAL '5 minutes' ORDER BY account_id LIMIT 20`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		p, err := lockDynamicPool(ctx, tx, id)
		if err == nil {
			err = absorbDynamicRequests(ctx, tx, id, p.Cycle, false)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

type DynamicAbsorptionFilter struct {
	UserID, GroupID         int64
	Status, Platform, Scope string
	Page, PageSize          int
	SummaryOnly             bool
	Category                string
	Visibility              string
}

type DynamicAbsorptionSummary struct {
	Requests         int64   `json:"requests"`
	KnownRequests    int64   `json:"known_requests"`
	KnownStandardUSD float64 `json:"known_standard_usd"`
	UnknownRequests  int64   `json:"unknown_requests"`
}

type DynamicAbsorptionRecord struct {
	ID               string     `json:"id"`
	UserID           int64      `json:"user_id"`
	Email            string     `json:"email"`
	SubscriptionID   int64      `json:"subscription_id"`
	GroupID          int64      `json:"group_id"`
	GroupName        string     `json:"group_name"`
	AccountID        int64      `json:"account_id"`
	AccountName      string     `json:"account_name"`
	Cycle            int64      `json:"cycle"`
	Model            string     `json:"model"`
	Reason           string     `json:"reason"`
	KnownStandardUSD *float64   `json:"known_standard_usd"`
	ReferenceHoldUSD float64    `json:"reference_hold_usd"`
	StartedAt        time.Time  `json:"started_at"`
	AbsorbedAt       time.Time  `json:"absorbed_at"`
	ClosedAt         *time.Time `json:"closed_at"`
	DisplayClearedAt *time.Time `json:"display_cleared_at"`
	NeedsReview      bool       `json:"needs_review"`
	CanCharge        bool       `json:"can_charge"`
	ChargeUSD        *float64   `json:"charge_usd"`
}

type DynamicAbsorptionReport struct {
	Summary  DynamicAbsorptionSummary  `json:"summary"`
	Items    []DynamicAbsorptionRecord `json:"items"`
	Page     int                       `json:"page"`
	PageSize int                       `json:"page_size"`
	Pages    int64                     `json:"pages"`
}

// Fixed ownership wins over a later key reassignment. Legacy rows have only a
// key, so use the existing subscription match without multiplying reservations.
const dynamicAbsorptionFromSQL = ` FROM dynamic_quota_requests d
 LEFT JOIN api_keys k ON k.id=d.api_key_id
 JOIN user_subscriptions us ON us.id=COALESCE(d.owner_subscription_id,d.subscription_id,
   (SELECT s.id FROM user_subscriptions s WHERE s.user_id=k.user_id AND s.group_id=k.group_id ORDER BY (s.deleted_at IS NULL) DESC,s.id DESC LIMIT 1))
 LEFT JOIN users u ON u.id=COALESCE(d.owner_user_id,us.user_id)
 JOIN groups g ON g.id=us.group_id
 JOIN accounts a ON a.id=d.account_id
 WHERE d.operator_absorbed_at IS NOT NULL
 AND ($1::bigint=0 OR COALESCE(d.owner_user_id,us.user_id)=$1)
 AND ($2::bigint=0 OR us.group_id=$2)
 AND ($3::text='' OR ($3='revoked' AND us.deleted_at IS NOT NULL) OR (us.deleted_at IS NULL AND
   (($3='active' AND us.status='active' AND us.expires_at>NOW()) OR
    ($3='expired' AND (us.status='expired' OR (us.status='active' AND us.expires_at<=NOW()))) OR
    ($3 NOT IN ('active','expired','revoked') AND us.status=$3))))
 AND ($4::text='' OR (g.platform=$4 AND g.deleted_at IS NULL))
 AND (($5::text='current' AND d.source_closed_at IS NULL) OR ($5='history' AND d.source_closed_at IS NOT NULL))`

// Read-only and metadata-only. Totals and the selected page share one snapshot;
// a concurrent upstream reset cannot mix current totals with historical rows.
func (s *DynamicSubscriptionService) AbsorptionReport(ctx context.Context, f DynamicAbsorptionFilter) (*DynamicAbsorptionReport, error) {
	if f.Scope != "current" && f.Scope != "history" || f.Page < 1 || f.PageSize < 1 || f.PageSize > 100 || f.UserID < 0 || f.GroupID < 0 || (f.Category != "" && f.Category != "covered" && f.Category != "review") || (f.Visibility != "" && f.Visibility != "uncleared" && f.Visibility != "cleared" && f.Visibility != "all") {
		return nil, ErrDynamicQuotaBinding
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	args := []any{f.UserID, f.GroupID, f.Status, f.Platform, f.Scope}
	from := dynamicAbsorptionFromSQL
	amount, reason, processed := "d.operator_absorbed_standard_usd", "COALESCE(d.operator_absorption_reason,'operator_decision')", "d.operator_absorbed_at"
	if f.Category == "review" {
		from = strings.Replace(from, "WHERE d.operator_absorbed_at IS NOT NULL", "WHERE d.operator_absorbed_at IS NULL AND d.review_required_at IS NOT NULL AND d.status IN ('pending','uncertain')", 1)
		amount, reason, processed = "d.review_standard_usd", "COALESCE(d.review_reason,'missing_evidence')", "d.review_required_at"
	}
	switch f.Visibility {
	case "", "uncleared":
		from += " AND d.display_cleared_at IS NULL"
	case "cleared":
		from += " AND d.display_cleared_at IS NOT NULL"
	}
	out := &DynamicAbsorptionReport{Items: []DynamicAbsorptionRecord{}, Page: f.Page, PageSize: f.PageSize}
	err = tx.QueryRowContext(ctx, `SELECT count(*),count(`+amount+`),COALESCE(sum(`+amount+`),0),count(*) FILTER(WHERE `+amount+` IS NULL)`+from, args...).
		Scan(&out.Summary.Requests, &out.Summary.KnownRequests, &out.Summary.KnownStandardUSD, &out.Summary.UnknownRequests)
	if err != nil {
		return nil, err
	}
	out.Pages = (out.Summary.Requests + int64(f.PageSize) - 1) / int64(f.PageSize)
	if !f.SummaryOnly {
		args = append(args, f.PageSize, (int64(f.Page)-1)*int64(f.PageSize))
		rows, err := tx.QueryContext(ctx, `SELECT d.id,COALESCE(d.owner_user_id,us.user_id),COALESCE(u.email,''),us.id,us.group_id,g.name,
 d.account_id,a.name,d.cycle,COALESCE(d.request_context->>'model',d.billing_receipt->>'Model',d.evidence->>'model',d.late_billing_receipt->>'Model',d.late_evidence->>'model',''),
 `+reason+`,`+amount+`,d.hold_standard_usd,d.started_at,`+processed+`,d.source_closed_at,d.display_cleared_at,d.billing_receipt,COALESCE(d.api_key_id,0)`+from+` ORDER BY `+processed+` DESC,d.id LIMIT $6 OFFSET $7`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r DynamicAbsorptionRecord
			var receipt []byte
			var key int64
			if err = rows.Scan(&r.ID, &r.UserID, &r.Email, &r.SubscriptionID, &r.GroupID, &r.GroupName, &r.AccountID, &r.AccountName, &r.Cycle, &r.Model, &r.Reason, &r.KnownStandardUSD, &r.ReferenceHoldUSD, &r.StartedAt, &r.AbsorbedAt, &r.ClosedAt, &r.DisplayClearedAt, &receipt, &key); err != nil {
				rows.Close()
				return nil, err
			}
			r.NeedsReview = f.Category == "review"
			if c := dynamicAbsorptionReceipt(receipt, r.ID, r.AccountID, key, r.UserID); r.NeedsReview && c != nil {
				r.CanCharge = c.SubscriptionID != nil && *c.SubscriptionID == r.SubscriptionID && c.BalanceCost == 0 && r.ClosedAt == nil
				if r.CanCharge {
					amount := QuantizeUsageBillingAmount(c.SubscriptionCost)
					r.ChargeUSD = &amount
				}
			}
			out.Items = append(out.Items, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Clear only the explicit records the administrator selected. New arrivals are
// never swept up by a filter or a timestamp cutoff. Repeating a request is safe.
func (s *DynamicSubscriptionService) ClearAbsorbedUsage(ctx context.Context, ids []string, actor int64) (int64, error) {
	if actor <= 0 || len(ids) == 0 || len(ids) > 100 {
		return 0, ErrDynamicQuotaBinding
	}
	seen := make(map[uuid.UUID]bool, len(ids))
	canonical := make([]string, 0, len(ids))
	for _, id := range ids {
		u, err := uuid.Parse(id)
		if err != nil || u == uuid.Nil || seen[u] {
			return 0, ErrDynamicQuotaBinding
		}
		seen[u] = true
		canonical = append(canonical, u.String())
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var admin int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=$1 AND role='admin' AND status='active' AND deleted_at IS NULL FOR SHARE`, actor).Scan(&admin); err != nil {
		return 0, ErrDynamicQuotaBinding
	}
	result, err := tx.ExecContext(ctx, `WITH selected AS (
 SELECT id FROM dynamic_quota_requests WHERE id=ANY($1::uuid[])
 AND operator_absorbed_at IS NOT NULL AND display_cleared_at IS NULL ORDER BY id FOR UPDATE
 ) UPDATE dynamic_quota_requests d SET display_cleared_at=NOW(),display_cleared_by=$2
 FROM selected s WHERE d.id=s.id`, pq.Array(canonical), admin)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
