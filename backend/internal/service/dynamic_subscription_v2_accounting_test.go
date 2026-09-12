package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func dynamicAccountingTestStore(t *testing.T) (*DynamicSubscriptionService, *sql.DB) {
	t.Helper()
	s, db := dynamicTestStore(t)
	dynamicExec(t, db, `CREATE TABLE usage_billing_dedup(request_id TEXT,api_key_id BIGINT);
 CREATE TABLE usage_billing_dedup_archive(request_id TEXT,api_key_id BIGINT);
 ALTER TABLE users ADD COLUMN email TEXT DEFAULT 'synthetic@example.invalid';
 ALTER TABLE groups ADD COLUMN name TEXT DEFAULT 'Synthetic group';`)
	return s, db
}

func dynamicAccountingException(t *testing.T, s *DynamicSubscriptionService, key, sub int64, price *float64) *UsageBillingCommand {
	t.Helper()
	r, err := s.Begin(context.Background(), key, 4)
	require.NoError(t, err)
	require.NoError(t, r.MarkDispatched())
	r.Finish(nil, context.Canceled, false)
	c := &UsageBillingCommand{DynamicQuotaReservationID: r.ID, RequestID: uuid.NewString(), APIKeyID: key,
		UserID: sub - 10, AccountID: 4, SubscriptionID: &sub}
	var raw any
	if price != nil {
		c.DynamicStandardCost, c.SubscriptionCost = *price, *price*2
		c.UsageLog = &UsageLog{RequestID: c.RequestID, APIKeyID: key, UserID: c.UserID, AccountID: 4,
			SubscriptionID: &sub, TotalCost: *price, ActualCost: *price * 2, Model: "synthetic"}
		c.Normalize()
		b, err := json.Marshal(c)
		require.NoError(t, err)
		raw = string(b)
	}
	dynamicExec(t, s.db, `UPDATE dynamic_quota_requests SET finished_at=NOW()-INTERVAL '6 minutes',
 billing_receipt=$2::jsonb,hold_standard_usd=999,
 request_context=request_context-'settlement_policy' WHERE id=$1`, r.ID, raw)
	return c
}

