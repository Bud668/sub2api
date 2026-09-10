package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaRecoveryKeepsHoldsAndSourceOwnership(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	alive, err := s.Begin(WithDynamicQuotaRequestMetadata(ctx, "synthetic", "/v1/responses", 3), 101, 4)
	require.NoError(t, err)
	deadService := NewDynamicSubscriptionService(db, nil, nil, nil)
	dead, err := deadService.Begin(ctx, 102, 4)
	require.NoError(t, err)
	require.NoError(t, dead.MarkDispatched())
	other, err := deadService.Begin(ctx, 201, 5)
	require.NoError(t, err)
	// No sleeps or host-clock inference: expire only synthetic worker leases.
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET lease_until=NOW()-INTERVAL '1 minute'`)
	legacyID := uuid.NewString()
	dynamicExec(t, db, `INSERT INTO dynamic_quota_requests(id,account_id,cycle,api_key_id,hold_standard_usd)
 VALUES($1,4,0,101,2)`, legacyID)
	var before, after float64
	require.NoError(t, db.QueryRow(`SELECT sum(hold_standard_usd) FROM dynamic_quota_requests`).Scan(&before))
	require.NoError(t, s.recoverAccounting(ctx))
	for id, want := range map[string]string{alive.ID: "pending", dead.ID: "uncertain", other.ID: "uncertain", legacyID: "pending"} {
		var status string
		require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, id).Scan(&status))
		require.Equal(t, want, status)
	}
	require.NoError(t, db.QueryRow(`SELECT sum(hold_standard_usd) FROM dynamic_quota_requests WHERE status IN ('pending','uncertain')`).Scan(&after))
	require.Equal(t, before, after, "worker loss never means free/estimated usage")
	var user, sub int64
	var metadata []byte
	require.NoError(t, db.QueryRow(`SELECT owner_user_id,owner_subscription_id,request_context FROM dynamic_quota_requests WHERE id=$1`, alive.ID).Scan(&user, &sub, &metadata))
	require.Equal(t, int64(1), user)
	require.Equal(t, int64(11), sub)
	var request dynamicQuotaMetadata
	require.NoError(t, json.Unmarshal(metadata, &request))
	require.Equal(t, 3, request.Turn)
	require.Equal(t, "synthetic", request.Model)
	// Reassigning a key cannot hide its old subscription's outstanding work.
	dynamicExec(t, db, `UPDATE api_keys SET group_id=8 WHERE id=101`)
	status, err := s.AdminStatus(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 1, status.SubscriptionPendingRequests)
	dispatched, err := s.Begin(ctx, 102, 4)
	require.NoError(t, err)
	require.NoError(t, dispatched.MarkDispatched())
	s.BeginShutdown()
	require.ErrorIs(t, dispatched.MarkDispatched(), ErrDynamicQuotaUnavailable, "shutdown also blocks retries on an existing reservation")
	_, err = s.Begin(ctx, 201, 5)
	require.ErrorIs(t, err, ErrDynamicQuotaUnavailable)
	require.ErrorIs(t, alive.MarkDispatched(), ErrDynamicQuotaUnavailable)
	alive.Finish(nil, errors.New("shutdown before send"), false)
	var outcome string
	require.NoError(t, db.QueryRow(`SELECT outcome FROM dynamic_quota_requests WHERE id=$1`, alive.ID).Scan(&outcome))
	require.Equal(t, "not_forwarded", outcome)
}

func TestDynamicQuotaUnknownUsageIsNotZeroBillOrTransportRefund(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		result *OpenAIForwardResult
		err    error
		want   string
	}{
		{"empty_result", &OpenAIForwardResult{}, errors.New("stream lost"), "uncertain"},
		{"failed_without_usage", &OpenAIForwardResult{OpenAIWSMode: true, UpstreamTerminalEvent: "response.failed"}, nil, "uncertain"},
		{"transport_502", nil, &UpstreamFailoverError{StatusCode: 502}, "uncertain"},
		{"rejected_429", nil, &UpstreamFailoverError{StatusCode: 429}, "rejected"},
		{"known_tokens", &OpenAIForwardResult{RequestID: "resp_partial", Usage: OpenAIUsage{InputTokens: 12, OutputTokens: 3}}, errors.New("stream lost"), "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.Begin(ctx, 101, 4)
			require.NoError(t, err)
			require.NoError(t, r.MarkDispatched())
			r.Finish(tc.result, tc.err, false)
			var status string
			require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&status))
			require.Equal(t, tc.want, status)
			if tc.want == "uncertain" && tc.result != nil {
				require.ErrorIs(t, (&OpenAIGatewayService{}).RecordUsage(ctx, &OpenAIRecordUsageInput{Result: tc.result}), ErrDynamicQuotaUnavailable)
			}
		})
	}
}

func TestDynamicQuotaRecoveryClaimsFrozenReceiptsWithoutActivePolicies(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	cmd := UsageBillingCommand{DynamicQuotaReservationID: r.ID, RequestID: "frozen", APIKeyID: 101, AccountID: 4,
		SubscriptionCost: 0.000078125, UsageLog: &UsageLog{Model: "synthetic", CreatedAt: time.Now().UTC()}}
	cmd.Normalize()
	raw, err := json.Marshal(cmd)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=$2::jsonb,billing_retry_at=NOW()-INTERVAL '1 second' WHERE id=$1`, r.ID, string(raw))
	calls := 0
	s.replay = func(_ context.Context, got *UsageBillingCommand) error {
		calls++
		require.Equal(t, cmd.RequestFingerprint, got.RequestFingerprint)
		require.Equal(t, cmd.SubscriptionCost, got.SubscriptionCost)
		return errors.New("synthetic db failure")
	}
	require.NoError(t, s.recoverAccounting(ctx))
	require.Equal(t, 1, calls)
	require.NoError(t, s.recoverAccounting(ctx))
	require.Equal(t, 1, calls, "retry backoff prevents a hot failure loop")
	var persisted []byte
	require.NoError(t, db.QueryRow(`SELECT billing_receipt FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&persisted))
	require.JSONEq(t, string(raw), string(persisted))
	other, err := s.Begin(ctx, 201, 5)
	require.NoError(t, err)
	otherCmd := cmd
	otherCmd.DynamicQuotaReservationID, otherCmd.AccountID, otherCmd.APIKeyID = other.ID, 5, 201
	otherRaw, err := json.Marshal(otherCmd)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=$2::jsonb,billing_retry_at=NOW()-INTERVAL '1 second' WHERE id=$1`, other.ID, string(otherRaw))
	s.replay = func(_ context.Context, got *UsageBillingCommand) error {
		calls++
		require.Equal(t, r.ID, got.DynamicQuotaReservationID, "source sync cannot claim another source's bill")
		return errors.New("synthetic db failure")
	}
	require.NoError(t, s.recoverBillingReceipts(ctx, 4))
	require.Equal(t, 2, calls, "source sync tries its priced receipt once even while background backoff is pending")
	var otherStillDue bool
	require.NoError(t, db.QueryRow(`SELECT billing_retry_at<NOW() FROM dynamic_quota_requests WHERE id=$1`, other.ID).Scan(&otherStillDue))
	require.True(t, otherStillDue)
}

