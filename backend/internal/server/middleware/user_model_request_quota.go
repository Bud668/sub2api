package middleware

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func writeUserModelPolicyError(c *gin.Context, err error) {
	status, code, message := infraerrors.Code(err), infraerrors.Reason(err), infraerrors.Message(err)
	var quota *service.ModelRequestQuotaError
	if errors.As(err, &quota) {
		status, code, message = http.StatusTooManyRequests, "MODEL_REQUEST_QUOTA_EXCEEDED", quota.Error()
		c.Header("Retry-After", strconv.Itoa(quota.RetryAfter))
		c.Set(service.OpsUserModelQuotaErrorKey, quota)
	}
	if status < 400 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	if status < 500 {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
		MarkIngressRejected(c, IngressRejectModelNotAllowed)
	}
	c.Header("Cache-Control", "private, no-store")
	// Preserve the protocol's error envelope; the message includes the expiry.
	if strings.Contains(c.Request.URL.Path, "/messages") || strings.Contains(c.Request.URL.Path, "/v1beta") {
		groupModelAllowlistErrorWriter(c)(c, status, message)
	} else {
		body := gin.H{"type": "invalid_request_error", "code": code, "message": message}
		if quota != nil {
			body["type"] = "rate_limit_error"
			body["retry_after"] = quota.RetryAfter
			body["resets_at"] = quota.ResetsAt
		}
		c.JSON(status, gin.H{"error": body})
	}
	c.Abort()
}

func runUserModelRequestQuota(c *gin.Context, policies *service.UserModelPolicyService, key *service.APIKey, models []string) bool {
	// Discovery, polling and token estimation are not generation requests.
	path := c.Request.URL.Path
	if strings.Contains(path, "/realtime") && len(models) == 0 {
		writeUserModelPolicyError(c, infraerrors.BadRequest("MODEL_REQUIRED", "An explicit model is required under user model permissions"))
		return false
	}
	// The built-in search tool is not a model-generation endpoint and has no
	// model selector. Its existing per-call billing/authorization still applies.
	if strings.HasSuffix(path, "/alpha/search") && len(models) == 0 {
		c.Next()
		return true
	}
	if c.Request.Method != http.MethodPost || strings.HasSuffix(path, "/count_tokens") || strings.HasSuffix(path, "/input_tokens") {
		for _, model := range models {
			if r, ok := key.Group.ModelAllowlist.UserPolicy.Rule(model); ok && r.Mode == "limited" && strings.Contains(path, "/realtime") {
				writeUserModelPolicyError(c, infraerrors.BadRequest("MODEL_QUOTA_UNSUPPORTED_TRANSPORT", "Limited models require Responses, Chat Completions or Messages; realtime quota accounting is not supported"))
				return false
			}
		}
		c.Next()
		return true
	}
	if len(models) == 0 {
		// Never let an implicit server-selected model evade a user's rules.
		writeUserModelPolicyError(c, infraerrors.BadRequest("MODEL_REQUIRED", "An explicit model is required under user model permissions"))
		return false
	}
	model := service.CanonicalUserModel(models[0])
	for _, candidate := range models {
		if service.CanonicalUserModel(candidate) != model {
			writeUserModelPolicyError(c, infraerrors.BadRequest("AMBIGUOUS_MODEL", "Conflicting model values are not allowed"))
			return false
		}
	}
	if rule, ok := key.Group.ModelAllowlist.UserPolicy.Rule(model); ok && rule.Mode == "limited" {
		if !(strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/responses/compact") || strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/messages")) {
			writeUserModelPolicyError(c, infraerrors.BadRequest("MODEL_QUOTA_UNSUPPORTED_ENDPOINT", "Request quotas currently support text generation via Responses, Chat Completions and Messages"))
			return false
		}
	}
	reservation, err := policies.Reserve(c.Request.Context(), key.UserID, model)
	if err != nil {
		writeUserModelPolicyError(c, err)
		return false
	}
	state := &service.UserModelRequestEvidence{}
	c.Request = c.Request.WithContext(service.WithUserModelRequestEvidence(c.Request.Context(), state))
	defer func() { reservation.Finish(state.RefundHTTP(c.Writer.Status(), c.Request.Context().Err())) }()
	c.Next()
	return true
}
