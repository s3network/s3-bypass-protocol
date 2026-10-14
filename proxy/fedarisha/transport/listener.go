package transport

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/proxy/fedarisha/storage"
)

// Listener watches for new client sessions and returns net.Conn for each.
//
// InboundTag is the user-visible identifier of the inbound this listener
// represents. It is stamped onto every accepted Conn so the routing and
// stats layers can attribute the stream to its source. Empty in standalone
// (non-runtime-config) mode.
type Listener struct {
	Store         storage.Storage
	SessionsDir   string // e.g. "sessions"
	InboundTag    string // matches RuntimeConfig.Inbounds[].Tag
	MultiUser     bool   // scan */sessions/ for per-user prefixes
	PollInterval  time.Duration
	WriteInterval time.Duration
	IdleTimeout   time.Duration
	MaxFileSize   int
	WebhookHub    *WebhookHub // optional — enables event-driven session detection

	// IsUserAllowed is the layer-2 entitlement gate. Even when a stale or
	// out-of-band PAK lets a client write to the bucket, we refuse to handshake
	// for prefixes that aren't registered as active users on this inbound.
	// nil means "no gate" — accept every prefix (single-user / standalone mode).
	IsUserAllowed func(userPrefix string) bool

	ctx    context.Context
	cancel context.CancelFunc

	incoming chan *Conn
	known    map[string]bool // key: "sessionsDir/sessID"
	knownMu  sync.Mutex

	// noHelloSince records when a session directory was first seen without a
	// hello, so that probing it can be given up on instead of repeated on
	// every poll tick until the process restarts.
	noHelloSince map[string]time.Time
	noHelloMu    sync.Mutex

	// accepting holds session directories with a handshake in flight. The
	// previous serialisation guaranteed one handshake per directory; running
	// them concurrently has to guarantee it explicitly, or a ticker and a
	// webhook arriving together hand the same session over twice.
	accepting map[string]bool

	// handshakeSem bounds concurrent handshakes.
	handshakeSem chan struct{}

	closeOnce sync.Once
	addr      net.Addr
}

// ListenOpts holds optional parameters for Listen/ListenMultiUser.
type ListenOpts struct {
	WebhookHub    *WebhookHub
	InboundTag    string // stamped onto every accepted Conn
	PollInterval  time.Duration
	WriteInterval time.Duration
	IdleTimeout   time.Duration
	MaxFileSize   int
	IsUserAllowed func(userPrefix string) bool
}

const DefaultSessionsDir = "sessions"

// staleSessionProbeWindow is how long a session directory that has never shown
// a hello keeps being probed before the listener gives up on it.
//
// A directory with no hello is normal for the first moments of a connection —
// the client creates it and writes the hello a moment later — so this has to be
// comfortably longer than that. It also has to be short enough that a client
// which died mid-handshake stops costing requests within a minute, because
// until it does, every poll tick re-reads a file that can only answer 404.
var staleSessionProbeWindow = 60 * time.Second

// Per-call budgets for the listener's own storage calls.
//
// Every one of these used l.ctx — the context of the whole listener — with no
// deadline of its own. The watchLoop goroutine runs scanForNewSessions on
// itself, and that List is the gateway to every user's sessions: one backend
// that accepts the request and then says nothing parks that goroutine
// indefinitely, and session discovery stops for EVERY user on the inbound with
// nothing in the log. A per-call deadline turns "the loop is stuck" into "this
// one list failed", which the poll retries and the operator can see.
//
// Vars, shortened by the specs that measure them. Atomics for the same reason
// listTimeoutNanos is one: the values are read on goroutines the shortening
// test does not own. Zero means "use the default".
var (
	listCallTimeoutNanos      atomic.Int64
	handshakeCallTimeoutNanos atomic.Int64
)

const (
	defaultListCallTimeout      = 4 * time.Second
	defaultHandshakeCallTimeout = 10 * time.Second
)

func listTimeout() time.Duration {
	if v := listCallTimeoutNanos.Load(); v > 0 {
		return time.Duration(v)
	}
	return defaultListCallTimeout
}

func handshakeTimeout() time.Duration {
	if v := handshakeCallTimeoutNanos.Load(); v > 0 {
		return time.Duration(v)
	}
	return defaultHandshakeCallTimeout
}

func effectiveSessionsDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return DefaultSessionsDir
	}
	return dir
}

