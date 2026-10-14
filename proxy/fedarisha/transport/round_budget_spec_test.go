package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/proxy/fedarisha/storage"
)

// --- decompression bomb -------------------------------------------------------

// A frame is a flate stream: a few kilobytes of highly compressible input from
// the bucket expands to whatever the encoder chose. It used to be read with a
// bare io.ReadAll, so one object could allocate until the process died.
func TestDecodeRefusesAPayloadPastTheLimit(t *testing.T) {
	bomb := encodePayload(bytes.Repeat([]byte{0}, 8<<20)) // 8MB of zeros
	if len(bomb) > 64<<10 {
		t.Fatalf("the test payload should compress small, got %d bytes", len(bomb))
	}

	if _, err := decodePayload(bomb, 1<<20); err == nil {
		t.Fatal("a frame expanding past the limit must be refused, not read")
	}
}

func TestDecodeAcceptsAPayloadInsideTheLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("fedarisha"), 1024)
	frame := encodePayload(payload)

	out, err := decodePayload(frame, int64(len(payload))+1)
	if err != nil {
		t.Fatalf("a frame inside the limit must decode: %v", err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("decoded %d bytes, want the original %d", len(out), len(payload))
	}
}

func TestMaxDecodedBytesIsFlooredAtTheDefault(t *testing.T) {
	small := NewConn(ConnConfig{Store: newFakeStore(), SessionID: GenerateSessionID(), MaxFileSize: 1024})
	defer small.Close()
	if got := small.maxDecodedBytes(); got < DefaultMaxFileSize {
		t.Fatalf("a connection configured with a tiny maxFileSize must not refuse its peer's legal frames: %d", got)
	}

	big := NewConn(ConnConfig{Store: newFakeStore(), SessionID: GenerateSessionID(), MaxFileSize: 8 << 20})
	defer big.Close()
	if got := big.maxDecodedBytes(); got != 8<<20+1 {
		t.Fatalf("limit = %d, want the configured file size plus one", got)
	}
}

// --- the round budget ---------------------------------------------------------

// roundCountingStore records how many GETs were issued between two consecutive
// List calls, i.e. how much work one fetch round actually did.
type roundCountingStore struct {
	*fakeStore
	mu          sync.Mutex
	downloads   int
	lastMark    int
	maxPerRound int
}

func (s *roundCountingStore) Download(ctx context.Context, path string) ([]byte, error) {
	s.mu.Lock()
	s.downloads++
	s.mu.Unlock()
	return s.fakeStore.Download(ctx, path)
}

// List closes the previous round: everything downloaded since the call before
// this one was that round's work. The round's GETs happen AFTER its List
// returns, so the mark has to be taken on entry, not on exit.
func (s *roundCountingStore) List(ctx context.Context, dir, prefix string) ([]storage.FileInfo, error) {
	s.mu.Lock()
	if round := s.downloads - s.lastMark; round > s.maxPerRound {
		s.maxPerRound = round
	}
	s.lastMark = s.downloads
	s.mu.Unlock()
	return s.fakeStore.List(ctx, dir, prefix)
}

func (s *roundCountingStore) peakRound() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxPerRound
}

// maxReadBatch is a COUNT of files. At the default 2MB per file, 64 of them is
// 128MB taken in by one connection in one poll, with nothing pushing back when
// the consumer above is slow. The round has to stop at a byte ceiling.
func TestFetchRoundStopsAtItsByteBudget(t *testing.T) {
	restore := roundBytesBudget.Load()
	roundBytesBudget.Store(4096)
	defer roundBytesBudget.Store(restore)

	inner := newFakeStore()
	store := &roundCountingStore{fakeStore: inner}
	sessDir := "sessions/round-budget"
	c := NewConn(ConnConfig{Store: store, SessionDir: sessDir, SessionID: GenerateSessionID()})
	defer c.Close()

	// 8 incompressible files of 1KB: a count-only budget would take all eight
	// in one round, a 4KB budget four.
	const files, fileSize = 8, 1024
	plantSizedSeqs(t, c, inner, sessDir, files, fileSize)

	if !waitFor(func() bool { return store.peakRound() > 0 }, 10*time.Second) {
		t.Fatal("no round ever ran")
	}
	if peak := store.peakRound(); peak > files/2 {
		t.Fatalf("one round issued %d GETs for a 4KB budget over %d files of %d B: the byte budget did not bind",
			peak, files, fileSize)
	}
}

// The read buffer only drains when the application reads. A round that appends
// past its ceiling is how one slow consumer became 128MB of buffers — and, since
// several sessions share a process, everyone else's memory too.
func TestReadBufferStopsAtItsCeiling(t *testing.T) {
	restore := readBufBytesBudget.Load()
	readBufBytesBudget.Store(2048)
	defer readBufBytesBudget.Store(restore)

	store := newFakeStore()
	sessDir := "sessions/buf-ceiling"
	c := NewConn(ConnConfig{Store: store, SessionDir: sessDir, SessionID: GenerateSessionID()})
	defer c.Close()

	const files, fileSize = 8, 1024
	plantSizedSeqs(t, c, store, sessDir, files, fileSize)

	// Nothing ever reads from this connection.
	if !waitFor(func() bool { return readBufferLen(c) > 0 }, 10*time.Second) {
		t.Fatal("no data ever arrived")
	}
	time.Sleep(300 * time.Millisecond) // let several more rounds run

	if held := int64(readBufferLen(c)); held > readBufBytes() {
		t.Fatalf("read buffer holds %d bytes, over its %d ceiling", held, readBufBytes())
	}
	if readBufferLen(c) == 0 {
		t.Fatal("the ceiling was never reached, so the spec proved nothing")
	}
}

