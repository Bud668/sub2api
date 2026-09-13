package requestdrain

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Cancellation alone does not interrupt net/http's request Body.Read or a
// blocked ResponseWriter.Write. Exercise real sockets, not a context-aware fake.
func TestShutdownUnblocksClientIOAndStillWaitsForFinalizer(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			d := New()
			defer d.Cancel()
			started, ioEnded, commit, finished := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			s := httptest.NewServer(d.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				if direction == "upload" {
					_, _ = io.Copy(io.Discard, r.Body)
				} else {
					block := make([]byte, 64<<10)
					for {
						if _, err := w.Write(block); err != nil {
							break
						}
					}
				}
				close(ioEnded)
				<-commit // Billing must remain alive after transport cancellation.
				close(finished)
			})))
			conn, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
			if err != nil {
				s.Close()
				t.Fatal(err)
			}
			defer func() { conn.Close(); close(commit); s.Close() }()
			if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			request := "GET /responses HTTP/1.1\r\nHost: localhost\r\n\r\n"
			if direction == "upload" {
				request = "POST /responses HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1000000\r\n\r\nx"
			}
			if _, err := fmt.Fprint(conn, request); err != nil {
				t.Fatal(err)
			}
			await(t, started)
			d.StopAccepting()
			shutdown, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancelShutdown()
			_ = s.Config.Shutdown(shutdown)
			d.Cancel()
			select {
			case <-ioEnded:
			case <-time.After(time.Second):
				t.Fatal("client I/O remained blocked after process cancellation")
			}
			select {
			case <-finished:
				t.Fatal("billing finalizer was skipped")
			default:
			}
			check, cancelCheck := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancelCheck()
			if err := d.Wait(check); err == nil {
				t.Fatal("shutdown ignored a live billing finalizer")
			}
		})
	}
}
