package handler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func writeUserModelPolicyWSError(c *gin.Context, ctx context.Context, conn *coderws.Conn, err error) error {
	code, message, status := infraerrors.Reason(err), infraerrors.Message(err), infraerrors.Code(err)
	entry := map[string]any{"type": "invalid_request_error", "code": code, "message": message}
	var quota *service.ModelRequestQuotaError
	if errors.As(err, &quota) {
		status = 429
		entry = map[string]any{"type": "rate_limit_error", "code": "MODEL_REQUEST_QUOTA_EXCEEDED", "message": quota.Error(), "retry_after": quota.RetryAfter, "resets_at": quota.ResetsAt}
		c.Set(service.OpsUserModelQuotaErrorKey, quota)
	}
	if status >= 500 {
		entry["type"] = "server_error"
	} else {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
		middleware.MarkIngressRejected(c, middleware.IngressRejectModelNotAllowed)
	}
	payload, _ := json.Marshal(map[string]any{"type": "error", "status": status, "error": entry})
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if conn != nil {
		_ = conn.Write(writeCtx, coderws.MessageText, payload)
	}
	closeCode := coderws.StatusPolicyViolation
	if status >= 500 {
		closeCode = coderws.StatusInternalError
	}
	// Close only this connection, never disable the user or mark a cyber event.
	return service.NewOpenAIWSClientCloseError(closeCode, "model permission or request quota rejected; see error event", err)
}
