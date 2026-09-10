package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaWSPartialUsageReachesAccounting(t *testing.T) {
	for _, tc := range []struct{ mode, interruption string }{
		{OpenAIWSIngressModeCtxPool, "disconnect"}, {OpenAIWSIngressModePassthrough, "disconnect"},
		{OpenAIWSIngressModeCtxPool, "cancel"}, {OpenAIWSIngressModePassthrough, "cancel"},
		{OpenAIWSIngressModeCtxPool, "metered_error"}, {OpenAIWSIngressModePassthrough, "metered_error"},
	} {
		t.Run(tc.mode+"/"+tc.interruption, func(t *testing.T) {
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			adapter, upstream, upstreamCtx := newTestOpenAIWSClientPair(t)
			dialer := &openAIWSSingleConnDialer{conn: adapter}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
				openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSPool: pool, openaiWSPassthroughDialer: dialer}
			account := passthroughLifecycleAccount()
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = tc.mode
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			forwardCtx, cancelForward := context.WithCancel(ctx)
			defer cancelForward()
			results := make(chan *OpenAIForwardResult, 4)
			server, done := startPassthroughLifecycleServerWithHooks(t, forwardCtx, svc, account, func(*gin.Context) *OpenAIWSIngressHooks {
				return &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, _ error) { results <- result }}
			})
			defer server.Close()
			client := dialPassthroughLifecycleClient(t, server)
			defer client.CloseNow()
			_, _, err := upstream.Read(upstreamCtx)
			require.NoError(t, err)
			require.NoError(t, upstream.Write(upstreamCtx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_paid_first","usage":{"input_tokens":100,"output_tokens":50}}}`)))
			_, _, err = client.Read(ctx)
			require.NoError(t, err)
			first := <-results
			require.Equal(t, 100, first.Usage.InputTokens)
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"synthetic"}`)))
			_, _, err = upstream.Read(upstreamCtx)
			require.NoError(t, err)
			partialEvent := []byte(`{"type":"response.in_progress","response":{"id":"resp_partial_second","usage":{"input_tokens":7,"output_tokens":2}}}`)
			if tc.interruption == "metered_error" {
				partialEvent = []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"synthetic limit"},"response":{"id":"resp_partial_second","usage":{"input_tokens":7,"output_tokens":2}}}`)
			}
			require.NoError(t, upstream.Write(upstreamCtx, coderws.MessageText, partialEvent))
			if tc.interruption != "metered_error" {
				_, _, err = client.Read(ctx)
				require.NoError(t, err)
			}
			if tc.interruption == "cancel" {
				cancelForward()
			} else {
				require.NoError(t, upstream.CloseNow())
			}
			// Keep reading to complete close handshakes while accounting finishes.
			go func() {
				for {
					if _, _, err := client.Read(ctx); err != nil {
						return
					}
				}
			}()
			select {
			case second := <-results:
				require.NotNil(t, second)
				require.Equal(t, "resp_partial_second", second.RequestID)
				require.Equal(t, OpenAIUsage{InputTokens: 7, OutputTokens: 2}, second.Usage,
					"the previous completed turn must never be charged to the failed turn")
			case <-ctx.Done():
				t.Fatal("partial usage was lost before accounting hook")
			}
			select {
			case err := <-done:
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "a metered turn must not be replayed by the handler")
			case <-ctx.Done():
				t.Fatal("gateway did not stop after upstream loss")
			}
			require.Empty(t, results, "do not bill a turn twice")
		})
	}
}

func TestDynamicQuotaHTTPBridgeRetainsPreOutputUsage(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader("data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_partial\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n"))}}
	svc := &OpenAIGatewayService{cfg: passthroughLifecycleConfig(), httpUpstream: upstream}
	account := passthroughLifecycleAccount()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	payload := []byte(`{"type":"response.create","model":"gpt-5.1","input":"synthetic"}`)
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "synthetic-token", payload, len(payload),
		"gpt-5.1", "", "", "", "", 1, func([]byte) error { return nil })
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, OpenAIUsage{InputTokens: 7, OutputTokens: 2}, result.Usage)
}