func TestDynamicQuotaFinishFailureDoesNotRenewADeadRequest(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	require.NoError(t, r.MarkDispatched())
	// Force the finishing UPDATE to fail, then restore the database. The
	// process stays alive, but must not keep this ended request "in flight".
	dynamicExec(t, db, `ALTER TABLE dynamic_quota_requests RENAME COLUMN evidence TO evidence_unavailable`)
	r.Finish(nil, errors.New("upstream disconnected"), false)
	dynamicExec(t, db, `ALTER TABLE dynamic_quota_requests RENAME COLUMN evidence_unavailable TO evidence`)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET lease_until=NOW()-INTERVAL '1 minute' WHERE id=$1`, r.ID)
	require.NoError(t, s.recoverAccounting(ctx))
	var status string
	var hold float64
	require.NoError(t, db.QueryRow(`SELECT status,hold_standard_usd FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&status, &hold))
	require.Equal(t, "uncertain", status)
	require.Positive(t, hold)
}

func TestDynamicQuotaOperatorAbsorptionClosesOnlyCustomerLiability(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	_, held, _, pending, err := dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	require.Positive(t, held)
	require.Equal(t, 1, pending)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET operator_absorbed_at=NOW() WHERE id=$1`, r.ID)
	status, err := s.AdminStatus(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, status.SubscriptionPendingRequests)
	require.Zero(t, status.SubscriptionUncertainRequests)
	require.Zero(t, status.SubscriptionReservedStandardUSD)
	require.Zero(t, status.Policy.ReservedUSD)
	// Clearing customer liability alone must not release physical source headroom.
	_, stillHeld, _, stillPending, err := dynamicPoolTotals(ctx, db, 4)
	require.NoError(t, err)
	require.Equal(t, held, stillHeld)
	require.Equal(t, pending, stillPending)
	// Even an expired lease cannot rewrite the operator's preserved evidence.
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET lease_until=NOW()-INTERVAL '1 minute' WHERE id=$1`, r.ID)
	require.NoError(t, s.recoverAccounting(ctx))
	var expired bool
	require.NoError(t, db.QueryRow(`SELECT lease_until<NOW() FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&expired))
	require.True(t, expired)
	require.ErrorIs(t, r.MarkDispatched(), ErrDynamicQuotaUnavailable)
	r.RejectBeforeForward()
	r.Finish(&OpenAIForwardResult{Usage: OpenAIUsage{InputTokens: 2}}, errors.New("late callback"), false)
	var state string
	var evidence []byte
	require.NoError(t, db.QueryRow(`SELECT status,evidence FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&state, &evidence))
	require.Equal(t, "pending", state)
	require.Empty(t, evidence)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	subID := int64(11)
	err = SettleDynamicQuota(ctx, tx, &UsageBillingCommand{DynamicQuotaReservationID: r.ID, AccountID: 4, APIKeyID: 101, SubscriptionID: &subID})
	require.ErrorIs(t, err, ErrUsageBillingRequestConflict)
	require.NoError(t, tx.Rollback())
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, RejectDuplicateDynamicQuota(ctx, tx, &UsageBillingCommand{DynamicQuotaReservationID: r.ID, AccountID: 4, APIKeyID: 101}))
	require.NoError(t, tx.Commit())
	require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&state))
	require.Equal(t, "pending", state)
	dynamicTestSave(t, s, 11, 4, false)
	var used float64
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&used))
	require.Equal(t, 20.0, used, "operator absorption is neither a charge nor a refund")
	// Legacy pre-opt-in requests have no fixed owner or subscription columns.
	legacyID := uuid.NewString()
	dynamicExec(t, db, `INSERT INTO dynamic_quota_requests(id,account_id,cycle,api_key_id,hold_standard_usd,status,operator_absorbed_at)
 VALUES($1,4,0,103,2,'uncertain',NOW())`, legacyID)
	status, err = s.AdminStatus(ctx, 13)
	require.NoError(t, err)
	require.Zero(t, status.SubscriptionUncertainRequests)
	dynamicTestSave(t, s, 13, 4, true)
	other, err := s.Begin(ctx, 102, 4)
	require.NoError(t, err)
	require.NotNil(t, other)
	status, err = s.AdminStatus(ctx, 12)
	require.NoError(t, err)
	require.Equal(t, 1, status.SubscriptionPendingRequests, "other customers retain their own accounting guard")
	require.Error(t, s.Save(ctx, 12, DynamicSubscriptionInput{Enabled: true, AccountID: 4, Weight: 1, MaxLimitUSD: 700}))
}

