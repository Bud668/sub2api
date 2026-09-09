package service

import (
	"context"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
)

type dynamicQuotaSourceContextKey struct{}
type dynamicQuotaForwardContextKey struct{}
type dynamicQuotaRequestContextKey struct{}

func markDynamicQuotaDispatched(ctx context.Context) {
	if r, ok := ctx.Value(dynamicQuotaRequestContextKey{}).(*DynamicQuotaReservation); ok {
		r.MarkDispatched()
	}
}

const OpsDynamicQuotaErrorKey = "ops_dynamic_quota_error"

func isDynamicQuotaError(err error) bool {
	return strings.HasPrefix(infraerrors.Reason(err), "DYNAMIC_QUOTA_")
}

// No provider attribution or request content is needed for a local quota denial.
func MarkDynamicQuotaRejected(c *gin.Context, err error) {
	if c == nil {
		return
	}
	if !isDynamicQuotaError(err) {
		err = ErrDynamicQuotaUnavailable
	}
	c.Set(OpsDynamicQuotaErrorKey, err)
	if infraerrors.Code(err) < 500 {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
	}
}

// WS uses per-turn holds in BeforeRequest, including the http_bridge adapter.
func WithDynamicQuotaWSContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, dynamicQuotaForwardContextKey{}, true)
}

func WithDynamicQuotaSource(ctx context.Context, accountID int64) context.Context {
	return context.WithValue(ctx, dynamicQuotaSourceContextKey{}, accountID)
}

func dynamicQuotaAccountAllowed(ctx context.Context, account *Account) bool {
	id, _ := ctx.Value(dynamicQuotaSourceContextKey{}).(int64)
	return id <= 0 || (account != nil && account.ID == id)
}

func (s *OpenAIGatewayService) withDynamicQuotaForward(ctx context.Context, c *gin.Context, account *Account, forward func(context.Context) (*OpenAIForwardResult, error)) (*OpenAIForwardResult, error) {
	if ctx.Value(dynamicQuotaForwardContextKey{}) != nil || s.DynamicQuotas == nil {
		return forward(ctx)
	}
	r, err := s.DynamicQuotas.Begin(ctx, getAPIKeyIDFromContext(c), account.ID)
	if err != nil {
		MarkDynamicQuotaRejected(c, err)
		if c != nil && !c.Writer.Written() {
			status := infraerrors.Code(err)
			if status < 400 || status > 599 {
				status = http.StatusServiceUnavailable
			}
			if status < 500 {
				MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
			}
			c.JSON(status, gin.H{"error": gin.H{"type": "rate_limit_error", "code": infraerrors.Reason(err), "message": infraerrors.Message(err)}})
			MarkResponseCommitted(c)
		}
		return nil, err
	}
	size := 0
	if c != nil {
		size = c.Writer.Size()
	}
	ctx = context.WithValue(ctx, dynamicQuotaForwardContextKey{}, true)
	ctx = context.WithValue(ctx, dynamicQuotaRequestContextKey{}, r)
	result, err := forward(ctx)
	wrote := c != nil && c.Writer.Size() > size && c.Writer.Status() < 400
	r.Finish(result, err, wrote)
	return result, err
}

func (s *OpenAIGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	return s.withDynamicQuotaForward(ctx, c, account, func(inner context.Context) (*OpenAIForwardResult, error) {
		return s.forwardDynamicQuotaChecked(inner, c, account, body)
	})
}

func (s *OpenAIGatewayService) ForwardAsAnthropic(ctx context.Context, c *gin.Context, account *Account, body []byte, promptCacheKey, defaultMappedModel string) (*OpenAIForwardResult, error) {
	return s.withDynamicQuotaForward(ctx, c, account, func(inner context.Context) (*OpenAIForwardResult, error) {
		return s.forwardAsAnthropicDynamicQuotaChecked(inner, c, account, body, promptCacheKey, defaultMappedModel)
	})
}
