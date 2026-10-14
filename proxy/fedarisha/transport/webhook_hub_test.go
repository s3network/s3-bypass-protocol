package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func notificationFor(hub *WebhookHub, key string) *http.Request {
	body, _ := json.Marshal(s3Notification{Records: []s3Record{{
		EventName: "s3:ObjectCreated:Put",
		S3:        s3Detail{Object: s3Object{Key: key, Size: 10}},
	}}})
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	hub.ServeHTTP(httptest.NewRecorder(), req)
	return req
}

// Unregister used to close the channel, while handleRecord took the channel
// under RLock, released it, and only then sent. A teardown in that window made
// the send panic — "send on closed channel" — which kills the process, not just
// the session. Unregistering must be safe against a concurrent notification.
func TestUnregisterRacesNoLongerPanic(t *testing.T) {
	hub := NewWebhookHub("", "http://127.0.0.1:1/webhook")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 500; n++ {
				id := fmt.Sprintf("sess-%d", n%8)
				hub.Register(id)
				notificationFor(hub, "sessions/"+id+"/c_0_"+id)
				hub.Unregister(id)
			}
		}()
	}
	wg.Wait()
}

// A registered session must still be woken.
func TestNotificationReachesARegisteredSession(t *testing.T) {
	hub := NewWebhookHub("", "http://127.0.0.1:1/webhook")
	ch := hub.Register("abc")

	notificationFor(hub, "sessions/abc/c_0_5")

	select {
	case <-ch:
	default:
		t.Fatal("a data file for a registered session must wake its poll loop")
	}
}

// Unregister must not close the channel a live poll loop is selecting on: that
// would make every such select return immediately and spin.
func TestUnregisterDoesNotCloseTheChannel(t *testing.T) {
	hub := NewWebhookHub("", "http://127.0.0.1:1/webhook")
	ch := hub.Register("abc")
	hub.Unregister("abc")

	select {
	case _, open := <-ch:
		if !open {
			t.Fatal("the channel must not be closed: the poll loop selects on it")
		}
	default:
	}

	// And the hub must have forgotten the session entirely.
	notificationFor(hub, "sessions/abc/c_0_5")
	select {
	case <-ch:
		t.Fatal("an unregistered session must not be notified")
	default:
	}
}

func TestHubRejectsAnOversizedBody(t *testing.T) {
	hub := NewWebhookHub("", "http://127.0.0.1:1/webhook")
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(strings.Repeat("x", maxNotificationBytes+1)))
	rec := httptest.NewRecorder()
	hub.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d for a body past the limit", rec.Code, http.StatusBadRequest)
	}
}
