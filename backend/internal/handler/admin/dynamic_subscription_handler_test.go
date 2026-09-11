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
	for _, q := range []string{"user_id=-1", "group_id=abc", "page=0", "page_size=101", "scope=all", "category=all", "summary_only=invalid", "page=99999999999999999999999"} {
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/?"+q, nil))
		require.Equal(t, 400, r.Code, q)
		require.Equal(t, "no-store", r.Header().Get("Cache-Control"))
	}
}
