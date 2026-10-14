package transport

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/proxy/fedarisha/storage"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// wedgingStore accepts a call and then says nothing until the caller's context
// ends — the failure mode the per-call deadlines exist for.
//
// It honours ctx because a real backend does: the SDK's HTTP round trip is
// bound by the context it was handed. That is precisely why the missing piece
// was a deadline rather than a timeout inside the call — with the listener's own
// context, which has none, the call waits on a store that is never going to
// answer.
type wedgingStore struct {
	storage.Storage
}

func (wedgingStore) List(ctx context.Context, _, _ string) ([]storage.FileInfo, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (wedgingStore) Download(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The listener's List runs on the watchLoop goroutine, which is the gateway to
// session discovery for EVERY user on the inbound. With the listener's own
// context and no deadline, one backend that accepts the request and then goes
// quiet stops new sessions being found for the whole inbound — silently, because
// the scan is the only thing that goroutine does.
func TestScanForNewSessionsDoesNotHangOnAStuckList(t *testing.T) {
	restore := listCallTimeoutNanos.Load()
	listCallTimeoutNanos.Store(int64(100 * time.Millisecond))
	defer listCallTimeoutNanos.Store(restore)

	l := &Listener{
		Store:       wedgingStore{Storage: newFakeStore()},
		SessionsDir: DefaultSessionsDir,
		MultiUser:   true,
		ctx:         testContext(t),
	}

	done := make(chan struct{})
	go func() {
		l.scanForNewSessions()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck List stopped session discovery for the whole inbound")
	}
}

func TestScanSessionsInDoesNotHangOnAStuckList(t *testing.T) {
	restore := listCallTimeoutNanos.Load()
	listCallTimeoutNanos.Store(int64(100 * time.Millisecond))
	defer listCallTimeoutNanos.Store(restore)

	l := &Listener{
		Store:       wedgingStore{Storage: newFakeStore()},
		SessionsDir: DefaultSessionsDir,
		ctx:         testContext(t),
	}

	done := make(chan struct{})
	go func() {
		l.scanSessionsIn(DefaultSessionsDir)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck List parked the watch loop")
	}
}

// The handshake's own GET has the same exposure: it occupies one of the 16
// handshake slots, and sixteen of them end session dispatch altogether.
func TestHandshakeDownloadHasItsOwnDeadline(t *testing.T) {
	restore := handshakeCallTimeoutNanos.Load()
	handshakeCallTimeoutNanos.Store(int64(100 * time.Millisecond))
	defer handshakeCallTimeoutNanos.Store(restore)

	l := &Listener{
		Store:       wedgingStore{Storage: newFakeStore()},
		SessionsDir: DefaultSessionsDir,
		ctx:         testContext(t),
	}

	done := make(chan struct{})
	go func() {
		_, _ = l.downloadHandshakeFile("sessions/abc/" + HelloFile)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck hello GET held a handshake slot past any budget")
	}
}

// The two budgets must exist and be bounded, whatever the tests shorten them
// to: a zero budget would refuse every file and a missing one hangs forever.
func TestListenerCallBudgetsAreBounded(t *testing.T) {
	if listTimeout() <= 0 {
		t.Errorf("the List budget is %v", listTimeout())
	}
	if handshakeTimeout() <= 0 {
		t.Errorf("the handshake budget is %v", handshakeTimeout())
	}
	if cleanupCallTimeout() <= 0 {
		t.Errorf("the cleanup budget is %v", cleanupCallTimeout())
	}
}
