package fedarisha

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fedstorage "github.com/xtls/xray-core/proxy/fedarisha/storage"
	fedtransport "github.com/xtls/xray-core/proxy/fedarisha/transport"
)

// blockingStore is a backend that never completes an upload until its context
// dies — the shape of a store that has stopped accepting handshakes. It is
// what a permanent S3 refusal looks like from inside the dialer.
type blockingStore struct {
	uploads atomic.Int32
	entered chan struct{}
	once    sync.Once
	release chan struct{}
}

func newBlockingStore() *blockingStore {
	return &blockingStore{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (s *blockingStore) Init(context.Context) error              { return nil }
func (s *blockingStore) EnsureDir(context.Context, string) error { return nil }
func (s *blockingStore) List(context.Context, string, string) ([]fedstorage.FileInfo, error) {
	return nil, nil
}
func (s *blockingStore) Delete(context.Context, string) error { return nil }

func (s *blockingStore) Upload(ctx context.Context, _ string, _ []byte) error {
	s.uploads.Add(1)
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *blockingStore) Download(context.Context, string) ([]byte, error) {
	return nil, errors.New("not found")
}

func (s *blockingStore) Watch(context.Context, string, time.Time, time.Duration) ([]fedstorage.FileInfo, error) {
	return nil, nil
}

func newTestClient(ctx context.Context, store fedstorage.Storage) *Client {
	return &Client{
		ctx:    ctx,
		dialer: &fedtransport.Dialer{Store: store},
	}
}

func withShortDialTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	restore := dialTimeout
	dialTimeout = d
	t.Cleanup(func() { dialTimeout = restore })
}

// The outbound used to hold c.mu across the whole dial and dial with the
// client's own context. A store that refuses handshakes therefore parked the
// dial inside uploadRetrying — whose only exit is ctx.Err() — for the life of
// the client: every other caller queued behind the mutex, the error never came
// back, and the outbound silently served nothing. The dial has to end.
func TestDialEndsOnItsOwnDeadline(t *testing.T) {
	withShortDialTimeout(t, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newBlockingStore()
	c := newTestClient(ctx, store)

	start := time.Now()
	_, err := c.getSession()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a dial against a store that never accepts must fail")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the dial took %v: it is not bounded", elapsed)
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("the failure must name the deadline it hit, got %v", err)
	}
}

// Two callers, one handshake. Without single-flight the second one queued on
// the mutex and then dialled again, so a backend refusing connections saw one
// hello per concurrent connection instead of one per round.
func TestConcurrentGetSessionShareOneDial(t *testing.T) {
	withShortDialTimeout(t, 300*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newBlockingStore()
	c := newTestClient(ctx, store)

	const callers = 4
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.getSession()
		}(i)
	}

	// Wait until the winner is inside Upload, then let every other caller run.
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no dial ever started")
	}
	time.Sleep(100 * time.Millisecond)
	close(store.release)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d reported success against a store with no ACK", i)
		}
	}
	if n := store.uploads.Load(); n > 2 {
		t.Fatalf("the store saw %d hello uploads for %d concurrent callers: the dial is not single-flight", n, callers)
	}
}

// The mutex must not be held across the handshake: a second caller arriving
// during a dial has to reach the wait, not the lock.
func TestGetSessionDoesNotBlockOnTheMutexDuringADial(t *testing.T) {
	withShortDialTimeout(t, 300*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newBlockingStore()
	c := newTestClient(ctx, store)

	go func() { _, _ = c.getSession() }()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no dial ever started")
	}

	done := make(chan struct{})
	go func() {
		_, _ = c.getSession()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second caller never got past the dial")
	}
}
