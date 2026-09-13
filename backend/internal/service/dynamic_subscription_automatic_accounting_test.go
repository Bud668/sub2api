package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaAutomaticSettlementKeepsReceiptsAndClosesUnmetered(t *testing.T) {
	s, db := dynamicAccountingTestStore(t)
	ctx := context.Background()
	unknown := dynamicAccountingException(t, s, 101, 11, nil)
	legacy := dynamicAccountingException(t, s, 102, 12, nil)
	for _, amount := range []float64{1, 40} {
		c := dynamicAccountingException(t, s, 101, 11, &amount)
		dynamicExec(t, db, `UPDATE dynamic_quota_requests SET request_context=jsonb_set(request_context,'{settlement_policy}',to_jsonb($2::text)) WHERE id=$1`, c.DynamicQuotaReservationID, automaticSettlementPolicy)
	}
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET request_context=jsonb_set(request_context,'{settlement_policy}',to_jsonb($2::text)) WHERE id=$1`, unknown.DynamicQuotaReservationID, automaticSettlementPolicy)
	require.NoError(t, s.absorbExpiredEvidence(ctx))
	var closed, review bool
	var reason string
	var amount *float64
	require.NoError(t, db.QueryRow(`SELECT operator_absorbed_at IS NOT NULL,review_required_at IS NOT NULL,operator_absorption_reason,operator_absorbed_standard_usd FROM dynamic_quota_requests WHERE id=$1`, unknown.DynamicQuotaReservationID).Scan(&closed, &review, &reason, &amount))
	require.True(t, closed)
	require.False(t, review)
	require.Equal(t, "automatic_unmetered", reason)
	require.Nil(t, amount, "the 999-dollar reservation is not an actual bill")
	require.NoError(t, db.QueryRow(`SELECT operator_absorbed_at IS NOT NULL,review_required_at IS NOT NULL FROM dynamic_quota_requests WHERE id=$1`, legacy.DynamicQuotaReservationID).Scan(&closed, &review))
	require.False(t, closed, "a new policy must not silently write off legacy records")
	require.True(t, review)
	var retrying int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_requests WHERE billing_receipt IS NOT NULL AND billing_retry_at IS NOT NULL AND operator_absorbed_at IS NULL AND review_required_at IS NULL`).Scan(&retrying))
	require.Equal(t, 2, retrying, "valid receipts retry automatically regardless of price")
	require.NoError(t, s.absorbExpiredEvidence(ctx))
	var covered int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_requests WHERE operator_absorbed_at IS NOT NULL`).Scan(&covered))
	require.Equal(t, 1, covered, "classification must be idempotent")
	var used, held float64
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&used))
	require.Equal(t, 20.0, used, "no customer charge or refund without a receipt")
	require.NoError(t, db.QueryRow(`SELECT hold_standard_usd FROM dynamic_quota_requests WHERE id=$1 AND source_closed_at IS NULL`, unknown.DynamicQuotaReservationID).Scan(&held))
	require.Equal(t, 999.0, held, "personal closure must not release physical source capacity")
}

func TestDynamicQuotaAutomaticClosureFencesLateBillsAndInvalidReceipts(t *testing.T) {
	s, db := dynamicAccountingTestStore(t)
	ctx := context.Background()
	price := 8.0
	late := dynamicAccountingException(t, s, 101, 11, &price)
	invalid := dynamicAccountingException(t, s, 102, 12, nil)
	mismatch := dynamicAccountingException(t, s, 101, 11, &price)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET request_context=jsonb_set(request_context,'{settlement_policy}',to_jsonb($1::text))`, automaticSettlementPolicy)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=NULL WHERE id=$1`, late.DynamicQuotaReservationID)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt='{}'::jsonb WHERE id=$1`, invalid.DynamicQuotaReservationID)
	wrongOwner := int64(12)
	mismatch.SubscriptionID, mismatch.UsageLog.SubscriptionID = &wrongOwner, &wrongOwner
	raw, err := json.Marshal(mismatch)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=$2::jsonb WHERE id=$1`, mismatch.DynamicQuotaReservationID, string(raw))
	require.NoError(t, s.absorbExpiredEvidence(ctx))
	for _, c := range []*UsageBillingCommand{late, invalid, mismatch} {
		var closed bool
		var known *float64
		require.NoError(t, db.QueryRow(`SELECT operator_absorbed_at IS NOT NULL,operator_absorbed_standard_usd FROM dynamic_quota_requests WHERE id=$1`, c.DynamicQuotaReservationID).Scan(&closed, &known))
		require.True(t, closed)
		require.Nil(t, known)
	}
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.ErrorIs(t, SettleDynamicQuota(ctx, tx, late), ErrUsageBillingRequestConflict, "a late bill cannot charge a closed request")
	require.NoError(t, tx.Rollback())
	var used float64
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&used))
	require.Equal(t, 20.0, used)
	var alerts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE kind='accounting_classified' AND (details->>'alert')::int>0`).Scan(&alerts))
	require.Equal(t, 1, alerts, "invalid receipts need visible diagnostics, not silent loss")
}