// A round stopped by backpressure is not a hole: tearing the session down for a
// consumer that fell behind kills a perfectly healthy transfer.
func TestBackpressureIsNotTreatedAsAHole(t *testing.T) {
	restoreBuf := readBufBytesBudget.Load()
	readBufBytesBudget.Store(2048)
	defer readBufBytesBudget.Store(restoreBuf)

	store := newFakeStore()
	sessDir := "sessions/backpressure"
	c := NewConn(ConnConfig{Store: store, SessionDir: sessDir, SessionID: GenerateSessionID()})
	defer c.Close()

	const files, fileSize = 8, 1024
	plantSizedSeqs(t, c, store, sessDir, files, fileSize)

	// A holeTimeout this short would close a session whose readSeq file is
	// genuinely missing. Nothing here is missing — the data is in readBuf
	// waiting for a reader that never comes.
	if !waitFor(func() bool { return readBufferLen(c) > 0 }, 10*time.Second) {
		t.Fatal("no data ever arrived")
	}
	time.Sleep(500 * time.Millisecond)

	if c.closed != nil {
		select {
		case <-c.closed:
			t.Fatal("a full read buffer was escalated into a hole and closed the session")
		default:
		}
	}
}

func readBufferLen(c *Conn) int {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	return c.readBuf.Len()
}

// plantSizedSeqs writes n read-direction files of a known size, so a spec can
// reason about bytes rather than about a file count.
//
// The bodies are incompressible on purpose: a file of repeated bytes deflates
// to almost nothing, the store would report that tiny size in its listing, and
// a byte budget measured against it would never bind.
func plantSizedSeqs(t *testing.T, c *Conn, store *fakeStore, sessDir string, n, size int) {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(size) * 31))
	body := make([]byte, size)
	if _, err := rng.Read(body); err != nil {
		t.Fatal(err)
	}
	for seq := 0; seq < n; seq++ {
		data, err := c.encodeTestFile(uint64(seq), body)
		if err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("%s/%s", sessDir, SeqFileName(c.readPrefix, uint64(seq)))
		if err := store.Upload(context.Background(), path, data); err != nil {
			t.Fatal(err)
		}
	}
}

// --- teardown reporting -------------------------------------------------------

// failingDeleteStore refuses every delete, the way a revoked PAK does.
type failingDeleteStore struct {
	storage.Storage
	failures int
	mu       sync.Mutex
}

func (f *failingDeleteStore) Delete(context.Context, string) error {
	f.mu.Lock()
	f.failures++
	f.mu.Unlock()
	return errors.New("AccessDenied")
}

// Every teardown failure used to be assigned to `_`, so "the sweep finished"
// and "the sweep gave up half-way" produced the same silence. The count has to
// be in the log.
func TestCleanupSessionReportsWhatItCouldNotDelete(t *testing.T) {
	inner := newFakeStore()
	sessDir := "sessions/cleanup-report"
	for i := 0; i < 3; i++ {
		if err := inner.Upload(context.Background(), fmt.Sprintf("%s/c_%d_0", sessDir, i), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	store := &failingDeleteStore{Storage: inner}

	c := NewConn(ConnConfig{Store: store, SessionDir: sessDir, SessionID: GenerateSessionID()})
	defer c.Close()

	var out syncBuffer
	restore := captureLog(&out)
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.cleanupSession(ctx)
	restore()
	logged := out.String()

	if !strings.Contains(logged, "stay in the bucket") {
		t.Fatalf("the sweep reported nothing about its failures:\n%s", logged)
	}
	if !strings.Contains(logged, sessDir) {
		t.Fatalf("the report must name the session it could not clean:\n%s", logged)
	}
}

// A single stuck call must not eat the whole teardown budget: each DELETE gets
// its own deadline, so the sweep carries on past it.
func TestCleanupGivesEachDeleteItsOwnDeadline(t *testing.T) {
	restore := cleanupCallBudget.Load()
	cleanupCallBudget.Store(int64(50 * time.Millisecond))
	defer cleanupCallBudget.Store(restore)

	slow := &slowDeleteStore{Storage: newFakeStore(), delay: time.Hour}
	sessDir := "sessions/slow-cleanup"
	if err := slow.Upload(context.Background(), sessDir+"/c_0_0", []byte("x")); err != nil {
		t.Fatal(err)
	}

	c := NewConn(ConnConfig{Store: slow, SessionDir: sessDir, SessionID: GenerateSessionID()})
	defer c.Close()

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.cleanupSession(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("one slow delete held the whole sweep past the teardown budget")
	}
}

// slowDeleteStore takes longer than any budget to answer, ignoring its context
// — the worst case a wedged backend can present.
type slowDeleteStore struct {
	storage.Storage
	delay time.Duration
}

func (s *slowDeleteStore) Delete(ctx context.Context, path string) error {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
