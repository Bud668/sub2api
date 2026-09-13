package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaReviewRejectsInvalidInputBeforeAccessingStore(t *testing.T) {
	h := NewSubscriptionHandler(&service.SubscriptionService{DynamicQuotas: service.NewDynamicSubscriptionService(nil, nil, nil, nil)})
	router := gin.New()
	router.POST("/:request_id", func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
		h.ResolveDynamicAccounting(c)
	})
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/0", `{}`, 409},
		{"/abc", `{}`, 409},
		{"/11", `{"action":"charge","amount":9000}`, 400},
		{"/11", `{"action":10}`, 400},
		{"/11", `{} {}`, 400},
		{"/11", `{"action":"` + strings.Repeat("a", 1024) + `"}`, 400},
		{"/11", `{}`, 409},
		{"/11", `{"action":"charge"}`, 409},
	} {
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		require.Equal(t, tc.status, r.Code, tc.body)
		require.Equal(t, "no-store", r.Header().Get("Cache-Control"))
	}
}

func TestDynamicQuotaAbsorptionRejectsInvalidQueries(t *testing.T) {
	h := NewSubscriptionHandler(nil)
	router := gin.New()
	router.GET("/", h.GetAbsorbedUsage)
	for _, q := range []string{"user_id=-1", "group_id=abc", "page=0", "page_size=101", "scope=all", "category=all", "visibility=invalid", "summary_only=invalid", "page=99999999999999999999999"} {
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/?"+q, nil))
		require.Equal(t, 400, r.Code, q)
		require.Equal(t, "no-store", r.Header().Get("Cache-Control"))
	}
}

func TestDynamicQuotaAbsorptionClearRejectsUntrustedInput(t *testing.T) {
	h := NewSubscriptionHandler(nil)
	for _, tc := range []struct {
		actor int64
		body  string
		code  int
	}{
		{0, `{"ids":["a"],"actor_id":1}`, 401},
		{1, `{}`, 400}, {1, `{"ids":[]}`, 400},
		{1, `{"ids":["a"],"all":true}`, 400},
		{1, `{"ids":["a"]} {}`, 400},
		{1, `{"ids":["a"]}`, 503},
	} {
		router := gin.New()
		router.POST("/", func(c *gin.Context) {
			if tc.actor > 0 {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tc.actor})
			}
			h.ClearAbsorbedUsage(c)
		})
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
		require.Equal(t, tc.code, r.Code)
		require.Equal(t, "no-store", r.Header().Get("Cache-Control"))
	}
}

func TestAdminDebugConversionRequiresTrustedActorAndValidID(t *testing.T) {
	h := NewSubscriptionHandler(nil)
	for _, tc := range []struct {
		actor  int64
		id     string
		status int
	}{{0, "11", 401}, {1, "-1", 400}, {1, "abc", 400}, {1, "9223372036854775808", 400}, {1, "11", 503}} {
		router := gin.New()
		router.POST("/:id", func(c *gin.Context) {
			if tc.actor > 0 {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tc.actor})
			}
			h.ConvertToAdminDebug(c)
		})
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/"+tc.id, strings.NewReader(`{"actor_id":1,"role":"admin"}`)))
		require.Equal(t, tc.status, r.Code)
		require.Equal(t, "no-store", r.Header().Get("Cache-Control"))
	}
}

func TestAdminDebugQuotaRejectsUntrustedAndIncompleteEdits(t *testing.T) {
	h := NewSubscriptionHandler(nil)
	for _, tc := range []struct {
		actor int64
		body  string
		code  int
	}{
		{0, `{"revision":1,"weekly_limit_usd":0,"actor_id":1}`, 401},
		{1, `{}`, 400}, {1, `{"revision":1}`, 400}, {1, `{"revision":1,"weekly_limit_usd":null}`, 400},
		{1, `{"revision":1,"weekly_limit_usd":1,"reset_usage":true}`, 400},
		{1, `{"revision":1,"weekly_limit_usd":1} {}`, 400},
		{1, `{"revision":1,"weekly_limit_usd":0}`, 503},
	} {
		router := gin.New()
		router.PUT("/:id", func(c *gin.Context) {
			if tc.actor > 0 {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tc.actor})
			}
			h.SaveAdminDebugQuota(c)
		})
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(http.MethodPut, "/11", strings.NewReader(tc.body)))
		require.Equal(t, tc.code, r.Code, tc.body)
		require.Equal(t, "no-store", r.Header().Get("Cache-Control"))
	}
}

func TestDynamicQuotaManualResetRejectsUntrustedParameters(t *testing.T) {
	h := NewSubscriptionHandler(&service.SubscriptionService{DynamicQuotas: service.NewDynamicSubscriptionService(nil, nil, nil, nil)})
	router := gin.New()
	router.POST("/:id", func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
		h.DynamicReset(c)
	})
	for _, body := range []string{`{}`, `{"account_id":4,"cycle":0}`, `{"account_id":4,"cycle":1,"force":true}`, `{"account_id":4,"cycle":1,"reset_at":"2027-01-01"}`, `{"account_id":4,"cycle":1} {}`} {
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/11", strings.NewReader(body)))
		require.Equal(t, 400, r.Code, body)
		require.Equal(t, "no-store", r.Header().Get("Cache-Control"))
	}
	unauthenticated := gin.New()
	unauthenticated.POST("/:id", h.DynamicReset)
	r := httptest.NewRecorder()
	unauthenticated.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/11", strings.NewReader(`{"account_id":4,"cycle":1,"actor_id":1}`)))
	require.Equal(t, 401, r.Code)
}