func TestDynamicQuotaAutomaticPolicyDoesNotExpireLiveShutdownWork(t *testing.T) {
	s, db := dynamicAccountingTestStore(t)
	r, err := s.Begin(context.Background(), 101, 4)
	require.NoError(t, err)
	var policy string
	require.NoError(t, db.QueryRow(`SELECT request_context->>'settlement_policy' FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&policy))
	require.Equal(t, automaticSettlementPolicy, policy)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET lease_until=NOW()-INTERVAL '1 minute' WHERE id=$1`, r.ID)
	s.BeginShutdown()
	require.NoError(t, s.recoverAccounting(context.Background()))
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&status))
	require.Equal(t, "pending", status, "draining a live request must continue its lease")
}

func TestDynamicQuotaAutomaticPolicyNeverAbsorbsKnownMetering(t *testing.T) {
	s, db := dynamicAccountingTestStore(t)
	ctx := context.Background()
	price := 3.0
	c := dynamicAccountingException(t, s, 101, 11, &price)
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=NULL,
 evidence='{"usage":{"input_tokens":100,"output_tokens":20}}',
 request_context=jsonb_set(request_context,'{settlement_policy}',to_jsonb($2::text)) WHERE id=$1`, c.DynamicQuotaReservationID, automaticSettlementPolicy)
	for i := 0; i < 2; i++ {
		require.NoError(t, s.absorbExpiredEvidence(ctx))
	}
	var absorbed, reviewed bool
	var outcome string
	require.NoError(t, db.QueryRow(`SELECT operator_absorbed_at IS NOT NULL,review_required_at IS NOT NULL,outcome FROM dynamic_quota_requests WHERE id=$1`, c.DynamicQuotaReservationID).Scan(&absorbed, &reviewed, &outcome))
	require.False(t, absorbed, "real tokens must not become a write-off because pricing was delayed")
	require.False(t, reviewed, "do not block automatic settlement when the valid receipt arrives")
	require.Equal(t, "metered_without_receipt", outcome)
	var events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE kind='accounting_classified'`).Scan(&events))
	require.Equal(t, 1, events)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=$2::jsonb,billing_retry_at=NOW() WHERE id=$1`, c.DynamicQuotaReservationID, string(raw))
	calls := 0
	s.replay = func(ctx context.Context, cmd *UsageBillingCommand) error {
		calls++
		require.Equal(t, 6.0, cmd.SubscriptionCost, "use the frozen customer price, never the 999-dollar hold")
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = SettleDynamicQuota(ctx, tx, cmd); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE user_subscriptions SET weekly_usage_usd=weekly_usage_usd+$2 WHERE id=$1`, *cmd.SubscriptionID, cmd.SubscriptionCost); err != nil {
			return err
		}
		return tx.Commit()
	}
	for i := 0; i < 2; i++ {
		require.NoError(t, s.recoverAccounting(ctx))
	}
	require.Equal(t, 1, calls)
	var used float64
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&used))
	require.Equal(t, 26.0, used, "the real bill is recovered once")
}

func TestDynamicQuotaAutomaticPolicyPreservesNonTokenMetering(t *testing.T) {
	s, db := dynamicAccountingTestStore(t)
	ctx := context.Background()
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	require.NoError(t, r.MarkDispatched())
	r.Finish(&OpenAIForwardResult{SearchCount: 1}, nil, false)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET finished_at=NOW()-INTERVAL '6 minutes' WHERE id=$1`, r.ID)
	require.NoError(t, s.absorbExpiredEvidence(ctx))
	var observed, absorbed bool
	var outcome string
	require.NoError(t, db.QueryRow(`SELECT (evidence->>'has_billable_usage')::boolean,
 operator_absorbed_at IS NOT NULL,outcome FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&observed, &absorbed, &outcome))
	require.True(t, observed)
	require.False(t, absorbed, "non-token billing units are still real metering")
	require.Equal(t, "metered_without_receipt", outcome)
}
