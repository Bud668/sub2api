package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const rejectedPromptEvent = `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_prompt","message":"Invalid prompt: your prompt was flagged as potentially violating our usage policy. Please try again with a different prompt."}}`
const rejectedPromptFailed = `{"type":"response.failed","response":{"id":"resp_rejected","status":"failed","output":[],"error":{"code":"invalid_prompt","message":"Please try again with a different prompt."}}}`
const rejectedPromptMetered = `{"type":"response.failed","response":{"id":"resp_rejected","status":"failed","output":[],"error":{"code":"invalid_prompt","message":"Please try again with a different prompt."},"usage":{"input_tokens":11,"output_tokens":2}}}`

func rejectionSSE(events ...string) string {
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}

func TestOpenAIRequestRejectionClassification(t *testing.T) {
	for _, payload := range []string{rejectedPromptEvent, rejectedPromptFailed, rejectedPromptMetered} {
		require.True(t, openAIRequestRejection([]byte(payload)))
		require.False(t, openAIStreamErrorEventShouldFailover([]byte(payload), extractOpenAISSEErrorMessage([]byte(payload))))
		require.False(t, openAIStreamFailedEventShouldFailover([]byte(payload), extractOpenAISSEErrorMessage([]byte(payload))))
		require.Equal(t, 400, openAIStreamFailureStatus([]byte(payload), "try again"))
	}
	cyber := `{"type":"error","error":{"code":"cyber_policy","type":"invalid_prompt","message":"cyber policy"}}`
	for _, payload := range []string{cyber, `{"type":"error","error":{"code":"server_error","message":"try again"}}`, `{"input":"invalid_prompt"}`, `{bad json`} {
		require.False(t, openAIRequestRejection([]byte(payload)))
	}
	require.True(t, openAIStreamErrorEventShouldFailover([]byte(`{"type":"error","error":{"code":"server_error","message":"try again"}}`), "try again"))
}

// Same production error, every HTTP protocol adapter, with and without real
// metering. Error prose must never authorize replay or substitute an estimate.
func TestOpenAIRequestRejectionAdapters(t *testing.T) {
	for _, route := range []string{"responses_stream", "passthrough_stream", "responses_json", "passthrough_json", "chat_stream", "chat_json", "messages_stream", "messages_json", "ws_http_bridge"} {
		for _, tc := range []struct {
			name, body string
			tokens     int
		}{
			{"bare", rejectionSSE(rejectedPromptEvent), 0},
			{"failed", rejectionSSE(rejectedPromptFailed), 0},
			{"metered", rejectionSSE(rejectedPromptMetered), 11},
			{"bare_then_metered", rejectionSSE(rejectedPromptEvent, rejectedPromptMetered), 11},
			{"bare_with_usage", rejectionSSE(`{"type":"error","error":{"code":"invalid_prompt","message":"try again with another prompt"},"response":{"id":"resp_rejected","usage":{"input_tokens":11,"output_tokens":2}}}`), 11},
			{"zero_terminal_keeps_metering", rejectionSSE(`{"type":"response.in_progress","usage":{"input_tokens":11,"output_tokens":2}}`, strings.Replace(rejectedPromptMetered, `"input_tokens":11,"output_tokens":2`, `"input_tokens":0,"output_tokens":0`, 1)), 11},
			{"preamble_then_rejected", rejectionSSE(`{"type":"response.created","response":{"id":"resp_rejected","model":"gpt-5.4","output":[]}}`, rejectedPromptFailed), 0},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				c, _ := newNonStreamingFailoverContext(t)
				svc := newNonStreamingFailoverService()
				account := newNonStreamingFailoverAccount()
				resp := newNonStreamingSSEResponse()
				resp.Body = io.NopCloser(strings.NewReader(tc.body))
				var usage *OpenAIUsage
				var err error
				switch route {
				case "responses_stream":
					var r *openaiStreamingResult
					r, err = svc.handleStreamingResponseWithReasoning(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.4", "gpt-5.4", "")
					if r != nil {
						usage = r.usage
					}
				case "passthrough_stream":
					var r *openaiStreamingResultPassthrough
					r, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.4", "gpt-5.4")
					if r != nil {
						usage = r.usage
					}
				case "responses_json":
					var r *openaiNonStreamingResult
					r, err = svc.handleSSEToJSON(resp, c, account, []byte(tc.body), "gpt-5.4", "gpt-5.4")
					if r != nil {
						usage = r.usage
					}
				case "passthrough_json":
					var r *openaiNonStreamingResultPassthrough
					r, err = svc.handlePassthroughSSEToJSON(resp, c, account, []byte(tc.body), "gpt-5.4", "gpt-5.4")
					if r != nil {
						usage = r.usage
					}
				default:
					var r *OpenAIForwardResult
					switch route {
					case "chat_stream":
						r, err = svc.handleChatStreamingResponse(resp, c, account, "gpt-5.4", "gpt-5.4", "gpt-5.4", time.Now(), 100)
					case "chat_json":
						r, err = svc.handleChatBufferedStreamingResponse(resp, c, account, "gpt-5.4", "gpt-5.4", "gpt-5.4", time.Now())
					case "messages_stream":
						r, err = svc.handleAnthropicStreamingResponse(resp, c, account, "gpt-5.4", "gpt-5.4", "gpt-5.4", time.Now())
					case "messages_json":
						r, err = svc.handleAnthropicBufferedStreamingResponse(resp, c, account, "gpt-5.4", "gpt-5.4", "gpt-5.4", time.Now())
					case "ws_http_bridge":
						svc.httpUpstream = &httpUpstreamRecorder{resp: resp}
						account.Credentials["base_url"] = "https://synthetic.example.invalid"
						body := []byte(`{"type":"response.create","model":"gpt-5.4","input":"synthetic"}`)
						r, err = svc.proxyOpenAIWSHTTPBridgeTurn(c.Request.Context(), c, account, "synthetic", body, len(body), "gpt-5.4", "", "", "", "", 1, func([]byte) error { return nil })
					}
					if r != nil {
						usage = &r.Usage
					}
				}
				var rejected *UpstreamFailoverError
				require.ErrorAs(t, err, &rejected)
				require.True(t, rejected.IsOpenAIRequestRejection())
				require.False(t, rejected.ShouldRetryNextAccount())
				require.False(t, rejected.ShouldReportAccountScheduleFailure())
				require.True(t, dynamicQuotaKnownRejection(err))
				require.Nil(t, GetOpsCyberPolicy(c))
				if tc.tokens > 0 {
					require.NotNil(t, usage)
					require.Equal(t, tc.tokens, usage.InputTokens)
					require.Equal(t, 2, usage.OutputTokens)
				} else {
					require.False(t, openAIUsageHasTokens(usage))
				}
			})
		}
	}
}

