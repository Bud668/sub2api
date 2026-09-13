package service

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const OpenAIRequestRejectedReason GatewayFailureReason = "upstream_request_rejected"

// A structured prompt rejection wins over prose such as "try again with a
// different prompt". It is not a transport retry or a cyber-account verdict.
func openAIRequestRejection(payload []byte) bool {
	if !gjson.ValidBytes(payload) {
		return false
	}
	if hit, _, _ := detectOpenAICyberPolicy(payload); hit {
		return false
	}
	for _, path := range []string{"response.error.code", "error.code", "response.error.type", "error.type"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String())) {
		case "invalid_prompt", "content_policy_violation", "content_filter":
			return true
		}
	}
	return false
}

func (e *UpstreamFailoverError) IsOpenAIRequestRejection() bool {
	return e != nil && e.Reason == OpenAIRequestRejectedReason && e.StatusCode == http.StatusBadRequest && !e.ShouldRetryNextAccount()
}

// Buffered endpoints have not written to the client, but may already have
// received model output. Such a failed stream is not proof of zero usage.
func openAIRejectedSSEHasOutput(body string) bool {
	seen := false
	forEachOpenAISSEFrame(body, func(eventType string, data []byte) {
		if seen || eventType == "error" || eventType == "response.failed" {
			return
		}
		if openAIStreamEventTypeIsTerminal(eventType) {
			seen = len(gjson.GetBytes(data, "response.output").Array()) > 0
			return
		}
		seen = openAIStreamDataStartsClientOutput(string(data), eventType)
	})
	return seen
}

// Call only before semantic output. Keep only the error envelope, never the
// upstream response graph. Usage is collected by callers before returning this.
func (s *OpenAIGatewayService) newOpenAIRequestRejection(c *gin.Context, account *Account, payload []byte, requestID string) *UpstreamFailoverError {
	message := truncateString(sanitizeUpstreamErrorMessage(extractOpenAISSEErrorMessage(payload)), 1024)
	if message == "" {
		message = "Upstream rejected this prompt; revise the request before retrying"
	}
	code := truncateString(openAIStreamFailedEventErrorCode(payload), 64)
	if code == "" {
		code = "invalid_prompt"
	}
	body, _ := json.Marshal(gin.H{"error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
	s.recordOpenAIStreamUpstreamError(c, account, false, requestID, "request_rejected", payload, message)
	return &UpstreamFailoverError{
		StatusCode: http.StatusBadRequest, ResponseBody: body,
		Reason: OpenAIRequestRejectedReason, NextAccountAction: NextAccountStop,
		ClientStatusCode: http.StatusBadRequest, ClientMessage: message,
		// A failed response with output but no metering remains uncertain.
		SafeToFailoverAfterWrite: len(gjson.GetBytes(payload, "response.output").Array()) == 0 && len(gjson.GetBytes(payload, "output").Array()) == 0,
	}
}
