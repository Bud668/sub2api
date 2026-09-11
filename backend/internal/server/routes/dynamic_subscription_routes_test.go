package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDynamicQuotaAccountingRequiresAdminRoute(t *testing.T) {
	router := gin.New()
	h := &handler.Handlers{Admin: &handler.AdminHandlers{Subscription: adminhandler.NewSubscriptionHandler(nil)}}
	auth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
		} else {
			c.AbortWithStatus(http.StatusForbidden)
		}
	})
	audit := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), h, auth, audit, stepUp, nil, nil)
	for _, token := range []string{"", "Bearer user-token"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/subscriptions/absorbed-usage/00000000-0000-0000-0000-000000000001/resolve", nil)
		req.Header.Set("Authorization", token)
		r := httptest.NewRecorder()
		router.ServeHTTP(r, req)
		if token == "" {
			require.Equal(t, http.StatusUnauthorized, r.Code)
		} else {
			require.Equal(t, http.StatusForbidden, r.Code)
		}
		req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/subscriptions/absorbed-usage?summary_only=true", nil)
		req.Header.Set("Authorization", token)
		r = httptest.NewRecorder()
		router.ServeHTTP(r, req)
		if token == "" {
			require.Equal(t, http.StatusUnauthorized, r.Code)
		} else {
			require.Equal(t, http.StatusForbidden, r.Code)
		}
	}
	r := httptest.NewRecorder()
	router.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/api/v1/subscriptions/absorbed-usage/00000000-0000-0000-0000-000000000001/resolve", nil))
	require.Equal(t, http.StatusNotFound, r.Code)
}