func TestOpenAIRequestRejectionForwardRetainsUsage(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%v/stream=%v", passthrough, stream), func(t *testing.T) {
				c, _ := newNonStreamingFailoverContext(t)
				upstream := &httpUpstreamRecorder{resp: newNonStreamingSSEResponse()}
				upstream.resp.Body = io.NopCloser(strings.NewReader(rejectionSSE(rejectedPromptEvent, rejectedPromptMetered)))
				svc := newNonStreamingFailoverService()
				svc.httpUpstream = upstream
				account := newNonStreamingFailoverAccount()
				account.Credentials["api_key"] = "synthetic"
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.4","stream":%v,"instructions":"synthetic","input":"synthetic"}`, stream))
				var r *OpenAIForwardResult
				var err error
				if passthrough {
					r, err = svc.forwardOpenAIPassthrough(c.Request.Context(), c, account, body, body, "gpt-5.4", false, nil, stream, time.Now())
				} else {
					r, err = svc.Forward(c.Request.Context(), c, account, body)
				}
				var rejected *UpstreamFailoverError
				require.ErrorAs(t, err, &rejected)
				require.True(t, rejected.IsOpenAIRequestRejection())
				require.True(t, r.HasBillableUsage())
				require.Equal(t, OpenAIUsage{InputTokens: 11, OutputTokens: 2}, r.Usage)
				require.Len(t, upstream.requests, 1)

				usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
				billing := &openAIRecordUsageBillingRepoStub{}
				billSvc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billing,
					&openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
				r.DynamicQuotaReservationID = "00000000-0000-4000-8000-000000000001"
				require.NoError(t, billSvc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
					Result: r, User: &User{ID: 1}, Account: account,
					APIKey: &APIKey{ID: 2, GroupID: i64p(3), Group: &Group{
						ID: 3, SubscriptionType: SubscriptionTypeSubscription, RateMultiplier: 1,
					}},
					Subscription: &UserSubscription{ID: 4},
				}))
				require.Equal(t, 1, billing.calls)
				require.Equal(t, r.DynamicQuotaReservationID, billing.lastCmd.DynamicQuotaReservationID)
				require.Greater(t, billing.lastCmd.SubscriptionCost, 0.0)
				expected := expectedOpenAICost(t, billSvc, "gpt-5.4", r.Usage, 1)
				require.Equal(t, QuantizeUsageBillingAmount(expected.ActualCost), billing.lastCmd.SubscriptionCost)
				require.Zero(t, usageRepo.calls, "canonical billing owns the log and debit transaction")
			})
		}
	}
}

