package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaApprovalRejectsInvalidInputBeforeAccessingStore(t *testing.T) {
	h := NewSubscriptionHandler(&service.SubscriptionService{DynamicQuotas: service.NewDynamicSubscriptionService(nil, nil, nil, nil)})
	router := gin.New()
	router.POST("/:id", h.ApproveDynamicCapacity)
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/0", `{}`, 400},
		{"/abc", `{}`, 400},
		{"/11", `{"review_id":"test","capacity_usd":9000}`, 400},
		{"/11", `{"review_id":10}`, 400},
		{"/11", `{} {}`, 400},
		{"/11", `{"review_id":"` + strings.Repeat("a", 1024) + `"}`, 400},
		{"/11", `{}`, 409},
		{"/11", `{"review_id":"not-a-uuid"}`, 409},
	} {
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		require.Equal(t, tc.status, r.Code, tc.body)
		require.Equal(t, "no-store", r.Header().Get("Cache-Control"))
	}
}
