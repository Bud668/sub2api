package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaManualResetRequiresIndependentSourceEvidence(t *testing.T) {
	s, db := dynamicTestStore(t)
	ctx := context.Background()
	dynamicTestSave(t, s, 11, 4, true)
	dynamicTestSave(t, s, 21, 5, true)
	now := time.Now().UTC()
	calls := 0
	fetched := now.Add(-40 * time.Second)
	s.fetch = func(_ context.Context, id int64) (DynamicQuotaObservation, error) {
		calls++
		require.Equal(t, int64(4), id)
		return dynamicTestObservation(id, 0, now.Add(7*24*time.Hour-time.Minute), fetched), nil
	}
	preview, err := s.ResetPreview(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, int64(4), preview.AccountID)
	require.Equal(t, 1, preview.Members)
	require.Zero(t, calls, "opening the preview is local and read-only")
	_, err = s.SyncReset(ctx, 11, 5, preview.Cycle)
	require.ErrorIs(t, err, ErrDynamicQuotaChanged)
	require.Zero(t, calls)
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET state=jsonb_set(jsonb_set(state,'{snapshot,fetched_at}',to_jsonb($1::text)),
 '{health,last_attempt_at}',to_jsonb($1::text)) WHERE account_id=4`, now.Add(-2*time.Minute).Format(time.RFC3339Nano))
	result, err := s.SyncReset(ctx, 11, 4, preview.Cycle)
	require.NoError(t, err)
	require.Equal(t, "confirming", result.Status)
	require.Equal(t, 1, calls)
	q, err := s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, 20.0, q.UsedUSD)
	// Rapid retries cannot count the same quota observation twice.
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET state=jsonb_set(state,'{health,last_attempt_at}',to_jsonb($1::text)) WHERE account_id=4`, now.Format(time.RFC3339Nano))
	result, err = s.SyncReset(ctx, 11, 4, preview.Cycle)
	require.NoError(t, err)
	require.Equal(t, "confirming", result.Status)
	require.Equal(t, 1, calls)
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET state=jsonb_set(state,'{health,last_attempt_at}',to_jsonb($1::text)) WHERE account_id=4`, fetched.Format(time.RFC3339Nano))
	fetched = now
	result, err = s.SyncReset(ctx, 11, 4, preview.Cycle)
	require.NoError(t, err)
	require.Equal(t, "reset", result.Status)
	require.Equal(t, preview.Cycle+1, result.Cycle)
	require.Equal(t, 2, calls)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, q.UsedUSD)
	other, err := s.Load(ctx, 21)
	require.NoError(t, err)
	require.Equal(t, int64(1), other.Cycle)
	require.Equal(t, 20.0, other.UsedUSD)
	_, err = s.SyncReset(ctx, 11, 4, preview.Cycle)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "a completed preview cannot start another reset")
	var events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dynamic_quota_events WHERE kind='reset_confirmed'`).Scan(&events))
	require.Equal(t, 1, events)
	s.fetch = func(context.Context, int64) (DynamicQuotaObservation, error) {
		return DynamicQuotaObservation{}, errors.New("unavailable")
	}
	dynamicExec(t, db, `UPDATE dynamic_quota_pools SET state=jsonb_set(state,'{health,last_attempt_at}',to_jsonb($1::text)) WHERE account_id=4`, now.Add(-time.Minute).Format(time.RFC3339Nano))
	_, err = s.SyncReset(ctx, 11, 4, result.Cycle)
	require.Error(t, err)
	q, err = s.Load(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, result.Cycle, q.Cycle, "fetch failures never fabricate a new cycle")
}
