package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- the freeze must be releasable --------------------------------------------

// The copy loop parks inside waitThawed for as long as the freeze lasts, and
// the sleep gave it no way to hear the stop signal: closing the control channel
// at the end of a run left the loop spinning with its sockets open. Closing the
// stop channel must end it.
func TestFrozenCopyReturnsWhenTheRelayStops(t *testing.T) {
	src, relayIn := net.Pipe()
	relayOut, sink := net.Pipe()
	defer src.Close()
	defer sink.Close()

	r := NewRelay("upstream:1")
	r.SetFrozen(true)

	done := make(chan struct{})
	go func() { r.copy(relayOut, relayIn); close(done) }()

	// Hand it a byte so it gets past the read and parks itself in the freeze.
	go func() { _, _ = src.Write([]byte("held")) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("a frozen copy must stay parked until thawed or stopped")
	default:
	}

	r.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a frozen copy did not return when the relay stopped")
	}
}

// Watch ending is the user-visible stop signal, so it has to stop the relay.
func TestWatchStopEndsTheRelay(t *testing.T) {
	dir := t.TempDir()
	control := filepath.Join(dir, "freeze")
	if err := os.WriteFile(control, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRelay("upstream:1")
	stop := make(chan struct{})
	go r.Watch(control, stop)

	// Wait until it has seen the control file.
	deadline := time.Now().Add(2 * time.Second)
	for !r.Frozen() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	close(stop)
	if !waitFor(func() bool { return r.stopped.Load() }, 2*time.Second) {
		t.Fatal("ending the watch must stop the relay; a frozen copy would spin forever")
	}
}

// --- one direction ending must not kill the other -----------------------------

// echoServer answers one line per request and half-closes its write side when
// the client is done, the way an HTTP/1.0 origin does.
func echoServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if line != "" {
						if _, werr := c.Write([]byte("echo:" + line)); werr != nil {
							return
						}
					}
					if err != nil {
						if cw, ok := c.(closeWriter); ok {
							_ = cw.CloseWrite()
						}
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// After the upstream half of a connection is done, the client must still be
// able to finish writing. The relay used to return on the FIRST copy to end,
// which tore the connection down while the other copy was still mid-stream.
func TestClientCanFinishWritingAfterUpstreamEnds(t *testing.T) {
	addr, stopServer := echoServer(t)
	defer stopServer()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	r := NewRelay(addr)
	go func() { _ = r.Serve(ln) }()

	client, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, err := client.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "echo:hello\n" {
		t.Fatalf("got %q", buf[:n])
	}

	// The second request must still make it: the direction that is still live
	// was never torn down.
	if _, err := client.Write([]byte("again\n")); err != nil {
		t.Fatalf("the relay closed the client direction too early: %v", err)
	}
	n, err = client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "echo:again\n" {
		t.Fatalf("got %q", buf[:n])
	}
}

// handle must not report the connection closed while a copy is still running.
func TestHandleWaitsForBothDirections(t *testing.T) {
	addr, stopServer := echoServer(t)
	defer stopServer()

	r := NewRelay(addr)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		r.handle(conn)
		ln.Close()
	}()

	client, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, err := client.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(buf); err != nil {
		t.Fatal(err)
	}

	// Half-close from the client: the client→upstream copy ends, the other runs
	// on until the server closes its side. handle must stay alive for it.
	if cw, ok := client.(closeWriter); ok {
		if err := cw.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}

	if !waitFor(func() bool { return r.connCount() == 0 }, 5*time.Second) {
		t.Fatal("the relay never finished the connection")
	}
}

func (r *Relay) connCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns
}

var _ io.Writer = (*net.TCPConn)(nil)
