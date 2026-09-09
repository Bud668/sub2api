package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func assertUserModelQuotaOpsEntry(t *testing.T, requestType int16) {
	t.Helper()
	require.Len(t, opsErrorLogQueue, 1, "one local rejection must produce exactly one record")
	entry := (<-opsErrorLogQueue).entry
	require.Equal(t, int64(7), *entry.UserID)
	require.Equal(t, int64(9), *entry.APIKeyID)
	require.Equal(t, int64(3), *entry.GroupID)
	require.Equal(t, "gpt-5.6-luna", entry.Model)
	require.Equal(t, requestType, *entry.RequestType)
	require.Equal(t, requestType != 1, entry.Stream)
	require.Equal(t, "model_request_quota_exceeded", entry.ErrorType)
	require.Equal(t, "request", entry.ErrorPhase)
	require.Equal(t, "client", entry.ErrorOwner)
	require.Equal(t, "gateway", entry.ErrorSource)
	require.Equal(t, "P3", entry.Severity)
	require.Equal(t, 429, entry.StatusCode)
	require.True(t, entry.IsBusinessLimited, "local limits must not count towards provider/SLA failures")
	require.Nil(t, entry.AccountID)
	require.Nil(t, entry.UpstreamStatusCode)
	require.Empty(t, entry.UpstreamEndpoint)
	require.Empty(t, entry.UpstreamErrors)
	require.Empty(t, entry.APIKeyPrefix)
	require.Nil(t, entry.ClientIP)
	require.Empty(t, entry.UserAgent)
	require.NotContains(t, entry.ErrorBody, "PRIVATE")
	require.Equal(t, "MODEL_REQUEST_QUOTA_EXCEEDED", gjson.Get(entry.ErrorBody, "error.code").String())
	require.NotEmpty(t, gjson.Get(entry.ErrorBody, "error.resets_at").String())
	require.Positive(t, gjson.Get(entry.ErrorBody, "error.retry_after").Int())
}

func userModelQuotaOpsKey(c *gin.Context) {
	groupID := int64(3)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID: 9, UserID: 7, Key: "PRIVATE-KEY", GroupID: &groupID,
		User: &service.User{ID: 7}, Group: &service.Group{ID: 3, Platform: service.PlatformOpenAI},
	})
	c.Next()
}

func TestUserModelQuotaOpsHTTPAdmissionRecordsExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		path, payload string
		requestType   int16
	}{
		{"/v1/responses", `{"model":"gpt-5.6-luna","input":"PRIVATE-PROMPT"}`, 1},
		{"/v1/chat/completions", `{"model":"gpt-5.6-luna","stream":true,"messages":[{"role":"user","content":"PRIVATE-PROMPT"}]}`, 2},
		{"/v1/messages", `{"model":"gpt-5.6-luna","stream":true,"messages":[{"role":"user","content":"PRIVATE-PROMPT"}]}`, 2},
	} {
		t.Run(tc.path, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 4)
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			expectUserModelWSLoad(mock, 7, "gpt-5.6-luna") // middleware projection
			expectUserModelWSLoad(mock, 7, "gpt-5.6-luna") // Reserve
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT rules").WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"rules"}).AddRow(`[{"model":"gpt-5.6-luna","mode":"limited","request_limit":1,"window_mode":"daily"}]`))
			mock.ExpectQuery("INSERT INTO user_model_request_windows").WithArgs(int64(7), "gpt-5.6-luna", int64(1), 0, true).WillReturnRows(sqlmock.NewRows([]string{"generation"}))
			mock.ExpectQuery("SELECT resets_at").WillReturnRows(sqlmock.NewRows([]string{"resets_at", "seconds"}).AddRow(time.Now().Add(time.Hour), 3600))
			mock.ExpectRollback()
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			r := gin.New()
			r.Use(OpsErrorLoggerMiddleware(ops), userModelQuotaOpsKey, middleware.GroupModelAllowlist(&service.APIKeyService{ModelPolicies: service.NewUserModelPolicyService(db)}))
			r.POST(tc.path, func(c *gin.Context) { t.Error("quota denial reached dispatch") })
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.payload))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			require.Equal(t, 429, w.Code)
			assertUserModelQuotaOpsEntry(t, tc.requestType)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUserModelQuotaOpsWSRecordsAfterUpgradeWithoutUpstreamAttribution(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 4)
	done := make(chan struct{})
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Next(); close(done) }, OpsErrorLoggerMiddleware(ops), userModelQuotaOpsKey)
	r.GET("/v1/responses", func(c *gin.Context) {
		service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
		// A previous turn may have had an account/error; it is not this denial's cause.
		setOpsSelectedAccount(c, 88)
		service.SetOpsUpstreamError(c, 503, "PRIVATE-UPSTREAM", "PRIVATE-DETAIL")
		conn, err := coderws.Accept(c.Writer, c.Request, nil)
		require.NoError(t, err)
		defer conn.CloseNow()
		_ = writeUserModelPolicyWSError(c, c.Request.Context(), conn, &service.ModelRequestQuotaError{
			Model: "gpt-5.6-luna", RetryAfter: 3600, ResetsAt: time.Now().Add(time.Hour),
		})
		require.Nil(t, service.GetOpsCyberPolicy(c))
	})
	server := httptest.NewServer(r)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	_, event, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(429), gjson.GetBytes(event, "status").Int())
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("WS handler did not finish")
	}
	assertUserModelQuotaOpsEntry(t, 3)
}
