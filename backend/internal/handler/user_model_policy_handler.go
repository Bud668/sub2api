package handler

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *APIKeyHandler) modelPolicyService(c *gin.Context) *service.UserModelPolicyService {
	c.Header("Cache-Control", "private, no-store")
	if h == nil || h.apiKeyService == nil || h.apiKeyService.ModelPolicies == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"message": "Model policy service unavailable"})
		return nil
	}
	return h.apiKeyService.ModelPolicies
}

func modelPolicyUserID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid user ID")
		return 0, false
	}
	return id, true
}

func (h *APIKeyHandler) AdminGetModelPolicy(c *gin.Context) {
	s := h.modelPolicyService(c)
	if s == nil {
		return
	}
	id, ok := modelPolicyUserID(c)
	if !ok {
		return
	}
	p, err := s.Status(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, p)
}

func (h *APIKeyHandler) GetMyModelPolicy(c *gin.Context) {
	s := h.modelPolicyService(c)
	if s == nil {
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	p, err := s.Status(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// A prepared draft is visible only to administrators until activation.
	if !p.Enabled {
		p.Rules = []service.UserModelRequestRule{}
		p.Windows = []service.UserModelRequestWindow{}
	}
	response.Success(c, p)
}

func (h *APIKeyHandler) AdminSaveModelPolicy(c *gin.Context) {
	s := h.modelPolicyService(c)
	if s == nil {
		return
	}
	id, ok := modelPolicyUserID(c)
	if !ok {
		return
	}
	var req struct {
		Revision int64                          `json:"revision"`
		Rules    []service.UserModelRequestRule `json:"rules"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid model policy")
		return
	}
	middleware.SetAuditAction(c, "admin.user.model_policy.update")
	if err := s.Save(c.Request.Context(), id, req.Revision, req.Rules); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.AdminGetModelPolicy(c)
}

func (h *APIKeyHandler) AdminResetModelQuota(c *gin.Context) {
	s := h.modelPolicyService(c)
	if s == nil {
		return
	}
	id, ok := modelPolicyUserID(c)
	if !ok {
		return
	}
	var req struct {
		Model string `json:"model" binding:"required,max=200"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid model")
		return
	}
	middleware.SetAuditAction(c, "admin.user.model_quota.reset")
	if err := s.Reset(c.Request.Context(), id, req.Model); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.AdminGetModelPolicy(c)
}

func (h *APIKeyHandler) AdminActivateModelPolicies(c *gin.Context) {
	s := h.modelPolicyService(c)
	if s == nil {
		return
	}
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Confirm != "enable-user-model-policies" {
		response.BadRequest(c, "Explicit migration confirmation required")
		return
	}
	middleware.SetAuditAction(c, "admin.user.model_policy.activate")
	if err := s.Activate(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"enabled": true})
}