// Listen starts watching the sessions directory for new client connections.
func Listen(ctx context.Context, store storage.Storage, sessionsDir string, opts ...ListenOpts) (*Listener, error) {
	sessionsDir = effectiveSessionsDir(sessionsDir)
	if err := store.EnsureDir(ctx, sessionsDir); err != nil {
		return nil, fmt.Errorf("fedarisha listen: ensure sessions dir: %w", err)
	}

	lCtx, cancel := context.WithCancel(ctx)
	l := &Listener{
		Store:        store,
		SessionsDir:  sessionsDir,
		ctx:          lCtx,
		cancel:       cancel,
		incoming:     make(chan *Conn, 16),
		known:        make(map[string]bool),
		noHelloSince: make(map[string]time.Time),
		accepting:    make(map[string]bool),
		handshakeSem: make(chan struct{}, handshakeConcurrency),
		addr:         fedarishaAddr{tag: "fedarisha-listener:" + sessionsDir},
	}
	l.applyOpts(opts)

	go l.watchLoop()
	return l, nil
}

// ListenMultiUser starts watching for sessions across all user prefixes.
// It scans */sessionsDir/ for new sessions, where each user has their own
// subdirectory under the storage root.
func ListenMultiUser(ctx context.Context, store storage.Storage, sessionsDir string, opts ...ListenOpts) (*Listener, error) {
	sessionsDir = effectiveSessionsDir(sessionsDir)
	lCtx, cancel := context.WithCancel(ctx)
	l := &Listener{
		Store:        store,
		SessionsDir:  sessionsDir,
		MultiUser:    true,
		ctx:          lCtx,
		cancel:       cancel,
		incoming:     make(chan *Conn, 16),
		known:        make(map[string]bool),
		noHelloSince: make(map[string]time.Time),
		accepting:    make(map[string]bool),
		handshakeSem: make(chan struct{}, handshakeConcurrency),
		addr:         fedarishaAddr{tag: "fedarisha-listener:*/" + sessionsDir},
	}
	l.applyOpts(opts)

	go l.watchLoop()
	return l, nil
}

func (l *Listener) applyOpts(opts []ListenOpts) {
	if len(opts) == 0 {
		return
	}
	opt := opts[0]
	l.WebhookHub = opt.WebhookHub
	l.InboundTag = opt.InboundTag
	l.PollInterval = opt.PollInterval
	l.WriteInterval = opt.WriteInterval
	l.IdleTimeout = opt.IdleTimeout
	l.MaxFileSize = opt.MaxFileSize
	l.IsUserAllowed = opt.IsUserAllowed
}

// Accept blocks until a new client session is detected or the listener is closed.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.incoming:
		return conn, nil
	case <-l.ctx.Done():
		return nil, l.ctx.Err()
	}
}

func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
	})
	return nil
}

func (l *Listener) Addr() net.Addr { return l.addr }

// ---------- internal ----------

func (l *Listener) watchLoop() {
	poll := l.PollInterval
	if poll == 0 {
		if l.WebhookHub != nil {
			poll = 10 * time.Second
		} else {
			poll = 500 * time.Millisecond // Sessions are rare events.
		}
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var webhookCh <-chan string
	if l.WebhookHub != nil {
		webhookCh = l.WebhookHub.NewSessions()
	}

	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			l.scanForNewSessions()
		case sessDir := <-webhookCh:
			l.dispatchHandshake(sessDir)
		}
	}
}

// handshakeConcurrency bounds how many handshakes may be in flight. Without it,
// a burst of new sessions would spawn one goroutine each; with it, a store that
// stops accepting writes stalls at most this many and the rest queue — which
// still discovers them, just not all at once.
const handshakeConcurrency = 16

// dispatchHandshake runs one handshake off the watch loop.
//
// The handshake performs network I/O — a GET, a DELETE, a key exchange and a
// retried PUT — and doing that on the loop's goroutine meant a store that
// rejected a 32-byte ACK stopped the server noticing a new session for every
// user, indefinitely, with only a few retry lines in the log.
func (l *Listener) dispatchHandshake(sessDir string) {
	select {
	case l.handshakeSem <- struct{}{}:
	default:
		// Every slot is busy. Dropping the notification is safe: the ticker
		// re-scans, and a directory with a hello in it is not forgotten.
		return
	}
	go func() {
		defer func() { <-l.handshakeSem }()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[fedarisha-server] handshake for %s panicked: %v", shortID(sessDir), r)
			}
		}()
		l.acceptSession(sessDir)
	}()
}

func (l *Listener) scanForNewSessions() {
	if !l.MultiUser {
		l.scanSessionsIn(l.SessionsDir)
		return
	}

	// Multi-user: list user directories, then scan {user}/sessions/ for each.
	listCtx, cancel := context.WithTimeout(l.ctx, listTimeout())
	users, err := l.Store.List(listCtx, "", "")
	cancel()
	if err != nil {
		log.Printf("[fedarisha-server] list users error: %v", err)
		return
	}
	for _, u := range users {
		if !u.IsDir {
			continue
		}
		l.scanSessionsIn(u.Name + "/" + l.SessionsDir)
	}
}

