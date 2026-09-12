package requestdrain

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("request finalizer did not finish")
	}
}

func TestClientDisconnectStillCollectsUsageButShutdownWaitsForBilling(t *testing.T) {
	d := New()
	defer d.Cancel()
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	started, readEnded, commit, finished := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var upstream context.Context
	h := d.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var release context.CancelFunc
		upstream, release = Detach(r.Context())
		release() // Gateway builders release before doing upstream I/O.
		close(started)
		<-upstream.Done()
		// Real gateway billing also ignores request cancellation, with a timeout.
		billing, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
		defer cancel()
		if billing.Err() != nil {
			t.Error("shutdown canceled billing")
		}
		close(readEnded)
		<-commit
	}))
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/responses", nil).WithContext(clientCtx))
		close(finished)
	}()
	await(t, started)
	if upstream.Err() != nil {
		t.Fatal("builder release canceled upstream before I/O")
	}
	clientCancel()
	if upstream.Err() != nil {
		t.Fatal("ordinary client disconnect stopped usage collection")
	}
	d.StopAccepting()
	d.Cancel()
	await(t, readEnded)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if d.Wait(canceled) == nil {
		t.Fatal("allowed database cleanup before bill commit")
	}
	close(commit)
	await(t, finished)
	ctx, release := context.WithTimeout(context.Background(), time.Second)
	defer release()
	if err := d.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	rejected := httptest.NewRecorder()
	h.ServeHTTP(rejected, httptest.NewRequest("GET", "/v1/responses", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatal("accepted a new request during shutdown")
	}
}

func TestHijackedRequestIsNotLostWhenHTTPShutdownReturns(t *testing.T) {
	d := New()
	defer d.Cancel()
	started, finished := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(d.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\n\r\n")
		close(started)
		<-r.Context().Done()
		close(finished)
	})))
	defer s.Close()
	conn, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: localhost\r\n\r\n")
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	await(t, started)
	d.StopAccepting()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Config.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
		t.Fatal("HTTP shutdown unexpectedly finalized hijacked request")
	default:
	}
	d.Cancel()
	if err := d.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, finished)
}

func TestConcurrentAdmissionAndRepeatedStopAreSafe(t *testing.T) {
	d := New()
	defer d.Cancel()
	h := d.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil)) }()
	}
	d.StopAccepting()
	d.StopAccepting()
	d.Cancel()
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
