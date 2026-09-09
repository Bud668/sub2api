package handler

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type openAIWSCyberUserRepo struct {
	service.UserRepository
	mu       sync.RWMutex
	disabled bool
	fail     bool
}

func (r *openAIWSCyberUserRepo) GetByID(_ context.Context, id int64) (*service.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.fail {
		return nil, errors.New("simulated database outage")
	}
	status := service.StatusActive
	if r.disabled {
		status = service.StatusDisabled
	}
	return &service.User{ID: id, Role: service.RoleUser, Status: status}, nil
}

func (r *openAIWSCyberUserRepo) Update(_ context.Context, user *service.User, fields service.UserUpdateFields) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("simulated database outage")
	}
	if fields.Status {
		r.disabled = user.Status == service.StatusDisabled
	}
	return nil
}

func (r *openAIWSCyberUserRepo) setDisabled(disabled bool) {
	r.mu.Lock()
	r.disabled = disabled
	r.mu.Unlock()
}

func (r *openAIWSCyberUserRepo) isDisabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.disabled
}

func newOpenAIWSUserAuthTestService(cfg *config.Config) *service.APIKeyService {
	return service.NewAPIKeyService(nil, &openAIWSCyberUserRepo{}, nil, nil, nil, nil, cfg)
}

func TestCyberUserAccessRevalidatesEveryKeyAndFailsClosed(t *testing.T) {
	users := &openAIWSCyberUserRepo{}
	keys := service.NewAPIKeyService(nil, users, nil, nil, nil, nil, nil)
	h := &OpenAIGatewayHandler{apiKeyService: keys}
	for _, keyID := range []int64{1, 2} {
		key := &service.APIKey{ID: keyID, UserID: 17, User: &service.User{ID: 17, Status: service.StatusActive}}
		require.NoError(t, h.checkOpenAIWSCyberUserAccess(context.Background(), key))
		users.setDisabled(true)
		require.ErrorContains(t, h.checkOpenAIWSCyberUserAccess(context.Background(), key), "inactive")
		users.setDisabled(false) // Explicit administrator restoration.
		require.NoError(t, h.checkOpenAIWSCyberUserAccess(context.Background(), key))
	}
	key := &service.APIKey{ID: 1, UserID: 17}
	users.mu.Lock()
	users.fail = true
	users.mu.Unlock()
	require.ErrorContains(t, h.checkOpenAIWSCyberUserAccess(context.Background(), key), "forwarding paused")
	users.mu.Lock()
	users.fail = false
	users.mu.Unlock()
	keys.PauseAfterCyberUserBanFailure()
	require.ErrorContains(t, h.checkOpenAIWSCyberUserAccess(context.Background(), key), "forwarding paused")
}
