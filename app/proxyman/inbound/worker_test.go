package inbound

import (
	"context"
	"sync"
	"testing"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/signal/done"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/pipe"
)

type stubInbound struct {
	closed int
}

func (s *stubInbound) Network() []net.Network { return nil }

func (s *stubInbound) Process(context.Context, net.Network, stat.Connection, routing.Dispatcher) error {
	return nil
}

func (s *stubInbound) Start() error { return nil }

func (s *stubInbound) Close() error {
	s.closed++
	return nil
}

// When Start() failed there is no hub, but the proxy still has to be closed.
func TestTCPWorkerCloseWithoutHub(t *testing.T) {
	proxy := &stubInbound{}
	w := &tcpWorker{proxy: proxy}

	if err := w.Close(); err != nil {
		t.Fatal("failed to close worker:", err)
	}
	if proxy.closed != 1 {
		t.Error("expected the proxy to be closed, but it was closed", proxy.closed, "times")
	}
}

// Live UDP sessions must not outlive the worker, their proxy.Process()
// goroutines would stay blocked on the connection pipe otherwise.
func TestUDPWorkerCloseClosesActiveConns(t *testing.T) {
	proxy := &stubInbound{}
	w := &udpWorker{proxy: proxy, activeConn: make(map[connID]*udpConn, 2)}

	conns := make([]*udpConn, 0, 2)
	for i := 0; i < 2; i++ {
		_, writer := pipe.New()
		conn := &udpConn{done: done.New(), writer: writer}
		w.activeConn[connID{src: net.UDPDestination(net.LocalHostIP, net.Port(10000+i))}] = conn
		conns = append(conns, conn)
	}

	if err := w.Close(); err != nil {
		t.Fatal("failed to close worker:", err)
	}

	for i, conn := range conns {
		if !conn.done.Done() {
			t.Error("active connection", i, "was not closed")
		}
	}
	if len(w.activeConn) != 0 {
		t.Error("expected the connection map to be empty, but it still holds", len(w.activeConn), "entries")
	}
	if proxy.closed != 1 {
		t.Error("expected the proxy to be closed, but it was closed", proxy.closed, "times")
	}
}

// udpConn.cancel is published under the worker lock and read by Close() from
// the cleanup path, so both must not run unguarded.
func TestUDPConnCancelIsRaceFree(t *testing.T) {
	w := &udpWorker{activeConn: make(map[connID]*udpConn, 8)}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		_, writer := pipe.New()
		conn := &udpConn{done: done.New(), writer: writer}
		w.activeConn[connID{src: net.UDPDestination(net.LocalHostIP, net.Port(10000+i))}] = conn

		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cancel := context.WithCancel(context.Background())
			w.Lock()
			conn.cancel = cancel
			w.Unlock()
			conn.Close()
		}()
	}
	wg.Wait()
}

// setInactive is called from the connection goroutine and from the cleaner.
func TestUDPConnInactive(t *testing.T) {
	_, writer := pipe.New()
	conn := &udpConn{done: done.New(), writer: writer}

	if conn.inactive.Load() {
		t.Error("a fresh connection must not be inactive")
	}

	conn.setInactive()
	if !conn.inactive.Load() {
		t.Error("expected the connection to be inactive")
	}
}
