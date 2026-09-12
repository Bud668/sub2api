package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SubscriptionHandler) ListGroupDynamicQuotas(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if h.subscriptionService == nil || h.subscriptionService.DynamicQuotas == nil {
		response.ErrorFrom(c, service.ErrDynamicQuotaUnavailable)
		return
	}
	out, err := h.subscriptionService.DynamicQuotas.GroupPolicies(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, out)
}

func (h *SubscriptionHandler) GetGroupDynamicQuota(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid group ID")
		return
	}
	if h.subscriptionService == nil || h.subscriptionService.DynamicQuotas == nil {
		response.ErrorFrom(c, service.ErrDynamicQuotaUnavailable)
		return
	}
	out, err := h.subscriptionService.DynamicQuotas.GroupStatus(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, out)
}

func (h *SubscriptionHandler) SaveGroupDynamicQuota(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	middleware2.SetAuditAction(c, "admin.group.dynamic_quota.update")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid group ID")
		return
	}
	if h.subscriptionService == nil || h.subscriptionService.DynamicQuotas == nil {
		response.ErrorFrom(c, service.ErrDynamicQuotaUnavailable)
		return
	}
	var in service.DynamicSubscriptionInput
	d := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	d.DisallowUnknownFields()
	if err = d.Decode(&in); err != nil {
		response.BadRequest(c, "Invalid dynamic quota settings")
		return
	}
	if err = d.Decode(new(any)); err != io.EOF {
		response.BadRequest(c, "Expected one JSON object")
		return
	}
	if err = h.subscriptionService.DynamicQuotas.SaveGroup(c.Request.Context(), id, in); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.GetGroupDynamicQuota(c)
}
