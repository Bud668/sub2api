package service

import (
	"context"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIRequestRejectionWSAccounting(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		for _, metered := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/bare_eof", true: "/bare_then_metered"}[metered], func(t *testing.T) {
				cfg := passthroughLifecycleConfig()
				adapter, upstream, upstreamCtx := newTestOpenAIWSClientPair(t)
				dialer := &openAIWSSingleConnDialer{conn: adapter}
				pool := newOpenAIWSConnPool(cfg)
				pool.setClientDialerForTest(dialer)
				defer pool.Close()
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
					openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSPool: pool, openaiWSPassthroughDialer: dialer}
				account := passthroughLifecycleAccount()
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				type observed struct {
					result *OpenAIForwardResult
					err    error
				}
				results := make(chan observed, 4)
				server, done := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(*gin.Context) *OpenAIWSIngressHooks {
					return &OpenAIWSIngressHooks{AfterTurn: func(_ int, r *OpenAIForwardResult, e error) { results <- observed{r, e} }}
				})
				defer server.Close()
				client := dialPassthroughLifecycleClient(t, server)
				defer client.CloseNow()
				go func() {
					for {
						if _, _, err := client.Read(ctx); err != nil {
							return
						}
					}
				}()
				_, _, err := upstream.Read(upstreamCtx)
				require.NoError(t, err)
				require.NoError(t, upstream.Write(upstreamCtx, coderws.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_rejected","model":"gpt-5.1","output":[]}}`)))
				require.NoError(t, upstream.Write(upstreamCtx, coderws.MessageText, []byte(rejectedPromptEvent)))
				if metered {
					require.NoError(t, upstream.Write(upstreamCtx, coderws.MessageText, []byte(rejectedPromptMetered)))
				}
				require.NoError(t, upstream.CloseNow())
				select {
				case got := <-results:
					var rejected *UpstreamFailoverError
					require.ErrorAs(t, got.err, &rejected)
					require.True(t, rejected.IsOpenAIRequestRejection())
					require.True(t, dynamicQuotaKnownRejection(got.err))
					if metered {
						require.NotNil(t, got.result)
						require.Equal(t, OpenAIUsage{InputTokens: 11, OutputTokens: 2}, got.result.Usage)
					} else {
						require.False(t, got.result.HasBillableUsage())
					}
				case <-ctx.Done():
					t.Fatal("rejection did not reach accounting")
				}
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("gateway did not finish")
				}
				require.Empty(t, results, "one turn must not create duplicate bills")
			})
		}
	}
}