func (l *Listener) scanSessionsIn(sessionsDir string) {
	dirs, err := l.listDir(sessionsDir)
	if err != nil {
		return // Directory may not exist yet.
	}

	// Reclaim bookkeeping for directories that are no longer there.
	//
	// known and noHelloSince exist only to stop a directory being
	// re-probed, which is worth nothing once the directory is gone — after
	// cleanupSession wiped it, or the lifecycle rule swept it. Without this
	// both maps grew for the life of the process.
	l.pruneMissing(sessionsDir, dirs)

	for _, d := range dirs {
		if !d.IsDir {
			continue
		}
		sessDir := sessionsDir + "/" + d.Name
		l.dispatchHandshake(sessDir)
	}
}

// pruneMissing drops entries for directories under sessionsDir that the last
// listing did not return.
//
// Scoped to sessionsDir on purpose: in multi-user mode each user is scanned
// separately, and one user's scan must never conclude that another user's
// directory has gone — that would drop live sessions.
func (l *Listener) pruneMissing(sessionsDir string, dirs []storage.FileInfo) {
	live := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		if d.IsDir {
			live[d.Name] = struct{}{}
		}
	}
	prefix := sessionsDir + "/"

	l.knownMu.Lock()
	for sessDir := range l.known {
		if name, ok := strings.CutPrefix(sessDir, prefix); ok {
			if _, keep := live[name]; !keep {
				delete(l.known, sessDir)
			}
		}
	}
	l.knownMu.Unlock()

	l.noHelloMu.Lock()
	for sessDir := range l.noHelloSince {
		if name, ok := strings.CutPrefix(sessDir, prefix); ok {
			if _, keep := live[name]; !keep {
				delete(l.noHelloSince, sessDir)
			}
		}
	}
	l.noHelloMu.Unlock()
}

// acceptSessionFromWebhook handles a webhook notification about a new hello file.
// sessDir is the relative path like "sessions/abc123" or "user1/sessions/abc123".
func (l *Listener) acceptSessionFromWebhook(sessDir string) {
	log.Printf("[fedarisha-server] webhook: new session in %s", sessDir)
	l.acceptSession(sessDir)
}

// staleFor reports whether a session directory has gone long enough without a
// hello that the listener should stop waiting for one. The first miss only
// starts the clock; it never retires a directory on its own, because the
// server can see the directory before the client's hello lands.
func (l *Listener) staleFor(sessDir string) bool {
	l.noHelloMu.Lock()
	first, seen := l.noHelloSince[sessDir]
	if !seen {
		l.noHelloSince[sessDir] = time.Now()
		l.noHelloMu.Unlock()
		return false
	}
	l.noHelloMu.Unlock()

	return time.Since(first) > staleSessionProbeWindow
}

func (l *Listener) forgetNoHello(sessDir string) {
	l.noHelloMu.Lock()
	delete(l.noHelloSince, sessDir)
	l.noHelloMu.Unlock()
}

// downloadHandshakeFile GETs one handshake file under its own deadline, so a
// store that accepts the request and then goes quiet cannot park the handshake
// goroutine (and its slot in the handshake semaphore) forever.
func (l *Listener) downloadHandshakeFile(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(l.ctx, handshakeTimeout())
	defer cancel()
	return l.Store.Download(ctx, path)
}

// deleteSessionFile removes one session file under its own deadline. Its
// failures are reported: a hello left behind is re-probed on every tick.
func (l *Listener) deleteSessionFile(path string) {
	ctx, cancel := context.WithTimeout(l.ctx, handshakeTimeout())
	defer cancel()
	if err := l.Store.Delete(ctx, path); err != nil {
		log.Printf("[fedarisha-server] delete of %s failed: %v", shortID(path), err)
	}
}

// listDir lists one directory under the listener's own deadline.
//
// Called on the watch loop, which is shared by every user's discovery: a List
// with no budget is a single slow backend stopping session detection for the
// whole inbound.
func (l *Listener) listDir(dir string) ([]storage.FileInfo, error) {
	ctx, cancel := context.WithTimeout(l.ctx, listTimeout())
	defer cancel()
	return l.Store.List(ctx, dir, "")
}

