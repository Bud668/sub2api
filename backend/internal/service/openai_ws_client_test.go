package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestCoderOpenAIWSClientDialer_ProxyHTTPClientReuse(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	c1, err := impl.proxyHTTPClient("http://127.0.0.1:8080")
	require.NoError(t, err)
	c2, err := impl.proxyHTTPClient("http://127.0.0.1:8080")
	require.NoError(t, err)
	require.Same(t, c1, c2, "同一代理地址应复用同一个 HTTP 客户端")

	c3, err := impl.proxyHTTPClient("http://127.0.0.1:8081")
	require.NoError(t, err)
	require.NotSame(t, c1, c3, "不同代理地址应分离客户端")
}

func TestCoderOpenAIWSClientDialer_ProxyHTTPClientInvalidURL(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	_, err := impl.proxyHTTPClient("://bad")
	require.Error(t, err)
}

func TestCoderOpenAIWSClientDialer_TransportMetricsSnapshot(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	_, err := impl.proxyHTTPClient("http://127.0.0.1:18080")
	require.NoError(t, err)
	_, err = impl.proxyHTTPClient("http://127.0.0.1:18080")
	require.NoError(t, err)
	_, err = impl.proxyHTTPClient("http://127.0.0.1:18081")
	require.NoError(t, err)

	snapshot := impl.SnapshotTransportMetrics()
	require.Equal(t, int64(1), snapshot.ProxyClientCacheHits)
	require.Equal(t, int64(2), snapshot.ProxyClientCacheMisses)
	require.InDelta(t, 1.0/3.0, snapshot.TransportReuseRatio, 0.0001)
}

func TestCoderOpenAIWSClientDialer_ProxyClientCacheCapacity(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	total := openAIWSProxyClientCacheMaxEntries + 32
	for i := 0; i < total; i++ {
		_, err := impl.proxyHTTPClient(fmt.Sprintf("http://127.0.0.1:%d", 20000+i))
		require.NoError(t, err)
	}

	impl.proxyMu.Lock()
	cacheSize := len(impl.proxyClients)
	impl.proxyMu.Unlock()

	require.LessOrEqual(t, cacheSize, openAIWSProxyClientCacheMaxEntries, "代理客户端缓存应受容量上限约束")
}

func TestCoderOpenAIWSClientDialer_ProxyClientCacheIdleTTL(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	oldProxy := "http://127.0.0.1:28080"
	_, err := impl.proxyHTTPClient(oldProxy)
	require.NoError(t, err)

	impl.proxyMu.Lock()
	oldEntry := impl.proxyClients[oldProxy]
	require.NotNil(t, oldEntry)
	oldEntry.lastUsedUnixNano = time.Now().Add(-openAIWSProxyClientCacheIdleTTL - time.Minute).UnixNano()
	impl.proxyMu.Unlock()

	// 触发一次新的代理获取，驱动 TTL 清理。
	_, err = impl.proxyHTTPClient("http://127.0.0.1:28081")
	require.NoError(t, err)

	impl.proxyMu.Lock()
	_, exists := impl.proxyClients[oldProxy]
	impl.proxyMu.Unlock()

	require.False(t, exists, "超过空闲 TTL 的代理客户端应被回收")
}

func TestCoderOpenAIWSClientDialer_ProxyTransportTLSHandshakeTimeout(t *testing.T) {
	dialer := newDefaultOpenAIWSClientDialer()
	impl, ok := dialer.(*coderOpenAIWSClientDialer)
	require.True(t, ok)

	client, err := impl.proxyHTTPClient("http://127.0.0.1:38080")
	require.NoError(t, err)
	require.NotNil(t, client)

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport)
	require.Equal(t, 10*time.Second, transport.TLSHandshakeTimeout)
}

func TestCoderOpenAIWSClientConn_DoesNotSupportIdlePingWithoutReader(t *testing.T) {
	require.False(t, (&coderOpenAIWSClientConn{}).SupportsIdlePingWithoutReader())
}

// Real sockets exercise control-frame processing, unlike the pool's fake dialers.
func newTestOpenAIWSClientPair(t *testing.T) (*coderOpenAIWSClientConn, *coderws.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	accepted := make(chan *coderws.Conn, 1)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		accepted <- conn
		<-done
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(done) })
	adapter, _, _, err := newDefaultOpenAIWSClientDialer().Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil, "")
	require.NoError(t, err)
	client := adapter.(*coderOpenAIWSClientConn)
	t.Cleanup(func() {
		_ = client.conn.CloseNow()
		select {
		case <-client.readDone:
		case <-time.After(time.Second):
			t.Error("upstream read pump did not stop")
		}
	})
	select {
	case upstream := <-accepted:
		t.Cleanup(func() { _ = upstream.CloseNow() })
		return client, upstream, ctx
	case <-ctx.Done():
		t.Fatal("local websocket handshake timed out")
		return nil, nil, nil
	}
}

