package service

import (
	"context"
	"errors"
	"sync"
)

type userModelEvidenceContextKey struct{}

// Metadata only. Neither request bodies nor output text are stored to count.
type UserModelRequestEvidence struct {
	mu                            sync.Mutex
	forwarded, consumed, rejected bool
}

func WithUserModelRequestEvidence(ctx context.Context, evidence *UserModelRequestEvidence) context.Context {
	return context.WithValue(ctx, userModelEvidenceContextKey{}, evidence)
}

func ObserveUserModelRequest(ctx context.Context, result *OpenAIForwardResult, err error, wroteOutput bool) {
	if evidence, ok := ctx.Value(userModelEvidenceContextKey{}).(*UserModelRequestEvidence); ok {
		evidence.Observe(result, err, wroteOutput)
	}
}

func (e *UserModelRequestEvidence) Observe(result *OpenAIForwardResult, err error, wroteOutput bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forwarded = true
	e.rejected = false // A prior rejected retry says nothing about this attempt.
	consumed := wroteOutput && (result == nil || result.SucceededForScheduling())
	if result != nil {
		consumed = consumed || result.Usage.InputTokens > 0 || result.Usage.OutputTokens > 0 || result.Usage.CacheReadInputTokens > 0 || result.Usage.CacheCreationInputTokens > 0 || result.FirstTokenMs != nil || result.ImageCount > 0 || result.ClientDisconnect
		if err == nil && result.SucceededForScheduling() {
			consumed = true
		}
	}
	e.consumed = e.consumed || consumed
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) {
		e.rejected = true
	}
	if result != nil && !result.SucceededForScheduling() && !consumed && err == nil {
		e.rejected = true
	}
}

func (e *UserModelRequestEvidence) RefundHTTP(status int, ctxErr error) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Unknown disconnects are not free requests. A successful later retry wins
	// over an earlier rejection. Pure local/pre-dispatch failures are refunded.
	return !e.consumed && (e.rejected || (!e.forwarded && ctxErr == nil && status >= 400))
}

func (e *UserModelRequestEvidence) BeginAttempt() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rejected = false
}

func (e *UserModelRequestEvidence) RejectedBeforeForward() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rejected = true
}
