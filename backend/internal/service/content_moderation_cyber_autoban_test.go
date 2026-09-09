package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func newCyberAutoBanTestService(t *testing.T, user *User) (*ContentModerationService, *contentModerationTestRepo, *contentModerationTestUserRepo, *contentModerationTestAuthCacheInvalidator) {
	t.Helper()
	cfg := defaultContentModerationConfig()
	cfg.AutoBanEnabled = false
	cfg.CyberPolicyAutoBanEnabled = true
	cfg.CyberPolicyExcludeFromBanCount = true
	cfg.BanThreshold = 1000
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	repo := &contentModerationTestRepo{}
	users := &contentModerationTestUserRepo{user: user}
	invalidator := &contentModerationTestAuthCacheInvalidator{}
	svc := NewContentModerationService(&contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw),
	}}, repo, nil, nil, users, nil, invalidator, nil)
	return svc, repo, users, invalidator
}

func TestCyberAutoBan_FirstHitOnlyAndAdminRestore(t *testing.T) {
	svc, repo, users, invalidator := newCyberAutoBanTestService(t, &User{ID: 17, Role: RoleUser, Status: StatusActive, Balance: 123.45})
	err := svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{UserID: 17, Model: "test-model"})
	require.NoError(t, err)
	require.Equal(t, StatusDisabled, users.user.Status)
	require.Equal(t, 123.45, users.user.Balance)
	require.Equal(t, []int64{17}, invalidator.userIDs)
	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.True(t, logs[0].AutoBanned)
	require.Equal(t, 1, logs[0].ViolationCount)
	require.Equal(t, ContentModerationActionCyberPolicy, logs[0].Action)
	// Repeated events are idempotent; restoring a user does not replay old logs.
	require.NoError(t, svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{UserID: 17, Model: "test-model"}))
	require.Len(t, users.updated, 1)
	users.user.Status = StatusActive
	keys := &APIKeyService{userRepo: users}
	_, err = keys.ValidateUserActive(context.Background(), 17)
	require.NoError(t, err)
	require.Len(t, users.updated, 1)
}

func TestCyberAutoBan_DoesNotBanKeywordOrOtherModerationHits(t *testing.T) {
	svc, _, users, _ := newCyberAutoBanTestService(t, &User{ID: 17, Role: RoleUser, Status: StatusActive})
	runtime, err := svc.loadRuntimeSnapshot(context.Background())
	require.NoError(t, err)
	for _, action := range []string{ContentModerationActionKeywordBlock, ContentModerationActionHashBlock, ContentModerationActionBlock} {
		uid := int64(17)
		log := &ContentModerationLog{UserID: &uid, Flagged: true, Action: action}
		svc.persistContentModerationLog(context.Background(), runtime.config, log, "", false, true)
		require.False(t, log.AutoBanned, action)
	}
	require.Empty(t, users.updated)
	require.Equal(t, StatusActive, users.user.Status)
}

func TestCyberAutoBan_AdminRemainsAvailableForRecovery(t *testing.T) {
	svc, repo, users, _ := newCyberAutoBanTestService(t, &User{ID: 1, Role: RoleAdmin, Status: StatusActive})
	require.NoError(t, svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{UserID: 1, Model: "test-model"}))
	require.Empty(t, users.updated)
	require.False(t, repo.snapshotLogs()[0].AutoBanned)
}

type cyberBanFailingUserRepo struct{ *contentModerationTestUserRepo }

func (r cyberBanFailingUserRepo) Update(context.Context, *User, UserUpdateFields) error {
	return errors.New("simulated database write failure")
}

func TestCyberAutoBan_WriteFailurePausesEveryKeyAndWSWithoutClaimingSuccess(t *testing.T) {
	svc, repo, users, _ := newCyberAutoBanTestService(t, &User{ID: 17, Role: RoleUser, Status: StatusActive})
	svc.userRepo = cyberBanFailingUserRepo{users}
	keys := &APIKeyService{userRepo: users}
	svc.authCacheInvalidator = keys
	require.Error(t, svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{UserID: 17, Model: "test-model"}))
	require.Equal(t, StatusActive, users.user.Status)
	require.False(t, repo.snapshotLogs()[0].AutoBanned)
	for _, key := range []string{"test-key-a", "test-key-b"} {
		_, err := keys.GetByKey(context.Background(), key)
		require.ErrorIs(t, err, ErrCyberUserBanUnavailable)
	}
	_, err := keys.ValidateUserActive(context.Background(), 17)
	require.ErrorIs(t, err, ErrCyberUserBanUnavailable)
}

func TestCyberAutoBan_DefaultDisabledAndSettingRoundTrip(t *testing.T) {
	require.False(t, defaultContentModerationConfig().CyberPolicyAutoBanEnabled)
	svc, _, _, _ := newCyberAutoBanTestService(t, &User{ID: 17, Role: RoleUser, Status: StatusActive})
	off := false
	view, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{CyberPolicyAutoBanEnabled: &off})
	require.NoError(t, err)
	require.False(t, view.CyberPolicyAutoBanEnabled)
}
