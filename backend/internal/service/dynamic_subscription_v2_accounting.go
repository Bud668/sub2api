package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Development defaults from the reviewed draft; deployment still requires the
// operator to confirm these amounts. Unknown prices are never inferred from holds.
const (
	dynamicV2SingleReviewUSD = 5.0
	dynamicV2CycleReviewUSD  = 20.0
	dynamicV2UnknownAlert    = 5
)

// This unexported context key fences manual replay inside canonical settlement.
// An expired worker cannot charge after another administrator resolves the row.
type dynamicAccountingClaimKey struct{}

func dynamicV2MayAbsorb(known *float64, cumulative float64, unknown int) bool {
	return known != nil && validDynamicAmount(*known) && validDynamicAmount(cumulative) && unknown >= 0 &&
		*known < dynamicV2SingleReviewUSD && cumulative+*known < dynamicV2CycleReviewUSD && unknown < dynamicV2UnknownAlert
}

// Runs under the existing source lock, after one bounded recovery attempt.
// Only ended/lost managed requests are candidates. Old unowned records require
// explicit review; a healthy long request never becomes a small automatic waiver.
func classifyDynamicV2Accounting(ctx context.Context, tx *sql.Tx, accountID, cycle int64) error {
	var cumulative float64
	var unknown int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(COALESCE(operator_absorbed_standard_usd,review_standard_usd)),0),
 count(*) FILTER(WHERE COALESCE(operator_absorbed_standard_usd,review_standard_usd) IS NULL)
 FROM dynamic_quota_requests WHERE account_id=$1 AND (cycle=$2 OR ($2=1 AND cycle=0)) AND source_closed_at IS NULL
 AND (operator_absorbed_at IS NOT NULL OR (review_required_at IS NOT NULL AND status IN ('pending','uncertain')))`, accountID, cycle).Scan(&cumulative, &unknown)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.id,d.billing_receipt,COALESCE(d.api_key_id,0),d.owner_user_id,d.owner_subscription_id,
 COALESCE(d.request_context->>'settlement_policy','')
 FROM dynamic_quota_requests d WHERE d.account_id=$1 AND (d.cycle=$2 OR ($2=1 AND d.cycle=0))
 AND d.source_closed_at IS NULL AND d.operator_absorbed_at IS NULL AND d.review_required_at IS NULL
 AND d.status IN ('pending','uncertain') AND d.worker_id IS NOT NULL AND d.owner_user_id IS NOT NULL
 AND d.owner_subscription_id IS NOT NULL AND COALESCE(d.finished_at,d.lease_until)<NOW()-INTERVAL '5 minutes'
 ORDER BY d.started_at,d.id LIMIT 100 FOR UPDATE`, accountID, cycle)
	if err != nil {
		return err
	}
	type request struct {
		id               string
		raw              []byte
		key, user, owner int64
		policy           string
	}
	var requests []request
	for rows.Next() {
		var r request
		if err = rows.Scan(&r.id, &r.raw, &r.key, &r.user, &r.owner, &r.policy); err != nil {
			rows.Close()
			return err
		}
		requests = append(requests, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var covered, review, alert int
	for _, r := range requests {
		var known *float64
		reason := "missing_evidence"
		c := dynamicAbsorptionReceipt(r.raw, r.id, accountID, r.key, r.user)
		if c != nil {
			var billed bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2
 UNION ALL SELECT 1 FROM usage_billing_dedup_archive WHERE request_id=$1 AND api_key_id=$2)`, c.RequestID, r.key).Scan(&billed); err != nil {
				return err
			}
			amount := QuantizeUsageBillingAmount(c.DynamicStandardCost)
			if c.SubscriptionID == nil || *c.SubscriptionID != r.owner || c.BalanceCost > 0 {
				reason = "receipt_scope" // Subscription waivers cannot close unrelated billing.
			}
			if billed {
				amount, reason = 0, "already_billed"
			}
			known = &amount
		}
		if r.policy == automaticSettlementPolicy && c != nil && reason != "already_billed" && reason != "receipt_scope" {
			// A valid unpaid receipt belongs to automatic idempotent billing, not
			// a size-based waiver or a manual review queue. Keep retrying it.
			if _, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_requests SET billing_retry_at=COALESCE(billing_retry_at,NOW()) WHERE id=$1`, r.id); err != nil {
				return err
			}
			continue
		}
		if r.policy == automaticSettlementPolicy {
			if reason != "already_billed" {
				// No verified customer bill: close personal liability without
				// inventing a price or releasing physical source capacity. Late
				// evidence remains audit-only through the existing settlement fence.
				known = nil
				switch {
				case reason == "receipt_scope":
					reason = "automatic_receipt_mismatch"
					alert++
				case len(r.raw) > 0:
					reason = "automatic_invalid_receipt"
					alert++
				default:
					reason = "automatic_unmetered"
				}
				if unknown+1 >= dynamicV2UnknownAlert {
					alert++
				}
			}
			_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_requests SET operator_absorbed_at=NOW(),
 operator_absorption_reason=$2,operator_absorbed_standard_usd=$3,billing_retry_at=NULL WHERE id=$1`, r.id, reason, known)
			covered++
		} else if reason == "already_billed" || (reason != "receipt_scope" && dynamicV2MayAbsorb(known, cumulative, unknown)) {
			if reason != "already_billed" {
				reason = "small_exception"
			}
			_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_requests SET operator_absorbed_at=NOW(),
 operator_absorption_reason=$2,operator_absorbed_standard_usd=$3,billing_retry_at=NULL WHERE id=$1`, r.id, reason, known)
			covered++
		} else {
			if known != nil && reason != "receipt_scope" {
				reason = "review_threshold"
			}
			_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_requests SET review_required_at=NOW(),review_reason=$2,
 review_standard_usd=$3,billing_retry_at=NULL WHERE id=$1`, r.id, reason, known)
			review++
			if known != nil || unknown+1 >= dynamicV2UnknownAlert {
				alert++
			}
		}
		if err != nil {
			return err
		}
		if known == nil {
			unknown++
		} else {
			cumulative += *known
		}
	}
	if covered+review == 0 {
		return nil
	}
	if alert > 0 {
		var notified bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM dynamic_quota_events WHERE account_id=$1
 AND cycle=$2 AND kind='accounting_classified' AND (details->>'alert')::int>0)`, accountID, cycle).Scan(&notified); err != nil {
			return err
		}
		if notified {
			alert = 0 // One escalation per source-cycle, not one message per exception.
		}
	}
	details, _ := json.Marshal(map[string]any{"covered": covered, "review": review, "alert": alert, "known_standard_usd": cumulative, "unknown_requests": unknown})
	_, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_events(account_id,cycle,kind,details)
 VALUES($1,$2,'accounting_classified',$3::jsonb)`, accountID, cycle, string(details))
	return err
}

// A review action identifies an existing receipt, never a caller-provided price.
// Charging reuses canonical billing dedup. Claiming under the source lock keeps
// two admins (or charge/cover) from racing; crashed claims expire after a minute.
func (s *DynamicSubscriptionService) ResolveAccounting(ctx context.Context, id string, actor int64, action string) error {
	if _, err := uuid.Parse(id); err != nil || actor <= 0 || (action != "charge" && action != "cover") {
		return ErrDynamicQuotaChanged
	}
	var source int64
	if err := s.db.QueryRowContext(ctx, `SELECT account_id FROM dynamic_quota_requests WHERE id=$1`, id).Scan(&source); err != nil {
		return ErrDynamicQuotaChanged
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := lockDynamicPool(ctx, tx, source)
	if err != nil {
		return err
	}
	var raw []byte
	var key, user, owner, cycle int64
	var eligible bool
	var known sql.NullFloat64
	err = tx.QueryRowContext(ctx, `SELECT billing_receipt,COALESCE(api_key_id,0),COALESCE(owner_user_id,0),
 COALESCE(owner_subscription_id,0),cycle,review_standard_usd,
 review_required_at IS NOT NULL AND operator_absorbed_at IS NULL AND source_closed_at IS NULL
 AND status IN ('pending','uncertain') AND (billing_retry_at IS NULL OR billing_retry_at<=NOW())
 FROM dynamic_quota_requests WHERE id=$1 FOR UPDATE`, id).Scan(&raw, &key, &user, &owner, &cycle, &known, &eligible)
	if err != nil {
		return err
	}
	if !eligible || (cycle != p.Cycle && !(cycle == 0 && p.Cycle == 1 && p.ConfirmedAt == nil)) {
		return ErrDynamicQuotaChanged
	}
	c := dynamicAbsorptionReceipt(raw, id, source, key, user)
	if action == "charge" && (c == nil || c.SubscriptionID == nil || *c.SubscriptionID != owner || c.BalanceCost > 0 || s.replay == nil) {
		return ErrDynamicQuotaUnavailable
	}
	claimID := uuid.NewString()
	if action == "cover" {
		_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_requests SET operator_absorbed_at=NOW(),
 operator_absorption_reason='operator_decision',operator_absorbed_standard_usd=$2,
 reviewed_by=$3,reviewed_at=NOW(),billing_retry_at=NULL,review_claim_id=NULL WHERE id=$1`, id, known, actor)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_requests SET billing_retry_at=NOW()+INTERVAL '1 minute',
 reviewed_by=$2,review_claim_id=$3 WHERE id=$1`, id, actor, claimID)
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_events(account_id,cycle,kind,details)
 VALUES($1,$2,'accounting_review_action',jsonb_build_object('request_id',$3::text,'actor_id',$4::bigint,'action',$5::text))`, source, cycle, id, actor, action); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil || action == "cover" {
		return err
	}
	err = s.replay(context.WithValue(ctx, dynamicAccountingClaimKey{}, claimID), c)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var settled bool
	cleanupErr := s.db.QueryRowContext(cleanup, `UPDATE dynamic_quota_requests SET
 billing_retry_at=CASE WHEN status IN ('settled','rejected') AND $3 THEN billing_retry_at ELSE NULL END,
 review_claim_id=NULL,
 reviewed_at=CASE WHEN status IN ('settled','rejected') THEN NOW() ELSE reviewed_at END
	WHERE id=$1 AND review_required_at IS NOT NULL AND review_claim_id=$2 AND operator_absorbed_at IS NULL
 RETURNING status IN ('settled','rejected')`, id, claimID, err != nil).Scan(&settled)
	if err == nil && cleanupErr == nil && !settled {
		return ErrDynamicQuotaUnavailable // A nil callback result is not proof of a committed bill.
	}
	return errors.Join(err, cleanupErr)
}