func TestDynamicQuotaRecoveryRetriesCachesAfterMoneyCommitted(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	subID := int64(11)
	cmd := UsageBillingCommand{DynamicQuotaReservationID: r.ID, RequestID: "cache-recovery", APIKeyID: 101,
		UserID: 1, AccountID: 4, SubscriptionID: &subID, SubscriptionCost: 1.25, DynamicStandardCost: 0.5,
		UsageLog: &UsageLog{Model: "synthetic", CreatedAt: time.Now().UTC()}}
	raw, err := json.Marshal(cmd)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=$2::jsonb,billing_retry_at=NOW()-INTERVAL '1 second' WHERE id=$1`, r.ID, string(raw))
	calls := 0
	s.replay = func(_ context.Context, _ *UsageBillingCommand) error {
		calls++
		if calls == 1 {
			dynamicTestSettle(t, db, r, 101, subID, 0.5, 1.25)
			return errors.New("synthetic cache failure after commit")
		}
		return nil // Canonical dedup has already applied the money; only caches remain.
	}
	require.NoError(t, s.recoverAccounting(ctx))
	var retry sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT billing_retry_at FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&retry))
	require.True(t, retry.Valid, "a committed bill still needs retry after cache failure")
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_retry_at=NOW()-INTERVAL '1 second' WHERE id=$1`, r.ID)
	require.NoError(t, s.recoverAccounting(ctx))
	require.NoError(t, s.recoverAccounting(ctx))
	require.Equal(t, 2, calls)
	require.NoError(t, db.QueryRow(`SELECT billing_retry_at FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&retry))
	require.False(t, retry.Valid)
	var used float64
	require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&used))
	require.Equal(t, 21.25, used, "cache recovery must not add another monetary delta")
}

func TestDynamicQuotaBalanceReceiptNeedsAuxiliaryAccountingReview(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	r, err := s.Begin(ctx, 101, 4)
	require.NoError(t, err)
	cmd := UsageBillingCommand{DynamicQuotaReservationID: r.ID, RequestID: "balance-review", APIKeyID: 101,
		UserID: 1, AccountID: 4, BalanceCost: 1, UsageLog: &UsageLog{Model: "synthetic"}}
	raw, err := json.Marshal(cmd)
	require.NoError(t, err)
	dynamicExec(t, db, `UPDATE dynamic_quota_requests SET billing_receipt=$2::jsonb,billing_retry_at=NOW()-INTERVAL '1 second' WHERE id=$1`, r.ID, string(raw))
	s.replay = func(context.Context, *UsageBillingCommand) error {
		t.Fatal("balance replay must not bypass separate user-platform quota accounting")
		return nil
	}
	require.NoError(t, s.recoverAccounting(ctx))
	var status, outcome string
	var retry sql.NullTime
	var hold float64
	require.NoError(t, db.QueryRow(`SELECT status,outcome,billing_retry_at,hold_standard_usd FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&status, &outcome, &retry, &hold))
	require.Equal(t, "uncertain", status)
	require.Equal(t, "balance_quota_review", outcome)
	require.False(t, retry.Valid)
	require.Positive(t, hold)
}

