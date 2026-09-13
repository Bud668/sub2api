package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIRequestRejectionClientStatus(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			r := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(r)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if streaming {
				c.Header("Content-Type", "text/event-stream")
				c.Writer.WriteHeaderNow()
			}
			err := &service.UpstreamFailoverError{
				StatusCode: 400, ClientStatusCode: 400, Reason: service.OpenAIRequestRejectedReason,
				NextAccountAction: service.NextAccountStop, ClientMessage: "Please revise the prompt",
				ResponseBody: []byte(`{"error":{"type":"invalid_request_error","code":"invalid_prompt","message":"Please revise the prompt"}}`),
			}
			h := &OpenAIGatewayHandler{}
			if anthropic {
				h.handleAnthropicFailoverExhausted(c, err, streaming)
			} else {
				h.handleFailoverExhausted(c, err, streaming)
			}
			if streaming {
				require.Equal(t, 200, r.Code, "an already committed SSE header cannot change")
				require.Contains(t, r.Body.String(), "data:")
			} else {
				require.Equal(t, 400, r.Code)
			}
			require.Contains(t, r.Body.String(), "invalid_request")
			require.Contains(t, r.Body.String(), "Please revise the prompt")
		}
	}
}