func TestCoderOpenAIWSClientConn_IdleHeartbeat(t *testing.T) {
	client, upstream, ctx := newTestOpenAIWSClientPair(t)
	upstream.CloseRead(ctx) // Processes pongs; this test sends no client data.
	lease := &openAIWSConnLease{conn: newOpenAIWSConn("heartbeat", 1, client, nil)}
	require.True(t, lease.SupportsIdlePingWithoutReader())
	require.NoError(t, upstream.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed"}`)))
	_, err := lease.ReadMessageContext(ctx)
	require.NoError(t, err)
	// No lease reader now: even unsolicited metadata must not stall the pump.
	for i := 0; i < 3; i++ {
		require.NoError(t, upstream.Write(ctx, coderws.MessageText, []byte(`{"type":"rate_limits.updated"}`)))
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		require.NoError(t, upstream.Ping(pingCtx))
		cancel()
		require.NoError(t, lease.PingWithTimeout(time.Second))
	}
	for i := 0; i < 3; i++ {
		payload, err := lease.ReadMessageContext(ctx)
		require.NoError(t, err)
		require.Contains(t, string(payload), "rate_limits.updated")
	}
}

func TestCoderOpenAIWSClientConn_OrderedFramesBeforeClose(t *testing.T) {
	client, upstream, ctx := newTestOpenAIWSClientPair(t)
	for _, kind := range []coderws.MessageType{coderws.MessageText, coderws.MessageBinary} {
		require.NoError(t, upstream.Write(ctx, kind, []byte("delta")))
	}
	require.NoError(t, upstream.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed"}`)))
	require.NoError(t, upstream.Close(coderws.StatusGoingAway, "test complete"))
	for _, kind := range []coderws.MessageType{coderws.MessageText, coderws.MessageBinary} {
		actual, payload, err := client.ReadFrame(ctx)
		require.NoError(t, err)
		require.Equal(t, kind, actual)
		require.Equal(t, "delta", string(payload))
	}
	payload, err := client.ReadMessage(ctx)
	require.NoError(t, err)
	require.Contains(t, string(payload), "response.completed")
	for i := 0; i < 2; i++ {
		_, err = client.ReadMessage(ctx)
		require.Equal(t, coderws.StatusGoingAway, coderws.CloseStatus(err))
	}
	require.Zero(t, client.queuedBytes.Load())
}

func TestCoderOpenAIWSClientConn_CancelAndClose(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout", "close"} {
		t.Run(mode, func(t *testing.T) {
			client, upstream, ctx := newTestOpenAIWSClientPair(t)
			upstream.CloseRead(ctx)
			if mode == "close" {
				require.NoError(t, client.Close())
				require.NoError(t, client.Close())
			} else {
				readCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
				if mode == "cancel" {
					cancel()
				}
				_, err := client.ReadMessage(readCtx)
				require.ErrorIs(t, err, readCtx.Err())
			}
			select {
			case <-client.readDone:
			case <-ctx.Done():
				t.Fatal("close/cancel leaked the reader")
			}
			_, err := client.ReadMessage(ctx)
			require.Error(t, err)
			require.Error(t, client.Ping(ctx))
		})
	}
}

func TestCoderOpenAIWSClientConn_BoundedBacklog(t *testing.T) {
	for _, size := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			client, upstream, ctx := newTestOpenAIWSClientPair(t)
			payload := bytes.Repeat([]byte("x"), size)
			limit := min(openAIWSReadQueueFrames, openAIWSReadQueueBytes/size)
			for i := 0; i <= limit; i++ {
				// Overflow may close the socket before the final write returns.
				if err := upstream.Write(ctx, coderws.MessageText, payload); err != nil {
					break
				}
			}
			select {
			case <-client.readDone:
			case <-ctx.Done():
				t.Fatal("full backlog blocked the reader")
			}
			require.Equal(t, int64(limit*size), client.queuedBytes.Load())
			for i := 0; i < limit; i++ {
				got, err := client.ReadMessage(ctx)
				require.NoError(t, err)
				require.Equal(t, payload, got)
			}
			_, err := client.ReadMessage(ctx)
			require.ErrorIs(t, err, errOpenAIWSReadBacklog)
			require.Zero(t, client.queuedBytes.Load())
		})
	}
}