func TestOpenAIRequestRejectionBufferedOutputIsNotZeroUsage(t *testing.T) {
	body := rejectionSSE(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"synthetic output"}`, rejectedPromptFailed)
	require.True(t, openAIRejectedSSEHasOutput(body))
	for _, passthrough := range []bool{false, true} {
		c, _ := newNonStreamingFailoverContext(t)
		s := newNonStreamingFailoverService()
		var err error
		if passthrough {
			_, err = s.handlePassthroughSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), []byte(body), "model", "model")
		} else {
			_, err = s.handleSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), []byte(body), "model", "model")
		}
		var rejected *UpstreamFailoverError
		require.ErrorAs(t, err, &rejected)
		require.True(t, rejected.IsOpenAIRequestRejection())
		require.False(t, dynamicQuotaKnownRejection(err), "pre-header does not mean pre-generation")
	}
}

type rejectionTraceUpstream struct {
	HTTPUpstream
	do func(*http.Request) (*http.Response, error)
}

func (u rejectionTraceUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.do(r)
}

func TestDynamicQuotaRequestRejectionAndDispatchEvidence(t *testing.T) {
	s, db := dynamicTestStore(t)
	for _, tc := range []struct {
		name                                           string
		getConn, gotConn, priorDispatch, output, meter bool
		want                                           string
	}{
		{"dns_tcp_tls_unsent", true, false, false, false, false, "rejected"},
		{"no_trace_is_uncertain", false, false, false, false, false, "uncertain"},
		{"connected_is_uncertain", true, true, false, false, false, "uncertain"},
		{"prior_ws_dispatch_is_uncertain", true, false, true, false, false, "uncertain"},
		{"output_is_uncertain", true, false, false, true, false, "uncertain"},
		{"real_metering_wins", true, false, false, false, true, "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.Begin(context.Background(), 101, 4)
			require.NoError(t, err)
			if tc.priorDispatch {
				require.NoError(t, r.MarkDispatched())
			}
			previousTrace := false
			ctx := context.WithValue(context.Background(), dynamicQuotaRequestContextKey{}, r)
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GetConn: func(string) { previousTrace = true }})
			req := httptest.NewRequest(http.MethodPost, "https://synthetic.example.invalid", nil).WithContext(ctx)
			svc := &OpenAIGatewayService{httpUpstream: rejectionTraceUpstream{do: func(req *http.Request) (*http.Response, error) {
				trace := httptrace.ContextClientTrace(req.Context())
				if tc.getConn {
					trace.GetConn("synthetic.example.invalid:443")
				}
				if tc.gotConn {
					trace.GotConn(httptrace.GotConnInfo{})
					trace.GetConn("retry.example.invalid:443")
				}
				return nil, errors.New("synthetic transport failure")
			}}}
			_, err = svc.doOpenAIUpstream(req, "", &Account{ID: 4})
			require.Error(t, err)
			require.Equal(t, tc.getConn, previousTrace, "existing trace must be composed, not replaced")
			var result *OpenAIForwardResult
			if tc.meter {
				result = &OpenAIForwardResult{Usage: OpenAIUsage{InputTokens: 7}}
			}
			r.Finish(result, err, tc.output)
			var status string
			require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&status))
			require.Equal(t, tc.want, status)
		})
	}
	for _, tc := range []struct {
		name, payload string
		output, meter bool
		want          string
	}{
		{"rejected", rejectedPromptEvent, false, false, "rejected"},
		{"keepalive_only", rejectedPromptEvent, true, false, "rejected"},
		{"metered_rejection", rejectedPromptMetered, false, true, "pending"},
		{"failed_with_unmetered_output", `{"response":{"output":[{"type":"message"}],"error":{"code":"invalid_prompt"}}}`, false, false, "uncertain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.Begin(context.Background(), 101, 4)
			require.NoError(t, err)
			require.NoError(t, r.MarkDispatched())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			rejected := (&OpenAIGatewayService{}).newOpenAIRequestRejection(c, &Account{ID: 4}, []byte(tc.payload), "synthetic")
			var result *OpenAIForwardResult
			if tc.meter {
				result = &OpenAIForwardResult{Usage: OpenAIUsage{InputTokens: 11}}
			}
			r.Finish(result, rejected, tc.output)
			var status string
			require.NoError(t, db.QueryRow(`SELECT status FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&status))
			require.Equal(t, tc.want, status)
			var charged float64
			require.NoError(t, db.QueryRow(`SELECT weekly_usage_usd FROM user_subscriptions WHERE id=11`).Scan(&charged))
			require.Equal(t, 20.0, charged, "Finish must not invent a charge from the hold")
		})
	}
}

func TestDynamicQuotaRealTLSFailureIsNotDispatched(t *testing.T) {
	s, db := dynamicTestStore(t)
	upstreamCalls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamCalls++ }))
	defer server.Close()
	r, err := s.Begin(context.Background(), 101, 4)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), dynamicQuotaRequestContextKey{}, r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("synthetic"))
	require.NoError(t, err)
	client := &http.Client{Transport: &http.Transport{}, Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	svc := &OpenAIGatewayService{httpUpstream: rejectionTraceUpstream{do: client.Do}}
	_, err = svc.doOpenAIUpstream(req, "", &Account{ID: 4})
	require.Error(t, err)
	r.Finish(nil, err, false)
	var status, outcome string
	require.NoError(t, db.QueryRow(`SELECT status,outcome FROM dynamic_quota_requests WHERE id=$1`, r.ID).Scan(&status, &outcome))
	require.Equal(t, "rejected", status)
	require.Equal(t, "not_forwarded", outcome)
	require.Zero(t, upstreamCalls)
}
