package handler

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
)

// A WebSocket outlives its handshake authentication. Reuse the key service's
// fresh user lookup before every request, including the first turn and failover.
func (h *OpenAIGatewayHandler) checkOpenAIWSCyberUserAccess(ctx context.Context, key *service.APIKey) error {
	if h.apiKeyService == nil || key == nil {
		return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "user authorization unavailable", nil)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	userID := key.UserID
	if userID <= 0 && key.User != nil {
		userID = key.User.ID
	}
	_, err := h.apiKeyService.ValidateUserActive(checkCtx, userID)
	if err == nil {
		return nil
	}
	if errors.Is(err, service.ErrUserNotActive) || errors.Is(err, service.ErrAPIKeyNotFound) {
		return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "user or API key is inactive; contact the administrator", err)
	}
	return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "user authorization unavailable; forwarding paused", err)
}
