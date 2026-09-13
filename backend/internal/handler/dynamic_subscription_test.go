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
	percent := 98.0
	if allow {
		percent = 50
	}
	p := service.DynamicQuotaPoolState{Cycle: 1, Status: "learning", StartedAt: now, Snapshot: &service.DynamicQuotaObservation{Identity: hex.EncodeToString(hash[:12]), UsedPercent: percent, ResetAt: now.Add(24 * time.Hour), WindowSeconds: 604800, FetchedAt: now}}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO dynamic_quota_pools").WithArgs(int64(9951), int64(1851)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT state FROM dynamic_quota_pools").WithArgs(int64(9951)).WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(raw))
	mock.ExpectQuery("SELECT us.id,us.status").WithArgs(int64(1851)).WillReturnRows(sqlmock.NewRows([]string{"id", "active", "group_id", "debug"}).AddRow(11, true, 4301, false))
	mock.ExpectQuery("SELECT account_id,enabled,weight.*FROM dynamic_group_policies").WithArgs(int64(4301)).WillReturnRows(sqlmock.NewRows([]string{"account_id", "enabled", "weight", "cap", "floor", "revision"}))
	mock.ExpectQuery("SELECT account_id FROM dynamic_subscription_policies").WithArgs(int64(11)).WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(9951))
	mock.ExpectQuery("SELECT EXISTS").WithArgs(int64(9951)).WillReturnRows(sqlmock.NewRows([]string{"protected"}).AddRow(true))
	mock.ExpectQuery("SELECT COALESCE\\(a.extra").WithArgs(int64(9951)).WillReturnRows(sqlmock.NewRows([]string{"extra", "settings"}).AddRow(`{"auto_pause_7d_threshold":0.98}`, `{}`))
	if !allow {
		mock.ExpectRollback()
		return
	}
	mock.ExpectQuery("SELECT COALESCE\\(credentials").WithArgs(int64(9951), int64(1851)).WillReturnRows(sqlmock.NewRows([]string{"identity", "bound"}).AddRow("9951", true))
	mock.ExpectQuery("SELECT p.standard_total_usd").WithArgs(int64(9951)).WillReturnRows(sqlmock.NewRows([]string{"total", "held", "max", "pending"}).AddRow(0, 0, 0, 0))
	mock.ExpectQuery("SELECT p.enabled,p.revision").WithArgs(int64(11)).WillReturnRows(sqlmock.NewRows([]string{"enabled", "revision", "account_id", "weight", "max_limit", "used_std", "allocated_std", "used", "user_id", "group_id", "rate", "peak_enabled", "peak_start", "peak_end", "peak_rate", "state", "updated", "start", "held", "applied", "floor", "activation_pending", "last_change", "fixed_slots", "source_fixed_slots"}).AddRow(true, 1, 9951, 1, 100, 0, 100, 0, 1751, 4301, 1, false, "", "", 1, raw, now, now, 0, 100, 10, false, nil, 0, 0))
	mock.ExpectQuery("SELECT COALESCE\\(a.extra").WithArgs(int64(9951)).WillReturnRows(sqlmock.NewRows([]string{"extra", "settings"}).AddRow(`{"auto_pause_7d_threshold":0.98}`, `{}`))
	mock.ExpectExec("INSERT INTO dynamic_quota_requests").WithArgs(sqlmock.AnyArg(), int64(9951), int64(1), int64(11), int64(1851), 0.01, int64(11), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("UPDATE dynamic_quota_requests SET dispatched_at").WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE dynamic_quota_requests SET status=\\$2").WithArgs(sqlmock.AnyArg(), "pending", "usage_received", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
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

// Real WS ingress in all three modes must finish its synchronous usage write
// before process cleanup, even though net/http no longer owns the connection.
func TestDynamicQuotaWSShutdownWaitsForUsagePersistence(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool, service.OpenAIWSIngressModeHTTPBridge} {
		t.Run(mode, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				event := []byte(`{"type":"response.completed","response":{"id":"resp_shutdown","model":"gpt-5.6-sol","usage":{"input_tokens":2,"output_tokens":1}}}`)
				if mode == service.OpenAIWSIngressModeHTTPBridge {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write(append(append([]byte("data: "), event...), []byte("\n\n")...))
					return
				}
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				if _, _, err = conn.Read(ctx); err != nil {
					return
				}
				_ = conn.Write(ctx, coderws.MessageText, event)
				_, _, _ = conn.Read(ctx)
			}))
			defer upstream.Close()
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			expectDynamicWSAdmission(t, mock, true)
			harness := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, false, func(h *OpenAIGatewayHandler, account *service.Account) {
				h.gatewayService.DynamicQuotas = service.NewDynamicSubscriptionService(db, nil, nil, nil)
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			})
			started, persist := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-persist:
				default:
					close(persist)
				}
			}()
			harness.usageRepo.beforeCreate = func(ctx context.Context) {
				close(started)
				<-persist
				require.NoError(t, ctx.Err(), "shutdown must not cancel billing")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Keep consuming so WebSocket close handshakes are not held by the test.
			go func() {
				for {
					if _, _, err := harness.clientConn.Read(ctx); err != nil {
						return
					}
				}
			}()
			require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"synthetic"}`)))
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("usage persistence never started")
			}
			harness.drain.StopAccepting()
			harness.drain.Cancel()
			short, release := context.WithTimeout(ctx, 10*time.Millisecond)
			require.ErrorIs(t, harness.drain.Wait(short), context.DeadlineExceeded, "cleanup must not pass an unfinished usage write")
			release()
			close(persist)
			select {
			case usage := <-harness.usageRepo.created:
				require.Equal(t, 2, usage.InputTokens)
				require.Equal(t, 1, usage.OutputTokens)
			case <-ctx.Done():
				t.Fatal("usage was lost during shutdown")
			}
			require.NoError(t, harness.drain.Wait(ctx))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDynamicQuotaPublicProjectionAndOps(t *testing.T) {
	sub := &service.UserSubscription{ID: 11, AdminDebug: true, Notes: "private-admin-notes", DynamicQuota: &service.DynamicSubscriptionQuota{Enabled: true, AccountID: 4, CapacityEstimateUSD: 12345, LimitUSD: 200, UsedUSD: 20, RemainingUSD: 180,
		GrowthFrozen: true, AllocationBudgetConflict: true}}
	public, err := json.Marshal(dto.UserSubscriptionFromService(sub))
	require.NoError(t, err)
	require.Contains(t, string(public), `"admin_debug":true`)
	require.NotContains(t, string(public), "private-admin-notes")
	require.NotContains(t, string(public), "account_id")
	require.NotContains(t, string(public), "capacity_estimate_usd")
	require.NotContains(t, string(public), "sample_count")
	require.NotContains(t, string(public), "learning_check")
	require.NotContains(t, string(public), "pool_settings")
	require.NotContains(t, string(public), "capacity_review")
	require.NotContains(t, string(public), "capacity_approval_ready")
	require.NotContains(t, string(public), "private-review-id")
	require.True(t, dto.UserSubscriptionFromService(sub).DynamicQuota.GrowthFrozen)
	require.NotContains(t, string(public), "allocation_budget_conflict")
	require.True(t, dto.UserSubscriptionFromServiceAdmin(sub).DynamicQuota.AllocationBudgetConflict)
	require.Equal(t, int64(4), dto.UserSubscriptionFromServiceAdmin(sub).DynamicQuota.AccountID)
	groupLimit := 10.0
	group := &service.Group{DailyLimitUSD: &groupLimit, WeeklyLimitUSD: &groupLimit, MonthlyLimitUSD: &groupLimit}
	sub.DailyUsageUSD, sub.WeeklyUsageUSD, sub.MonthlyUsageUSD = 100, 100, 100
	h := &GatewayHandler{}
	require.Equal(t, 180.0, h.calculateSubscriptionRemaining(group, sub), "native limits cannot hide dynamic headroom")
	sub.DynamicQuota.RemainingUSD = 0
	require.Zero(t, h.calculateSubscriptionRemaining(group, sub), "dynamic exhaustion still applies")
	sub.DynamicQuota.Enabled = false
	sub.DynamicQuota.RemainingUSD = 180
	require.Zero(t, h.calculateSubscriptionRemaining(group, sub), "disabled policies use native limits")
	sub.DynamicQuota.RequestedEnabled, sub.DynamicQuota.ActivationPending = true, true
	require.Zero(t, h.calculateSubscriptionRemaining(group, sub), "pending activation still uses native limits")
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
