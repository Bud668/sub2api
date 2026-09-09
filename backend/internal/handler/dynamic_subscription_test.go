package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func expectDynamicWSAdmission(t *testing.T, mock sqlmock.Sqlmock, allow bool) {
	t.Helper()
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte("9951"))
	percent := 97.0
	if allow {
		percent = 50
	}
	p := service.DynamicQuotaPoolState{Cycle: 1, Status: "learning", StartedAt: now, Snapshot: &service.DynamicQuotaObservation{Identity: hex.EncodeToString(hash[:12]), UsedPercent: percent, ResetAt: now.Add(24 * time.Hour), WindowSeconds: 604800, FetchedAt: now}}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO dynamic_quota_pools").WithArgs(int64(9951)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT state,config_revision,usage_ceiling_percent FROM dynamic_quota_pools").WithArgs(int64(9951)).WillReturnRows(sqlmock.NewRows([]string{"state", "config_revision", "usage_ceiling_percent"}).AddRow(raw, 0, 98))
	mock.ExpectQuery("SELECT us.id,us.status").WithArgs(int64(1851)).WillReturnRows(sqlmock.NewRows([]string{"id", "active"}).AddRow(11, true))
	mock.ExpectQuery("SELECT account_id FROM dynamic_subscription_policies").WithArgs(int64(11)).WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(9951))
	mock.ExpectQuery("SELECT EXISTS").WithArgs(int64(9951)).WillReturnRows(sqlmock.NewRows([]string{"protected"}).AddRow(true))
	if !allow {
		mock.ExpectRollback()
		return
	}
	mock.ExpectQuery("SELECT COALESCE\\(credentials").WithArgs(int64(9951), int64(1851)).WillReturnRows(sqlmock.NewRows([]string{"identity", "bound"}).AddRow("9951", true))
	mock.ExpectQuery("SELECT p.standard_total_usd").WithArgs(int64(9951)).WillReturnRows(sqlmock.NewRows([]string{"total", "held", "max", "pending"}).AddRow(0, 0, 0, 0))
	mock.ExpectQuery("SELECT p.enabled,p.revision").WithArgs(int64(11)).WillReturnRows(sqlmock.NewRows([]string{"enabled", "revision", "account_id", "weight", "max_limit", "used_std", "allocated_std", "used", "user_id", "group_id", "rate", "peak_enabled", "peak_start", "peak_end", "peak_rate", "state", "updated", "start", "held", "threshold", "applied", "config_revision", "ceiling"}).AddRow(true, 1, 9951, 1, 100, 0, 100, 0, 1751, 4301, 1, false, "", "", 1, raw, now, now, 0, 10, 100, 0, 98))
	mock.ExpectExec("INSERT INTO dynamic_quota_requests").WithArgs(sqlmock.AnyArg(), int64(9951), int64(1), int64(11), int64(1851), 0.01).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func TestDynamicQuotaWSFirstFrameReconnectAndTurns(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool, service.OpenAIWSIngressModeHTTPBridge} {
		for _, firstAllowed := range []bool{false, true} {
			label := "first_rejected"
			if firstAllowed {
				label = "second_rejected"
			}
			t.Run(mode+"/"+label, func(t *testing.T) {
				var frames atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
					defer cancel()
					event := []byte(`{"type":"response.completed","response":{"id":"resp_dynamic","model":"gpt-5.6-sol","usage":{"input_tokens":2,"output_tokens":1}}}`)
					if mode == service.OpenAIWSIngressModeHTTPBridge {
						frames.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write(append(append([]byte("data: "), event...), []byte("\n\n")...))
						return
					}
					conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
					if err != nil {
						return
					}
					defer conn.CloseNow()
					for {
						if _, _, err = conn.Read(ctx); err != nil {
							return
						}
						frames.Add(1)
						if err = conn.Write(ctx, coderws.MessageText, event); err != nil {
							return
						}
					}
				}))
				defer upstream.Close()
				attempts := 1
				if !firstAllowed {
					attempts = 2
				}
				for attempt := 0; attempt < attempts; attempt++ {
					db, mock, err := sqlmock.New()
					require.NoError(t, err)
					defer db.Close()
					if firstAllowed {
						expectDynamicWSAdmission(t, mock, true)
					}
					expectDynamicWSAdmission(t, mock, false)
					harness := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, false, func(h *OpenAIGatewayHandler, account *service.Account) {
						h.gatewayService.DynamicQuotas = service.NewDynamicSubscriptionService(db, nil, nil, nil)
						account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
					})
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					payload := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"synthetic"}`)
					require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, payload))
					_, event, err := harness.clientConn.Read(ctx)
					require.NoError(t, err)
					if firstAllowed {
						require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
						require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, payload))
						_, event, err = harness.clientConn.Read(ctx)
						require.NoError(t, err)
					}
					require.Equal(t, "DYNAMIC_QUOTA_EXHAUSTED", gjson.GetBytes(event, "error.code").String())
					require.Equal(t, int64(429), gjson.GetBytes(event, "status").Int())
					_, _, err = harness.clientConn.Read(ctx)
					require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err))
					select {
					case <-harness.handlerDone:
					case <-ctx.Done():
						t.Fatal("quota denial did not terminate WS")
					}
					require.False(t, harness.users.isDisabled())
					require.Empty(t, harness.moderationRepo.logSnapshot())
					require.NoError(t, mock.ExpectationsWereMet())
				}
				if firstAllowed {
					require.Equal(t, int32(1), frames.Load())
				} else {
					require.Zero(t, frames.Load())
				}
			})
		}
	}
}

func TestDynamicQuotaPublicProjectionAndOps(t *testing.T) {
	sub := &service.UserSubscription{ID: 11, DynamicQuota: &service.DynamicSubscriptionQuota{Enabled: true, AccountID: 4, CapacityEstimateUSD: 12345, SampleCount: 3, PoolSettings: &service.DynamicQuotaPoolSettings{UsageCeilingPercent: 98}, LimitUSD: 200, UsedUSD: 20, RemainingUSD: 180}}
	public, err := json.Marshal(dto.UserSubscriptionFromService(sub))
	require.NoError(t, err)
	require.NotContains(t, string(public), "account_id")
	require.NotContains(t, string(public), "capacity_estimate_usd")
	require.NotContains(t, string(public), "sample_count")
	require.NotContains(t, string(public), "pool_settings")
	require.Equal(t, int64(4), dto.UserSubscriptionFromServiceAdmin(sub).DynamicQuota.AccountID)
	for _, ws := range []bool{false, true} {
		setupOpsErrorLogTestQueue(t, 4)
		ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		router := gin.New()
		router.Use(OpsErrorLoggerMiddleware(ops), userModelQuotaOpsKey)
		router.POST("/v1/responses", func(c *gin.Context) {
			setOpsRequestContext(c, "gpt-5.6-sol", ws)
			setOpsSelectedAccount(c, 4)
			service.SetOpsUpstreamError(c, 503, "PRIVATE", "PRIVATE")
			if ws {
				service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
			}
			service.MarkDynamicQuotaRejected(c, service.ErrDynamicQuotaExhausted)
			if ws {
				c.Status(200)
			} else {
				c.Status(429)
			}
		})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("PRIVATE request text")))
		require.Len(t, opsErrorLogQueue, 1)
		entry := (<-opsErrorLogQueue).entry
		require.Equal(t, "dynamic_quota_rejected", entry.ErrorType)
		require.Equal(t, 429, entry.StatusCode)
		require.Equal(t, "P3", entry.Severity)
		require.True(t, entry.IsBusinessLimited)
		require.Nil(t, entry.AccountID)
		require.Empty(t, entry.UpstreamErrors)
		require.NotContains(t, entry.ErrorBody, "PRIVATE")
		require.Nil(t, entry.ClientIP)
	}
}
