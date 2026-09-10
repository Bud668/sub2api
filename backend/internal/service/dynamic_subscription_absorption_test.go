package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaAbsorptionReportCountsAllMatchingRecords(t *testing.T) {
	s, db := dynamicTestStore(t)
	dynamicExec(t, db, `ALTER TABLE users ADD COLUMN email TEXT DEFAULT 'synthetic@example.invalid';
 ALTER TABLE groups ADD COLUMN name TEXT DEFAULT 'Synthetic group';
 INSERT INTO dynamic_quota_pools(account_id) VALUES(4),(5);
 INSERT INTO dynamic_quota_requests(id,account_id,cycle,api_key_id,owner_user_id,owner_subscription_id,hold_standard_usd,status,operator_absorbed_at,operator_absorbed_standard_usd)
 VALUES('00000000-0000-4000-8000-000000000001',4,1,101,1,11,99,'uncertain',NOW(),1.25),
 ('00000000-0000-4000-8000-000000000002',4,1,102,2,12,99,'uncertain',NOW(),NULL),
 ('00000000-0000-4000-8000-000000000003',5,1,201,1,21,99,'uncertain',NOW(),2.5);
 UPDATE api_keys SET user_id=2,group_id=8 WHERE id=101;`)
	f := DynamicAbsorptionFilter{Scope: "current", Page: 1, PageSize: 1}
	r, err := s.AbsorptionReport(context.Background(), f)
	require.NoError(t, err)
	require.EqualValues(t, 3, r.Summary.Requests)
	require.Equal(t, 3.75, r.Summary.KnownStandardUSD)
	require.EqualValues(t, 1, r.Summary.UnknownRequests)
	require.Len(t, r.Items, 1)
	require.EqualValues(t, 3, r.Pages)
	f.UserID = 1
	f.GroupID = 7
	r, err = s.AbsorptionReport(context.Background(), f)
	require.NoError(t, err)
	require.EqualValues(t, 1, r.Summary.Requests)
	require.Equal(t, 1.25, r.Summary.KnownStandardUSD)
	require.EqualValues(t, 1, r.Items[0].UserID, "fixed owner must survive a key reassignment")
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET source_closed_at=NOW() WHERE account_id=4`)
	f.UserID = 0
	f.GroupID = 0
	f.SummaryOnly = true
	r, err = s.AbsorptionReport(context.Background(), f)
	require.NoError(t, err)
	require.EqualValues(t, 1, r.Summary.Requests)
	require.Empty(t, r.Items)
	f.Scope = "history"
	r, err = s.AbsorptionReport(context.Background(), f)
	require.NoError(t, err)
	require.EqualValues(t, 2, r.Summary.Requests)
	require.Equal(t, 1.25, r.Summary.KnownStandardUSD)
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "billing_receipt")
	dynamicExec(t, db, `UPDATE user_subscriptions SET expires_at=NOW()-INTERVAL '1 day' WHERE id=11;
 UPDATE user_subscriptions SET deleted_at=NOW() WHERE id=12;`)
	f.Status = "expired"
	r, err = s.AbsorptionReport(context.Background(), f)
	require.NoError(t, err)
	require.EqualValues(t, 1, r.Summary.Requests)
	f.Status = "revoked"
	r, err = s.AbsorptionReport(context.Background(), f)
	require.NoError(t, err)
	require.EqualValues(t, 1, r.Summary.Requests)
	f.Status = "active"
	r, err = s.AbsorptionReport(context.Background(), f)
	require.NoError(t, err)
	require.Zero(t, r.Summary.Requests)
	f.Status = ""
	f.Platform = "anthropic"
	r, err = s.AbsorptionReport(context.Background(), f)
	require.NoError(t, err)
	require.Zero(t, r.Summary.Requests)
}

func TestDynamicQuotaAbsorptionRecoveryAndResetIsolation(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	dynamicTestSave(t, s, 21, 5, true)
	old, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	other, err := s.Begin(ctx, 201, 5)
	require.NoError(t, err)
	live, err := s.Begin(ctx, 102, 4)
	require.NoError(t, err)
	require.NoError(t, old.MarkDispatched())
	old.Finish(nil, context.Canceled, false)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET finished_at=NOW()-INTERVAL '6 minutes' WHERE id=$1`, old.ID)
	require.NoError(t, s.recoverAccounting(ctx))
	var waived bool
	require.NoError(t, db.QueryRow(`SELECT operator_absorbed_at IS NOT NULL FROM dynamic_quota_requests WHERE id=$1`, old.ID).Scan(&waived))
	require.True(t, waived)
	_, held, _, _, err := dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	require.Positive(t, held)
	// First fresh sample cannot release old holds. The next independent sample
	// closes only this source, even if another healthy turn is still executing.
	now := time.Now().UTC()
	reset := now.Add(7*24*time.Hour - time.Minute)
	fetched := now.Add(-50 * time.Second)
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET state=jsonb_set(state,'{snapshot,fetched_at}',to_jsonb($2::text)) WHERE account_id=$1`, 4, now.Add(-2*time.Minute).Format(time.RFC3339Nano))
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		return dynamicTestObservation(id, 0, reset, fetched), nil
	}
	require.NoError(t, s.Refresh(ctx, 4))
	require.NoError(t, db.QueryRow(`SELECT source_closed_at IS NOT NULL FROM dynamic_quota_requests WHERE id=$1`, old.ID).Scan(&waived))
	require.False(t, waived)
	fetched = now
	// Failure after absorption and counter updates must roll the entire cutover
	// back, not leave old customer liability archived with an unchanged cycle.
	dynamicExec(t, db, `ALTER TABLE dynamic_quota_events ADD CONSTRAINT synthetic_reset_failure CHECK(kind<>'reset_confirmed')`)
	require.Error(t, s.Refresh(ctx, 4))
	require.NoError(t, db.QueryRow(`SELECT operator_absorbed_at IS NOT NULL FROM dynamic_quota_requests WHERE id=$1`, live.ID).Scan(&waived))
	require.False(t, waived)
	require.NoError(t, db.QueryRow(`SELECT source_closed_at IS NOT NULL FROM dynamic_quota_requests WHERE id=$1`, old.ID).Scan(&waived))
	require.False(t, waived)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.EqualValues(t, 1, q.Cycle)
	require.Equal(t, 20.0, q.UsedUSD)
	dynamicExec(t, db, `ALTER TABLE dynamic_quota_events DROP CONSTRAINT synthetic_reset_failure`)
	require.NoError(t, s.Refresh(ctx, 4))
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.EqualValues(t, 2, q.Cycle)
	require.Zero(t, q.UsedUSD)
	require.NoError(t, db.QueryRow(`SELECT source_closed_at IS NOT NULL FROM dynamic_quota_requests WHERE id=$1`, old.ID).Scan(&waived))
	require.True(t, waived)
	require.NoError(t, db.QueryRow(`SELECT source_closed_at IS NOT NULL FROM dynamic_quota_requests WHERE id=$1`, other.ID).Scan(&waived))
	require.False(t, waived)
	_, held, _, _, err = dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	require.Positive(t, held, "still executing is physical load, not old debt")
	live.Finish(&OpenAIForwardResult{RequestID: "late", Usage: OpenAIUsage{InputTokens: 2}}, nil, true)
	_, held, _, _, err = dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	require.Zero(t, held)
	require.ErrorIs(t, live.MarkDispatched(), ErrDynamicQuotaUnavailable)
	q, err = s.Load(ctx, 21)
	require.NoError(t, err)
	require.EqualValues(t, 1, q.Cycle)
	require.Equal(t, 20.0, q.UsedUSD)
	fetched = now.Add(time.Second)
	require.NoError(t, s.Refresh(ctx, 4))
	var events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE kind='reset_confirmed'`).Scan(&events))
	require.Equal(t, 1, events)
}