func TestDynamicQuotaCommittedBalanceReceiptOnlyReconcilesCaches(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	for _, status := range []string{"settled", "rejected"} {
		t.Run(status, func(t *testing.T) {
			r, err := s.Begin(ctx, 101, 4)
			require.NoError(t, err)
			cmd := UsageBillingCommand{DynamicQuotaReservationID: r.ID, BalanceCost: 1, UsageLog: &UsageLog{Model: "synthetic"}}
			raw, err := json.Marshal(cmd)
			require.NoError(t, err)
			dynamicExec(t, db, `UPDATE dynamic_quota_requests SET status=$2,billing_receipt=$3::jsonb,
 billing_retry_at=NOW()-INTERVAL '1 second' WHERE id=$1`, r.ID, status, string(raw))
			calls := 0
			s.replay = func(_ context.Context, got *UsageBillingCommand) error {
				calls++
				require.Equal(t, r.ID, got.DynamicQuotaReservationID)
				return nil // The canonical dedup path has no monetary effects.
			}
			require.NoError(t, s.recoverAccounting(ctx))
			require.NoError(t, s.recoverAccounting(ctx))
			require.Equal(t, 1, calls)
			var gotStatus string
			var retry sql.NullTime
			require.NoError(t, db.QueryRow(`SELECT status,billing_retry_at FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&gotStatus, &retry))
			require.Equal(t, status, gotStatus)
			require.False(t, retry.Valid)
		})
	}
}

func TestDynamicQuotaRecordUsageFreezesPrivateReceiptAndNoZeroLog(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	billing := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: false}}
	svc.usageBillingRepo = billing
	input := &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{DynamicQuotaReservationID: uuid.NewString(), OpenAIWSMode: true,
			Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 1200, OutputTokens: 300}},
		APIKey: &APIKey{ID: 2, Key: "synthetic-secret-key", User: &User{ID: 1}}, User: &User{ID: 1},
		Account:   &Account{ID: 3, Credentials: map[string]any{"token": "synthetic-secret-token"}},
		UserAgent: "synthetic-private-agent", IPAddress: "192.0.2.1", SessionID: "synthetic-private-session",
	}
	require.NoError(t, svc.RecordUsage(context.Background(), input))
	require.Zero(t, usageRepo.calls, "reserved usage is inserted by the canonical transaction, not separately")
	require.Equal(t, "reservation:"+input.Result.DynamicQuotaReservationID, billing.lastCmd.RequestID)
	require.Equal(t, billing.lastCmd.BalanceCost, billing.lastCmd.UsageLog.ActualCost)
	require.NotNil(t, billing.lastCmd.UsageLog.IPAddress)
	require.NotNil(t, billing.lastCmd.UsageLog.UserAgent)
	require.NotNil(t, billing.lastCmd.UsageLog.SessionID)
	require.Equal(t, input.IPAddress, *billing.lastCmd.UsageLog.IPAddress, "normal usage logs retain their existing metadata")
	require.Equal(t, input.UserAgent, *billing.lastCmd.UsageLog.UserAgent)
	require.Equal(t, input.SessionID, *billing.lastCmd.UsageLog.SessionID)
	raw, err := json.Marshal(billing.lastCmd)
	require.NoError(t, err)
	for _, private := range []string{"synthetic-secret"} {
		require.NotContains(t, string(raw), private)
	}
	billing.err = errors.New("synthetic database failure")
	require.Error(t, svc.RecordUsage(context.Background(), input))
	require.Zero(t, usageRepo.calls, "billing failure must not create an unrecoverable $0 usage row")
}
