package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Both HTTP and WS quota denials use the existing Ops queue. Keep admission/SLA
// exclusion and record only local metadata, never upstream context or prompts.
func logOpsUserModelQuotaRejected(c *gin.Context, ops *service.OpsService, quota *service.ModelRequestQuotaError) {
	apiKey := getOpsAPIKey(c)
	if apiKey == nil || apiKey.UserID <= 0 || c.Request == nil || c.Request.URL == nil {
		return
	}
	requestType := int16(service.RequestTypeSync)
	if service.GetOpenAIClientTransport(c) == service.OpenAIClientTransportWS {
		requestType = int16(service.RequestTypeWSV2)
	} else if body, ok := c.Request.Body.(*httputil.PrereadBody); ok && gjson.GetBytes(body.Bytes(), "stream").Bool() {
		requestType = int16(service.RequestTypeStream)
	}
	body, _ := json.Marshal(gin.H{"error": gin.H{
		"type": "rate_limit_error", "code": "MODEL_REQUEST_QUOTA_EXCEEDED",
		"message": quota.Error(), "model": quota.Model,
		"retry_after": quota.RetryAfter, "resets_at": quota.ResetsAt,
	}})
	requestID, _ := c.Request.Context().Value(ctxkey.RequestID).(string)
	if requestID == "" {
		requestID = c.Writer.Header().Get("X-Request-Id")
	}
	entry := &service.OpsInsertErrorLogInput{
		RequestID: requestID, UserID: &apiKey.UserID, APIKeyID: &apiKey.ID, GroupID: apiKey.GroupID,
		Platform: resolveOpsPlatform(c.Request.Context(), apiKey, guessPlatformFromPath(c.Request.URL.Path)),
		Model:    quota.Model, RequestedModel: quota.Model, RequestPath: c.Request.URL.Path,
		InboundEndpoint: GetInboundEndpoint(c), RequestType: &requestType,
		Stream:     requestType != int16(service.RequestTypeSync),
		ErrorPhase: "request", ErrorType: "model_request_quota_exceeded",
		Severity: "P3", StatusCode: http.StatusTooManyRequests, IsBusinessLimited: true,
		ErrorMessage: quota.Error(), ErrorBody: string(body),
		ErrorSource: "gateway", ErrorOwner: "client", CreatedAt: time.Now(),
	}
	enqueueOpsErrorLog(ops, entry)
}