func TestDynamicQuotaAbsorptionKnownReceiptIsNotAnEstimatedHold(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	c := UsageBillingCommand{DynamicQuotaReservationID: r.ID, RequestID: uuid.NewString(), APIKeyID: 101, UserID: 1, AccountID: 4, DynamicStandardCost: 1.25, SubscriptionCost: 2.5,
		UsageLog: &UsageLog{APIKeyID: 101, UserID: 1, AccountID: 4, TotalCost: 1.25, ActualCost: 2.5}}
	c.UsageLog.RequestID = c.RequestID
	c.Normalize()
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	dynamicExec(t, db, `CREATE TABLE usage_billing_dedup(request_id TEXT,api_key_id BIGINT);CREATE TABLE usage_billing_dedup_archive(request_id TEXT,api_key_id BIGINT);
 UPDATE dynamic_quota_requests SET hold_standard_usd=999 WHERE account_id=4;`)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=$2::jsonb WHERE id=$1`, r.ID, string(raw))
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	p, err := lockDynamicPool(ctx, tx, 4)
	require.NoError(t, err)
	require.NoError(t, absorbDynamicRequests(ctx, tx, 4, p.Cycle, true))
	require.NoError(t, tx.Commit())
	var amount, used float64
	require.NoError(t, db.QueryRow(`SELECT operator_absorbed_standard_usd FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&amount))
	require.Equal(t, 1.25, amount)
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&used))
	require.Equal(t, 20.0, used)
	for _, table := range []string{"usage_billing_dedup", "usage_billing_dedup_archive"} {
		alreadyBilled, err := s.Begin(ctx, 101, 4)
		require.NoError(t, err)
		c.DynamicQuotaReservationID, c.RequestID = alreadyBilled.ID, uuid.NewString()
		c.UsageLog.RequestID = c.RequestID
		raw, err = json.Marshal(c)
		require.NoError(t, err)
		dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=$2::jsonb WHERE id=$1`, alreadyBilled.ID, string(raw))
		dynamicExec(t, db, `INSERT INTO `+table+`(request_id,api_key_id) VALUES($1,$2)`, c.RequestID, c.APIKeyID)
		tx, err = db.BeginTx(ctx, nil)
		require.NoError(t, err)
		p, err = lockDynamicPool(ctx, tx, 4)
		require.NoError(t, err)
		require.NoError(t, absorbDynamicRequests(ctx, tx, 4, p.Cycle, true))
		require.NoError(t, tx.Commit())
		var reason string
		require.NoError(t, db.QueryRow(`SELECT operator_absorbed_standard_usd,operator_absorption_reason FROM dynamic_quota_requests WHERE id=$1`, alreadyBilled.ID).Scan(&amount, &reason))
		require.Zero(t, amount, "canonical billing is not an additional cost absorbed by the site")
		require.Equal(t, "already_billed", reason)
	}
}
