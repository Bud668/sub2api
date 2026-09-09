package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestUserModelPolicySelfUsesAuthenticatedUserOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewAPIKeyHandler(&service.APIKeyService{ModelPolicies: service.NewUserModelPolicyService(db)})
	mock.ExpectQuery("SELECT COALESCE").WithArgs(int64(9), "user_model_request_policy_enabled").WillReturnRows(sqlmock.NewRows([]string{"enabled", "revision", "rules"}).AddRow(false, 1, []byte(`[{"model":"private-draft","mode":"deny"}]`)))
	mock.ExpectQuery("SELECT model").WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"model", "used", "resets_at"}))
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9})
		c.Next()
	})
	r.GET("/user/model-policy", h.GetMyModelPolicy)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/user/model-policy?user_id=1", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-draft") || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("self read leaked: %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUserModelPolicyWSFirstFrameDenialIdentifiesUser(t *testing.T) {
	p := &service.UserModelRequestPolicy{Enabled: true, Rules: []service.UserModelRequestRule{{Model: "gpt-5.6-luna", Mode: "deny"}}}
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:            `{"type":"response.create","model":"gpt-5.6-luna","stream":false}`,
		group:                   &service.Group{ID: 4201, Platform: service.PlatformOpenAI, ModelAllowlist: p.Allowlist()},
		firstFrameCloseExpected: true,
		firstFrameCloseReason:   "not authorized for this user",
	})
}

func TestUserModelQuotaWSErrorIncludesRetryWithoutCyber(t *testing.T) {
	r := gin.New()
	r.GET("/ws", func(c *gin.Context) {
		conn, err := coderws.Accept(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = writeUserModelPolicyWSError(c, c.Request.Context(), conn, &service.ModelRequestQuotaError{Model: "gpt-5.6-sol", RetryAfter: 3600, ResetsAt: time.Now().Add(time.Hour)})
		if service.GetOpsCyberPolicy(c) != nil {
			t.Error("quota rejection became cyber violation")
		}
		_ = conn.Close(coderws.StatusPolicyViolation, "quota exhausted")
	})
	server := httptest.NewServer(r)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Status int `json:"status"`
		Error  struct {
			Code       string `json:"code"`
			RetryAfter int    `json:"retry_after"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Status != 429 || event.Error.Code != "MODEL_REQUEST_QUOTA_EXCEEDED" || event.Error.RetryAfter != 3600 {
		t.Fatalf("bad WS error %s", payload)
	}
}

func expectUserModelWSLoad(mock sqlmock.Sqlmock, userID int64, model string) {
	rules := fmt.Sprintf(`[{"model":%q,"mode":"limited","request_limit":1,"window_mode":"daily"}]`, model)
	mock.ExpectQuery("SELECT COALESCE").WithArgs(userID, "user_model_request_policy_enabled").
		WillReturnRows(sqlmock.NewRows([]string{"enabled", "revision", "rules"}).AddRow("true", 1, rules))
}

func expectUserModelWSReservation(mock sqlmock.Sqlmock, userID int64, model string, allow bool) {
	expectUserModelWSLoad(mock, userID, model) // latest policy check
	expectUserModelWSLoad(mock, userID, model) // reservation
	mock.ExpectBegin()
	rules := fmt.Sprintf(`[{"model":%q,"mode":"limited","request_limit":1,"window_mode":"daily"}]`, model)
	mock.ExpectQuery("SELECT rules FROM user_model_request_policies").WithArgs(userID).
		WillReturnRows(sqlmock.NewRows([]string{"rules"}).AddRow(rules))
	rows := sqlmock.NewRows([]string{"generation"})
	if allow {
		rows.AddRow(1)
	}
	mock.ExpectQuery("INSERT INTO user_model_request_windows").WithArgs(userID, model, int64(1), 0, true).WillReturnRows(rows)
	if allow {
		mock.ExpectCommit()
	} else {
		mock.ExpectQuery("SELECT resets_at").WithArgs(userID, model).
			WillReturnRows(sqlmock.NewRows([]string{"resets_at", "seconds"}).AddRow(time.Now().Add(time.Hour), 3600))
		mock.ExpectRollback()
	}
}

func TestUserModelQuotaWSFirstRequestAndReconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool, service.OpenAIWSIngressModeHTTPBridge} {
		t.Run(mode, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer upstream.Close()
			for attempt := 0; attempt < 2; attempt++ {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()
				expectUserModelWSReservation(mock, 1751, "gpt-5.6-luna", false)
				harness := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, false, func(h *OpenAIGatewayHandler, account *service.Account) {
					h.apiKeyService.ModelPolicies = service.NewUserModelPolicyService(db)
					account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.6-luna","input":"hello"}`)))
				_, event, err := harness.clientConn.Read(ctx)
				require.NoError(t, err)
				require.Equal(t, int64(429), gjson.GetBytes(event, "status").Int())
				require.Equal(t, "MODEL_REQUEST_QUOTA_EXCEEDED", gjson.GetBytes(event, "error.code").String())
				_, _, err = harness.clientConn.Read(ctx)
				require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err))
				select {
				case <-harness.handlerDone:
				case <-ctx.Done():
					t.Fatal("quota rejection did not close the handler")
				}
				require.Zero(t, upstreamCalls.Load(), "an exhausted quota must reject before any upstream connection")
				require.Empty(t, harness.moderationRepo.logSnapshot(), "quota exhaustion is not a cyber violation")
				require.False(t, harness.users.isDisabled())
				require.NoError(t, mock.ExpectationsWereMet())
			}
		})
	}
}

func TestUserModelQuotaWSFirstSuccessConsumesSlotBeforeNextTurn(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			var upstreamFrames atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				for {
					if _, _, err := conn.Read(ctx); err != nil {
						return
					}
					upstreamFrames.Add(1)
					if err := conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_quota_first","model":"gpt-5.6-luna","usage":{"input_tokens":2,"output_tokens":1}}}`)); err != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			expectUserModelWSReservation(mock, 1751, "gpt-5.6-luna", true)
			expectUserModelWSReservation(mock, 1751, "gpt-5.6-luna", false)
			harness := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, false, func(h *OpenAIGatewayHandler, account *service.Account) {
				h.apiKeyService.ModelPolicies = service.NewUserModelPolicyService(db)
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			payload := []byte(`{"type":"response.create","model":"gpt-5.6-luna","input":"hello"}`)
			require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, payload))
			_, event, err := harness.clientConn.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
			require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, payload))
			_, event, err = harness.clientConn.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "MODEL_REQUEST_QUOTA_EXCEEDED", gjson.GetBytes(event, "error.code").String())
			_, _, err = harness.clientConn.Read(ctx)
			require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err))
			select {
			case <-harness.handlerDone:
			case <-ctx.Done():
				t.Fatal("quota rejection did not close the handler")
			}
			require.Equal(t, int32(1), upstreamFrames.Load())
			require.NoError(t, mock.ExpectationsWereMet(), "the first request must reserve exactly once and must not refund success")
		})
	}
}
