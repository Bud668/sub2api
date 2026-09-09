package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func modelPolicyMock(t *testing.T) (*service.APIKeyService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	return &service.APIKeyService{ModelPolicies: service.NewUserModelPolicyService(db)}, mock
}

func expectModelPolicy(mock sqlmock.Sqlmock, enabled bool, rules string) {
	mock.ExpectQuery("SELECT COALESCE").WillReturnRows(sqlmock.NewRows([]string{"enabled", "revision", "rules"}).AddRow(enabled, 1, []byte(rules)))
}

func TestUserModelPolicyProjectionIsUserOnlyAndDoesNotPoisonCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, mock := modelPolicyMock(t)
	shared := &service.Group{ID: 7, Platform: service.PlatformOpenAI, ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"legacy-only"}}}
	key := &service.APIKey{ID: 1, UserID: 1, Group: shared}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), key); c.Next() }, GroupModelAllowlist(s))
	r.GET("/v1/models", func(c *gin.Context) {
		k, _ := GetAPIKeyFromContext(c)
		c.JSON(200, k.Group.ModelAllowlist.FilterForListing([]string{"sol", "luna", "legacy-only"}))
	})
	expectModelPolicy(mock, true, `[{"model":"luna","mode":"deny"}]`)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	if w.Code != 200 || w.Body.String() != `["sol","legacy-only"]` {
		t.Fatalf("user selection: %d %s", w.Code, w.Body.String())
	}
	if shared.ModelAllowlist.UserPolicy != nil || shared.ModelAllowlist.Models[0] != "legacy-only" || key.Group != shared {
		t.Fatal("shared auth snapshot mutated")
	}
	key = &service.APIKey{ID: 2, UserID: 2, Group: shared}
	expectModelPolicy(mock, true, `[{"model":"sol","mode":"deny"}]`)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	if w.Body.String() != `["luna","legacy-only"]` {
		t.Fatalf("cross-user model leak: %s", w.Body.String())
	}
}

func TestUserModelRequestQuotaHTTP429BeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, mock := modelPolicyMock(t)
	rules := `[{"model":"gpt-5.6-sol","mode":"limited","request_limit":3,"window_hours":6}]`
	expectModelPolicy(mock, true, rules)
	expectModelPolicy(mock, true, rules)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT rules").WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"rules"}).AddRow([]byte(rules)))
	mock.ExpectQuery("INSERT INTO user_model_request_windows").WithArgs(int64(1), "gpt-5.6-sol", int64(3), 21600, false).WillReturnRows(sqlmock.NewRows([]string{"generation"}))
	mock.ExpectQuery("SELECT resets_at").WillReturnRows(sqlmock.NewRows([]string{"resets_at", "remaining"}).AddRow(time.Now().Add(124*time.Second), 123.4))
	mock.ExpectRollback()
	r := gin.New()
	forwarded := false
	r.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 1, Group: &service.Group{ID: 7, Platform: service.PlatformOpenAI}})
		c.Next()
	}, GroupModelAllowlist(s))
	r.POST("/v1/responses", func(c *gin.Context) { forwarded = true; c.Status(200) })
	w := doJSON(t, r, "POST", "/v1/responses", `{"model":"gpt-5.6"}`)
	if forwarded || w.Code != 429 || w.Header().Get("Retry-After") != "124" || !strings.Contains(w.Body.String(), "MODEL_REQUEST_QUOTA_EXCEEDED") {
		t.Fatalf("bad rejection: forwarded=%v status=%d body=%s", forwarded, w.Code, w.Body.String())
	}
}

func TestUserModelPolicyUnconfiguredHTTPModelIsAllowed(t *testing.T) {
	s, mock := modelPolicyMock(t)
	for _, rules := range []string{`[]`, `[{"model":"gpt-5.6-luna","mode":"deny"}]`} {
		expectModelPolicy(mock, true, rules)
		expectModelPolicy(mock, true, rules)
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT rules").WillReturnRows(sqlmock.NewRows([]string{"rules"}).AddRow([]byte(rules)))
		mock.ExpectRollback()
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set(string(ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 1, Group: &service.Group{ID: 7, Platform: service.PlatformOpenAI}})
			c.Next()
		}, GroupModelAllowlist(s))
		r.POST("/v1/responses", func(c *gin.Context) { c.Status(200) })
		w := doJSON(t, r, "POST", "/v1/responses", `{"model":"gpt-5.6-sol"}`)
		if w.Code != 200 {
			t.Fatalf("unconfigured HTTP model rejected: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestUserModelPolicyRejectsUngroupedKeyAndDatabaseFailure(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "ungrouped", true: "database"}[unavailable], func(t *testing.T) {
			s, mock := modelPolicyMock(t)
			if unavailable {
				mock.ExpectQuery("SELECT COALESCE").WillReturnError(http.ErrServerClosed)
			} else {
				expectModelPolicy(mock, true, `[{"model":"sol","mode":"unlimited"}]`)
			}
			r := gin.New()
			r.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{UserID: 1}); c.Next() }, GroupModelAllowlist(s))
			r.POST("/v1/responses", func(c *gin.Context) { t.Error("authorization bypassed"); c.Status(200) })
			w := doJSON(t, r, "POST", "/v1/responses", `{"model":"sol"}`)
			want := 403
			if unavailable {
				want = 503
			}
			if w.Code != want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
