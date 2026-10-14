package browser_dialer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestWebsocketConn returns a real websocket connection to a throwaway
// server, so tests exercise the same *websocket.Conn type the dialer uses.
func newTestWebsocketConn(t *testing.T) *websocket.Conn {
	t.Helper()

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Acknowledge every task with "ok", like the real dialer page does.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte("ok"))
		}
	}))
	t.Cleanup(server.Close)

	u, err := url.Parse("ws" + strings.TrimPrefix(server.URL, "http") + "/")
	require.NoError(t, err)

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// newClosedWebsocketConn returns a connection whose writes always fail.
func newClosedWebsocketConn(t *testing.T) *websocket.Conn {
	t.Helper()
	conn := newTestWebsocketConn(t)
	require.NoError(t, conn.Close())
	return conn
}

// Reload must be safe to call while dials are in flight, and HasBrowserDialer
// must never observe a torn state (this is run under -race).
func TestReloadAndHasBrowserDialerAreRaceFree(t *testing.T) {
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = HasBrowserDialer()
		}
	}()

	for range 50 {
		Reload()
	}
	stop.Store(true)
	<-done

	Reload()
	assert.False(t, HasBrowserDialer())
}

// Without a configured dialer, a dial must fail fast instead of blocking on a
// nil channel forever.
func TestDialTaskWithoutDialerFailsFast(t *testing.T) {
	Reload()
	require.False(t, HasBrowserDialer())

	done := make(chan error, 1)
	go func() {
		_, err := dialTask(context.Background(), task{Method: "GET", URL: "http://example.com/"})
		done <- err
	}()

	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("dialTask blocked forever without a browser dialer")
	}
}

// The wait for a browser connection must end when the caller's context is done,
// and it must surface the context error.
func TestDialTaskHonorsContextCancellation(t *testing.T) {
	current.Store(&dialerState{addr: "127.0.0.1:0", conns: make(chan *websocket.Conn, 1)})
	t.Cleanup(func() { current.Store(nil) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := dialTask(ctx, task{Method: "WS", URL: "ws://example.com/"})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second, "dialTask ignored the cancelled context")
}

// A dial that keeps pulling dead browser connections must give up after a
// bounded number of attempts instead of retrying forever.
func TestDialTaskGivesUpOnDeadConnections(t *testing.T) {
	const dead = maxDialAttempts + 4
	conns := make(chan *websocket.Conn, dead)
	for range dead {
		conns <- newClosedWebsocketConn(t)
	}
	current.Store(&dialerState{addr: "127.0.0.1:0", conns: conns})
	t.Cleanup(func() { current.Store(nil) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err := dialTask(ctx, task{Method: "GET", URL: "http://example.com/"})
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "dialTask retried without a bound")
	// Exactly maxDialAttempts connections may have been consumed.
	assert.Equal(t, dead-maxDialAttempts, len(conns))
}

// A working browser connection completes the dial.
func TestDialTaskSucceedsWithLiveConnection(t *testing.T) {
	conns := make(chan *websocket.Conn, 1)
	conns <- newTestWebsocketConn(t)
	current.Store(&dialerState{addr: "127.0.0.1:0", conns: conns})
	t.Cleanup(func() { current.Store(nil) })

	conn, err := dialTask(context.Background(), task{Method: "WS", URL: "ws://example.com/"})
	require.NoError(t, err)
	require.NotNil(t, conn)
	_ = conn.Close()
}

// The public helpers must propagate the caller's context.
func TestDialGetHonorsContextCancellation(t *testing.T) {
	current.Store(&dialerState{addr: "127.0.0.1:0", conns: make(chan *websocket.Conn, 1)})
	t.Cleanup(func() { current.Store(nil) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := DialGet(ctx, "http://example.com/", http.Header{}, nil)
	assert.ErrorIs(t, err, context.Canceled)

	assert.ErrorIs(t, DialPacket(ctx, "POST", "http://example.com/", http.Header{}, nil, []byte("x")), context.Canceled)

	_, err = DialWS(ctx, "ws://example.com/", nil)
	assert.ErrorIs(t, err, context.Canceled)
}
