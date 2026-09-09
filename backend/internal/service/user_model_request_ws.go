package service

import (
	"context"
	"errors"
	"sync"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type userModelWSSlot struct {
	model       string
	reservation *UserModelRequestReservation
	evidence    UserModelRequestEvidence
}

// The map follows relay turn IDs; a failed logical turn may restart at turn 1
// on a different upstream account. Carry its reservation across that retry.
type UserModelRequestWSTracker struct {
	service *UserModelPolicyService
	userID  int64
	mu      sync.Mutex
	slots   map[int]*userModelWSSlot
	retry   *userModelWSSlot
}

func NewUserModelRequestWSTracker(s *UserModelPolicyService, userID int64) *UserModelRequestWSTracker {
	return &UserModelRequestWSTracker{service: s, userID: userID, slots: make(map[int]*userModelWSSlot)}
}

func (t *UserModelRequestWSTracker) Check(ctx context.Context, candidates []string) (bool, error) {
	if t.service == nil {
		return false, nil
	}
	p, err := t.service.Load(ctx, t.userID)
	if err != nil {
		return false, err
	}
	if !p.Enabled {
		return false, nil
	}
	var model string
	for _, candidate := range candidates {
		if !p.Allows(candidate) {
			return true, modelNotAllowed(candidate)
		}
		canonical := CanonicalUserModel(candidate)
		if model != "" && canonical != model {
			return true, infraerrors.BadRequest("AMBIGUOUS_MODEL", "Conflicting model values are not allowed")
		}
		model = canonical
	}
	if model == "" {
		return true, modelNotAllowed("")
	}
	return true, nil
}

func (t *UserModelRequestWSTracker) Begin(ctx context.Context, turn int, model string) error {
	if t.service == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	model = CanonicalUserModel(model)
	if existing := t.slots[turn]; existing != nil {
		if existing.model != model {
			return infraerrors.BadRequest("MODEL_CHANGED_DURING_RETRY", "Model changed during retry")
		}
		return nil
	}
	if t.retry != nil {
		if t.retry.model != model {
			return infraerrors.BadRequest("MODEL_CHANGED_DURING_RETRY", "Model changed during retry")
		}
		t.slots[turn], t.retry = t.retry, nil
		t.slots[turn].evidence.BeginAttempt()
		return nil
	}
	r, err := t.service.Reserve(ctx, t.userID, model)
	if err != nil {
		return err
	}
	t.slots[turn] = &userModelWSSlot{model: model, reservation: r}
	return nil
}

func (t *UserModelRequestWSTracker) After(turn int, result *OpenAIForwardResult, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	slot := t.slots[turn]
	if slot == nil {
		return
	}
	slot.evidence.Observe(result, err, false)
	delete(t.slots, turn)
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) {
		if t.retry != nil {
			t.retry.reservation.Finish(t.retry.evidence.RefundHTTP(503, nil))
		}
		t.retry = slot
		return
	}
	slot.reservation.Finish(slot.evidence.RefundHTTP(200, err))
}

func (t *UserModelRequestWSTracker) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, slot := range t.slots {
		slot.reservation.Finish(slot.evidence.RefundHTTP(200, context.Canceled))
	}
	if t.retry != nil {
		t.retry.reservation.Finish(t.retry.evidence.RefundHTTP(503, nil))
	}
	t.slots = nil
	t.retry = nil
}

func (t *UserModelRequestWSTracker) RejectBeforeForward(turn int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if slot := t.slots[turn]; slot != nil {
		slot.evidence.RejectedBeforeForward()
		slot.reservation.Finish(slot.evidence.RefundHTTP(403, nil))
		delete(t.slots, turn)
	}
}
