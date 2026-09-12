package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type dynamicQuotaMetadataKey struct{}
type dynamicQuotaMetadata struct {
	RequestID        string `json:"request_id"`
	Model            string `json:"model,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	Turn             int    `json:"turn,omitempty"`
	SettlementPolicy string `json:"settlement_policy,omitempty"`
}

const automaticSettlementPolicy = "automatic_v1"

func WithDynamicQuotaRequestMetadata(ctx context.Context, model, endpoint string, turn int) context.Context {
	return context.WithValue(ctx, dynamicQuotaMetadataKey{}, dynamicQuotaMetadata{Model: model, Endpoint: endpoint, Turn: turn})
}

func (s *DynamicSubscriptionService) runAccountingRecovery() {
	defer close(s.recoveryDone)
	if s.disabled {
		return
	}
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := s.recoverAccounting(ctx); err != nil {
			// Never print a receipt, credentials or the database error body.
			logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_accounting_recovery_unavailable")
		}
		cancel()
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

func (s *DynamicSubscriptionService) recoverAccounting(ctx context.Context) error {
	// A lease belongs to a process, not a request timeout. Long healthy turns
	// remain in flight. Expiry NEVER refunds or bills, even when dispatch is absent
	// (a process can die between the durable dispatch mark and the socket write).
	// Stopping admission does not mean a still-finalizing request is dead.
	active := make([]string, 0)
	s.active.Range(func(id, _ any) bool { active = append(active, id.(string)); return true })
	raw, _ := json.Marshal(active)
	if _, err := s.db.ExecContext(ctx, `UPDATE dynamic_quota_requests SET lease_until=NOW()+INTERVAL '2 minutes'
 WHERE worker_id=$1 AND status='pending' AND finished_at IS NULL AND lease_until IS NOT NULL
 AND (operator_absorbed_at IS NULL OR source_closed_at IS NOT NULL)
 AND id IN (SELECT jsonb_array_elements_text($2::jsonb)::uuid)`, s.workerID, string(raw)); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE dynamic_quota_requests SET status='uncertain',
 outcome=COALESCE(outcome,'worker_interrupted'),finished_at=COALESCE(finished_at,NOW())
 WHERE status='pending' AND lease_until<NOW() AND billing_receipt IS NULL AND operator_absorbed_at IS NULL`)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_accounting_recovery_pending count=%d", n)
	}
	if err := s.recoverBillingReceipts(ctx, 0); err != nil {
		return err
	}
	return s.absorbExpiredEvidence(ctx)
}

// Background retries obey backoff. A source sync gets one bounded final attempt
// at its priced receipts before observing/resetting the old customer cycle.
func (s *DynamicSubscriptionService) recoverBillingReceipts(ctx context.Context, accountID int64) error {
	if s.replay == nil {
		return nil
	}
	// Claim a bounded batch before calling canonical billing, preserving its
	// pool -> subscription lock order. The receipt survives process failure.
	// ponytail: 20 per batch (background + source sync); grow only for measured backlog.
	rows, err := s.db.QueryContext(ctx, `WITH due AS (
 SELECT id FROM dynamic_quota_requests WHERE status IN ('pending','uncertain','settled','rejected')
 AND billing_receipt IS NOT NULL AND operator_absorbed_at IS NULL
 AND (review_required_at IS NULL OR status IN ('settled','rejected'))
 AND (($1::bigint=0 AND billing_retry_at<=NOW()) OR ($1>0 AND account_id=$1 AND billing_retry_at IS NOT NULL))
 ORDER BY billing_retry_at,id LIMIT 20 FOR UPDATE SKIP LOCKED)
 UPDATE dynamic_quota_requests d SET billing_retry_at=NOW()+INTERVAL '1 minute'
 FROM due WHERE d.id=due.id RETURNING d.id,d.billing_receipt`, accountID)
	if err != nil {
		return err
	}
	type receipt struct {
		id  string
		raw []byte
	}
	var bills []receipt
	for rows.Next() {
		var bill receipt
		if err = rows.Scan(&bill.id, &bill.raw); err != nil {
			break
		}
		bills = append(bills, bill)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, bill := range bills {
		var cmd UsageBillingCommand
		if err := json.Unmarshal(bill.raw, &cmd); err != nil || cmd.DynamicQuotaReservationID != bill.id || cmd.UsageLog == nil {
			logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_billing_receipt_invalid reservation=%s", bill.id)
			continue
		}
		if cmd.BalanceCost > 0 {
			// ponytail: manual review until platform quotas support idempotent replay too.
			// Balance billing also increments Redis-backed platform quotas outside
			// the canonical transaction. An unfinished bill needs manual review;
			// replaying just its money would silently omit that auxiliary usage.
			res, err := s.db.ExecContext(ctx, `UPDATE dynamic_quota_requests SET status='uncertain',
 outcome='balance_quota_review',billing_retry_at=NULL,finished_at=COALESCE(finished_at,NOW())
 WHERE id=$1 AND status IN ('pending','uncertain') AND operator_absorbed_at IS NULL`, bill.id)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n > 0 {
				logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_balance_accounting_review_required reservation=%s", bill.id)
				continue
			}
			// Already-committed balance receipts only reconcile caches via dedup.
		}
		if err := s.replay(ctx, &cmd); err != nil {
			logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_billing_retry_failed reservation=%s", bill.id)
			continue
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE dynamic_quota_requests SET billing_retry_at=NULL
 WHERE id=$1 AND status IN ('settled','rejected')`, bill.id); err != nil {
			return err
		}
	}
	return nil
}
