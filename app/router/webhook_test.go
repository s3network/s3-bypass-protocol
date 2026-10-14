package router

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	routing_session "github.com/xtls/xray-core/features/routing/session"
)

func newTestWebhookNotifier(t *testing.T, handler http.Handler) *WebhookNotifier {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	h, err := NewWebhookNotifier(&WebhookConfig{Url: server.URL})
	if err != nil {
		t.Fatal("failed to create webhook notifier:", err)
	}
	if h == nil {
		t.Fatal("expected a notifier for a configured url")
	}
	return h
}

// Close must wait for the notifications that are already in flight.
func TestWebhookNotifierCloseWaitsForPendingPosts(t *testing.T) {
	var posted atomic.Int32
	h := newTestWebhookNotifier(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted.Add(1)
	}))

	h.Fire(&routing_session.Context{}, "out")
	if err := h.Close(); err != nil {
		t.Fatal("failed to close notifier:", err)
	}
	if posted.Load() != 1 {
		t.Error("Close returned before the pending POST was delivered")
	}

	// firing on a closed notifier must be a no-op instead of a panic
	h.Fire(&routing_session.Context{}, "out")
	if posted.Load() != 1 {
		t.Error("a notifier fired a notification after being closed")
	}
}

// Fire and Close used to race on the wait group: wg.Add(1) was called before
// the "is it closed" check, so Close could run wg.Wait() concurrently with an
// Add and panic with "Add called concurrently with Wait".
func TestWebhookNotifierFireWhileClosing(t *testing.T) {
	h := newTestWebhookNotifier(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
	}))

	ctx := &routing_session.Context{}

	var fire sync.WaitGroup
	for i := 0; i < 16; i++ {
		fire.Add(1)
		go func() {
			defer fire.Done()
			for j := 0; j < 64; j++ {
				h.Fire(ctx, "out")
			}
		}()
	}

	var closers sync.WaitGroup
	for i := 0; i < 4; i++ {
		closers.Add(1)
		go func() {
			defer closers.Done()
			if err := h.Close(); err != nil {
				t.Error("failed to close notifier:", err)
			}
		}()
	}

	fire.Wait()
	closers.Wait()

	// Close is idempotent
	if err := h.Close(); err != nil {
		t.Error("failed to close notifier a second time:", err)
	}
}