func TestDynamicQuotaV2AccountingThresholdsAndUnknowns(t *testing.T) {
	s, db := dynamicAccountingTestStore(t)
	ctx := context.Background()
	price := 4.0
	for i := 0; i < 5; i++ { // All users/keys share the same source-cycle threshold.
		key, sub := int64(101+i%2), int64(11+i%2)
		dynamicAccountingException(t, s, key, sub, &price)
		require.NoError(t, s.absorbExpiredEvidence(ctx))
	}
	for i := 0; i < 5; i++ {
		dynamicAccountingException(t, s, 101, 11, nil)
		require.NoError(t, s.absorbExpiredEvidence(ctx))
	}
	var covered, reviewed int
	var amount float64
	require.NoError(t, db.QueryRow(`SELECT count(*) FILTER(WHERE operator_absorbed_at IS NOT NULL),
 count(*) FILTER(WHERE review_required_at IS NOT NULL),sum(operator_absorbed_standard_usd) FROM dynamic_quota_requests`).Scan(&covered, &reviewed, &amount))
	require.Equal(t, 4, covered)
	require.Equal(t, 6, reviewed)
	require.Equal(t, 16.0, amount, "a 999-dollar hold is never a priced bill")
	var alert int
	require.NoError(t, db.QueryRow(`SELECT sum((details->>'alert')::int) FROM dynamic_quota_events WHERE kind='accounting_classified'`).Scan(&alert))
	require.Equal(t, 1, alert, "escalation is notified once for the whole source-cycle")
	require.NoError(t, s.absorbExpiredEvidence(ctx))
	var events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE kind='accounting_classified'`).Scan(&events))
	require.Equal(t, 10, events, "repeated scans cannot reclassify or notify again")
	f := DynamicAbsorptionFilter{Scope: "current", Category: "review", Page: 1, PageSize: 20}
	report, err := s.AbsorptionReport(ctx, f)
	require.NoError(t, err)
	require.EqualValues(t, 6, report.Summary.Requests)
	require.EqualValues(t, 5, report.Summary.UnknownRequests)
	for _, row := range report.Items {
		if row.KnownStandardUSD == nil {
			require.False(t, row.CanCharge)
			require.Nil(t, row.ChargeUSD)
		} else {
			require.True(t, row.CanCharge)
			require.Equal(t, 8.0, *row.ChargeUSD, "the button must show the frozen actual customer price")
		}
	}
	for _, amount := range []float64{5, 5.01} {
		require.False(t, dynamicV2MayAbsorb(&amount, 0, 0))
	}
	require.False(t, dynamicV2MayAbsorb(nil, 0, 0))
}

func TestDynamicQuotaV2ReviewChargeCoverAndFencing(t *testing.T) {
	s, db := dynamicAccountingTestStore(t)
	ctx := context.Background()
	price := 5.0
	charge := dynamicAccountingException(t, s, 101, 11, &price)
	unknown := dynamicAccountingException(t, s, 102, 12, nil)
	require.NoError(t, s.absorbExpiredEvidence(ctx))
	s.replay = func(ctx context.Context, c *UsageBillingCommand) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = SettleDynamicQuota(ctx, tx, c); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE user_subscriptions SET weekly_usage_usd=weekly_usage_usd+$2 WHERE id=$1`, *c.SubscriptionID, c.SubscriptionCost); err != nil {
			return err
		}
		return tx.Commit()
	}
	// An automatic/late replay cannot silently resolve a reviewed bill.
	require.ErrorIs(t, s.replay(ctx, charge), ErrUsageBillingRequestConflict)
	require.ErrorIs(t, s.ResolveAccounting(ctx, unknown.DynamicQuotaReservationID, 1, "charge"), ErrDynamicQuotaUnavailable)
	require.NoError(t, s.ResolveAccounting(ctx, charge.DynamicQuotaReservationID, 1, "charge"))
	require.ErrorIs(t, s.ResolveAccounting(ctx, charge.DynamicQuotaReservationID, 1, "charge"), ErrDynamicQuotaChanged)
	var used float64
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&used))
	require.Equal(t, 30.0, used)
	require.NoError(t, s.ResolveAccounting(ctx, unknown.DynamicQuotaReservationID, 2, "cover"))
	require.ErrorIs(t, s.ResolveAccounting(ctx, unknown.DynamicQuotaReservationID, 2, "cover"), ErrDynamicQuotaChanged)
	// An expired manual replay is fenced even if the next action uses the same admin.
	stale := dynamicAccountingException(t, s, 101, 11, &price)
	require.NoError(t, s.absorbExpiredEvidence(ctx))
	started, resume := make(chan struct{}), make(chan struct{})
	canonical := s.replay
	s.replay = func(ctx context.Context, c *UsageBillingCommand) error {
		close(started)
		<-resume
		return canonical(ctx, c)
	}
	done := make(chan error, 1)
	go func() { done <- s.ResolveAccounting(ctx, stale.DynamicQuotaReservationID, 1, "charge") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("review never started")
	}
	require.ErrorIs(t, s.ResolveAccounting(ctx, stale.DynamicQuotaReservationID, 2, "cover"), ErrDynamicQuotaChanged)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_retry_at=NOW()-INTERVAL '1 second' WHERE id=$1`, stale.DynamicQuotaReservationID)
	require.NoError(t, s.ResolveAccounting(ctx, stale.DynamicQuotaReservationID, 1, "cover"))
	close(resume)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("replay did not finish")
	}
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&used))
	require.Equal(t, 30.0, used, "stale workers cannot charge a covered request")
}

func TestDynamicQuotaV2LiveRequestsAndFalseReplaySuccess(t *testing.T) {
	s, db := dynamicAccountingTestStore(t)
	ctx := context.Background()
	live, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET started_at=NOW()-INTERVAL '2 hours' WHERE id=$1`, live.ID)
	require.NoError(t, s.absorbExpiredEvidence(ctx))
	var changed bool
	require.NoError(t, db.QueryRow(`SELECT review_required_at IS NOT NULL OR operator_absorbed_at IS NOT NULL FROM dynamic_quota_requests WHERE id=$1`, live.ID).Scan(&changed))
	require.False(t, changed)
	price := 5.0
	c := dynamicAccountingException(t, s, 102, 12, &price)
	require.NoError(t, s.absorbExpiredEvidence(ctx))
	s.replay = func(context.Context, *UsageBillingCommand) error { return nil }
	require.ErrorIs(t, s.ResolveAccounting(ctx, c.DynamicQuotaReservationID, 1, "charge"), ErrDynamicQuotaUnavailable)
	// Resolving accounting never grants a new cycle or assigns an unproven price.
	q := DynamicSubscriptionInput{AccountID: 4, Weight: 1, MaxLimitUSD: 600, FloorLimitUSD: &price, Enabled: true}
	require.NoError(t, s.Save(ctx, 12, q), "a review does not block configuration")
}

func TestDynamicQuotaV2SmallWaiversRequireMatchingSubscriptionBilling(t *testing.T) {
	for _, kind := range []string{"balance", "other_subscription"} {
		t.Run(kind, func(t *testing.T) {
			s, db := dynamicAccountingTestStore(t)
			ctx := context.Background()
			price := 1.0
			c := dynamicAccountingException(t, s, 101, 11, &price)
			if kind == "balance" {
				c.BalanceCost = c.SubscriptionCost
				c.SubscriptionCost = 0
			} else {
				other := int64(12)
				c.SubscriptionID, c.UsageLog.SubscriptionID = &other, &other
			}
			raw, err := json.Marshal(c)
			require.NoError(t, err)
			dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=$2::jsonb WHERE id=$1`, c.DynamicQuotaReservationID, string(raw))
			require.NoError(t, s.absorbExpiredEvidence(ctx))
			report, err := s.AbsorptionReport(ctx, DynamicAbsorptionFilter{Scope: "current", Category: "review", Page: 1, PageSize: 20})
			require.NoError(t, err)
			require.Len(t, report.Items, 1, "small amounts still require correct billing scope")
			require.Equal(t, "receipt_scope", report.Items[0].Reason)
			require.False(t, report.Items[0].CanCharge)
			require.ErrorIs(t, s.ResolveAccounting(ctx, c.DynamicQuotaReservationID, 1, "charge"), ErrDynamicQuotaUnavailable)
		})
	}
}
