package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Relay is a TCP relay that can be told to stop moving bytes.
//
// The failure this exists to reproduce is specific: a backend that ACCEPTS the
// connection and then goes quiet. Closing the socket is not the same thing — the
// client sees a fast, obvious error and retries immediately. Staying silent is
// what pins a poll loop forever, and it is what the earlier gpucloud incident
// actually looked like: "timeout awaiting response headers", not "connection
// refused".
//
// So the freeze does not close anything. It stops the copies at the point where
// they would write, so the kernel receive buffers fill, the peer's TCP window
// closes, and the peer stalls exactly as it would against a server that had
// gone to sleep. Nothing on the wire says anything is wrong, which is the
// whole point.
type Relay struct {
	upstream string
	frozen   atomic.Bool
	stopped  atomic.Bool

	mu    sync.Mutex
	conns int
}

func NewRelay(upstream string) *Relay { return &Relay{upstream: upstream} }

// Frozen reports whether relaying is currently suspended.
func (r *Relay) Frozen() bool { return r.frozen.Load() }

// SetFrozen suspends or resumes relaying.
func (r *Relay) SetFrozen(v bool) { r.frozen.Store(v) }

// Stop ends the run: every copy loop leaves, including one parked in a freeze.
//
// A frozen copy spins in waitThawed; without this it keeps spinning after the
// instrument has been told to stop, holding its sockets open and skewing every
// number the soak that reads this log goes on to produce.
func (r *Relay) Stop() { r.stopped.Store(true) }

// Watch polls for the appearance and disappearance of a control file and flips
// the freeze accordingly.
//
// Polling rather than fsnotify on purpose: this is a test instrument, it runs
// for minutes, and a 50ms tick is far below the resolution of anything it is
// being used to measure. A missed notification here would silently invalidate
// a run; a 50ms late one cannot.
func (r *Relay) Watch(controlPath string, stop <-chan struct{}) {
	defer r.Stop() // whatever ends the watch ends the relay with it

	was := r.Frozen()
	r.SetFrozen(fileExists(controlPath))
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			now := fileExists(controlPath)
			if now != was {
				was = now
				r.SetFrozen(now)
				if now {
					logEvent("FREEZE — relaying suspended, sockets left open")
				} else {
					logEvent("THAW — relaying resumed")
				}
			}
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// copy pumps src→dst, checking the freeze gate immediately BEFORE writing.
//
// The check has to sit before the write, not before the read: a read that is
// already parked in the kernel returns the moment data arrives, and a gate
// checked only on entry would let that first burst straight through. Holding
// the data instead is what fills the receive buffer and stalls the peer.
func (r *Relay) copy(dst io.Writer, src io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		if r.stopped.Load() {
			return
		}
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		if !r.waitThawed() {
			return
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

// waitThawed blocks while frozen and reports whether relaying may continue.
// A false return means the relay is shutting down.
//
// It checks the stop flag on every pass. The sleep is what made it deaf: a
// frozen copy spun here for as long as the freeze lasted and past the end of
// the run, holding both of the connection's copy goroutines and its sockets.
func (r *Relay) waitThawed() bool {
	for r.Frozen() {
		if r.stopped.Load() {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !r.stopped.Load()
}

func (r *Relay) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go r.handle(conn)
	}
}

// closeWriter is implemented by *net.TCPConn: shutting down one direction only.
// A full Close() here would tear down the copy that is still mid-stream.
type closeWriter interface {
	CloseWrite() error
}

// halfClose ends this direction and lets the peer see the EOF, which is how a
// finished request/response is signalled without killing the other direction.
func halfClose(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

// halfCloseGrace is how long the still-live direction is given after the other
// one has ended and its half-close has been sent.
const halfCloseGrace = 30 * time.Second

// handle bridges one client connection to the upstream.
//
// Both copies are awaited and each one half-closes its direction when it ends.
// The previous version returned on the FIRST copy, which is right for teardown
// but wrong for the protocol: after the upstream finished sending, the client's
// half-close never arrived, because the surviving goroutine kept reading from a
// connection nobody was serving any more — and the relay logged the connection
// closed while a goroutine still held it.
func (r *Relay) handle(conn net.Conn) {
	defer conn.Close()

	up, err := net.DialTimeout("tcp", r.upstream, 10*time.Second)
	if err != nil {
		logEvent("upstream dial failed: %v", err)
		return
	}
	defer up.Close()

	r.mu.Lock()
	r.conns++
	n := r.conns
	r.mu.Unlock()
	logEvent("conn #%d open (%s -> %s)", n, conn.RemoteAddr(), r.upstream)

	done := make(chan struct{}, 2)
	go func() { r.copy(up, conn); halfClose(up); done <- struct{}{} }()
	go func() { r.copy(conn, up); halfClose(conn); done <- struct{}{} }()
	<-done

	// The other direction gets as long as a peer needs to see our half-close
	// and answer it with its own. Past that it is not coming, and the deferred
	// Close() tears both ends down.
	select {
	case <-done:
	case <-time.After(halfCloseGrace):
		logEvent("conn #%d: second direction did not finish within %v", n, halfCloseGrace)
	}

	r.mu.Lock()
	r.conns--
	r.mu.Unlock()
	logEvent("conn #%d closed (%d still open)", n, r.conns)
}

var logMu sync.Mutex

// logEvent writes a timestamped line in the same shape the tunnel logs use, so
// the soak's correlation reads the relay's FREEZE and THAW as first-class
// events sitting next to the stalls they caused.
func logEvent(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	w := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(w, "%s [faultrelay] %s\n",
		time.Now().Format("2006/01/02 15:04:05.000000"), fmt.Sprintf(format, args...))
	w.Flush()
}