// acceptSession performs the key exchange handshake for a single session directory
// and enqueues the resulting connection.
func (l *Listener) acceptSession(sessDir string) {
	parts := strings.Split(sessDir, "/")
	sessID := parts[len(parts)-1]

	l.knownMu.Lock()
	if l.known[sessDir] || l.accepting[sessDir] {
		l.knownMu.Unlock()
		return
	}
	// Claim the directory for the duration of the handshake. Handshakes run
	// concurrently now, and the serialisation that used to guarantee one per
	// directory has to be stated explicitly — otherwise a tick and a webhook
	// arriving together hand the same session over twice.
	l.accepting[sessDir] = true
	l.knownMu.Unlock()
	defer func() {
		l.knownMu.Lock()
		delete(l.accepting, sessDir)
		l.knownMu.Unlock()
	}()

	// In multi-user mode the prefix is the first path segment.
	// sessDir format: "user1/sessions/abc123" → userPrefix = "user1"
	var userPrefix string
	if l.MultiUser {
		if idx := strings.Index(sessDir, "/"); idx > 0 {
			userPrefix = sessDir[:idx]
		}
	}

	// Layer-2 gate: refuse handshakes for prefixes that aren't currently
	// entitled. PAK revocation (layer 1) prevents most rogue writes, but a
	// race window between AddUser/RemoveUser and PAK provisioning leaves a
	// gap that the gate closes deterministically. We mark the session known
	// and delete the hello so we don't keep poking it on every poll tick.
	if l.IsUserAllowed != nil && userPrefix != "" && !l.IsUserAllowed(userPrefix) {
		l.knownMu.Lock()
		l.known[sessDir] = true
		l.knownMu.Unlock()
		l.deleteSessionFile(sessDir + "/" + HelloFile)
		log.Printf("[fedarisha-server] session %s rejected: user %q not allowed", shortID(sessID), userPrefix)
		return
	}

	// Check for hello file (contains sessID + client public key).
	helloPath := sessDir + "/" + HelloFile
	data, err := l.downloadHandshakeFile(helloPath)
	if err != nil || len(data) == 0 {
		// No hello yet. This is normal for the first moments of a connection,
		// but it is not a state worth re-probing forever: an abandoned
		// directory used to cost one guaranteed-404 GET on every poll tick,
		// and with the server's default 100ms interval a handful of them
		// saturates the read pool that the next real handshake needs.
		if l.staleFor(sessDir) {
			l.forgetNoHello(sessDir)
			l.knownMu.Lock()
			l.known[sessDir] = true
			l.knownMu.Unlock()
		}
		return
	}

	l.forgetNoHello(sessDir)

	// Extract client public key from hello (after sessID).
	if len(data) < len(sessID)+32 {
		log.Printf("[fedarisha-server] session %s: hello too short for key exchange", shortID(sessID))
		return
	}
	clientPub := data[len(sessID):][:32]

	log.Printf("[fedarisha-server] new session %s in %s", shortID(sessID), sessDir)

	l.knownMu.Lock()
	l.known[sessDir] = true
	l.knownMu.Unlock()

	l.deleteSessionFile(helloPath)

	// Generate server X25519 key pair and derive shared secret.
	privKey, pubKey, err := GenerateX25519()
	if err != nil {
		log.Printf("[fedarisha-server] session %s: keygen failed: %v", shortID(sessID), err)
		return
	}

	aead, err := DeriveAEAD(privKey, clientPub, sessID)
	if err != nil {
		log.Printf("[fedarisha-server] session %s: key derivation failed: %v", shortID(sessID), err)
		return
	}

	// Write ACK with server's public key.
	//
	// uploadRetrying retries until the store takes the file or its context
	// ends, and l.ctx ends with the listener. So a backend that refuses this
	// upload holds one of the 16 handshake slots for the life of the process,
	// and once sixteen such sessions pile up the 17th is never dispatched at
	// all — session detection quietly stops. The ACK gets the same budget the
	// client's dial gets.
	ackPath := sessDir + "/" + AckFile
	ackCtx, cancelAck := context.WithTimeout(l.ctx, DialBudget())
	err = uploadRetrying(ackCtx, l.Store, ackPath, pubKey, "ack")
	cancelAck()
	if err != nil {
		log.Printf("[fedarisha-server] failed to ACK session %s: %v", shortID(sessID), err)
		return
	}

	conn := NewConn(ConnConfig{
		Store:         l.Store,
		SessionID:     sessID,
		SessionDir:    sessDir,
		UserPrefix:    userPrefix,
		InboundTag:    l.InboundTag,
		IsClient:      false,
		PollInterval:  l.PollInterval,
		WriteInterval: l.WriteInterval,
		IdleTimeout:   l.IdleTimeout,
		MaxFileSize:   l.MaxFileSize,
		Cipher:        aead,
		WebhookHub:    l.WebhookHub,
	})

	select {
	case l.incoming <- conn:
	default:
		log.Printf("[fedarisha-server] incoming channel full, dropping session %s", shortID(sessID))
		conn.Close()
	}
}
