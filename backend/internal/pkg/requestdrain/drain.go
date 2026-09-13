// Package requestdrain keeps request finalizers alive until billing is durable.
// Client cancellation and process shutdown are deliberately different signals.
package requestdrain

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type shutdownKey struct{}

type Drain struct {
	mu       sync.Mutex
	active   int
	stopping bool
	done     chan struct{}
	force    context.Context
	cancel   context.CancelFunc
}

func New() *Drain {
	ctx, cancel := context.WithCancel(context.Background())
	return &Drain{done: make(chan struct{}), force: ctx, cancel: cancel}
}

// Wrap includes hijacked WebSocket handlers and their synchronous billing, which
// http.Server.Shutdown does not wait for. It does not buffer or wrap the writer.
func (d *Drain) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		if d.stopping {
			d.mu.Unlock()
			http.Error(w, "Server is restarting; retry shortly", http.StatusServiceUnavailable)
			return
		}
		d.active++
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			d.active--
			if d.stopping && d.active == 0 {
				close(d.done)
			}
			d.mu.Unlock()
		}()
		ctx, cancel := context.WithCancel(context.WithValue(r.Context(), shutdownKey{}, d.force))
		interrupted := make(chan struct{})
		stop := context.AfterFunc(d.force, func() {
			defer close(interrupted)
			cancel()
			// A canceled context alone cannot release blocked client Body.Read
			// or ResponseWriter.Write. Interrupt I/O only on process shutdown;
			// detached billing remains alive and is still included in Wait.
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(time.Now())
			_ = controller.SetWriteDeadline(time.Now())
		})
		defer func() {
			if !stop() {
				<-interrupted // Do not touch a writer after net/http recycles it.
			}
			cancel()
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (d *Drain) StopAccepting() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.stopping {
		d.stopping = true
		if d.active == 0 {
			close(d.done)
		}
	}
}

// Cancel interrupts request I/O, not detached billing transactions.
// Always Wait before closing the database or the accounting workers.
func (d *Drain) Cancel() { d.cancel() }

func (d *Drain) Wait(ctx context.Context) error {
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// upstreamContext takes cancellation from process shutdown and values from the
// request. It does not register a callback per upstream request/retry.
type upstreamContext struct {
	context.Context
	values context.Context
}

func (c upstreamContext) Value(key any) any { return c.values.Value(key) }

// Detach lets an ordinary disconnected client finish collecting upstream usage,
// but retains the process-stop signal. Keep the existing no-op release contract:
// gateway builders release immediately after constructing an http.Request, not
// after reading its response. Billing must NOT use this helper.
func Detach(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	base := context.WithoutCancel(ctx)
	force, ok := ctx.Value(shutdownKey{}).(context.Context)
	if !ok {
		return base, func() {}
	}
	return upstreamContext{Context: force, values: base}, func() {}
}
