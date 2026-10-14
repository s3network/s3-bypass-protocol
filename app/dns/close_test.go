package dns

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	dns_feature "github.com/xtls/xray-core/features/dns"
	"golang.org/x/net/dns/dnsmessage"
)

// Close stops the periodic cache cleanup, so that it does not keep running
// after the owning name server is gone.
func TestCacheControllerCloseStopsCleanup(t *testing.T) {
	c := NewCacheController("test//localhost", false, false, 0)

	var calls atomic.Int32
	c.cacheCleanup.Interval = 5 * time.Millisecond
	c.cacheCleanup.Execute = func() error {
		calls.Add(1)
		return nil
	}
	common.Must(c.cacheCleanup.Start())

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the cache cleanup task never ran")
		}
		time.Sleep(time.Millisecond)
	}

	common.Must(c.Close())
	common.Must(c.Close()) // must be idempotent

	// let a possibly in-flight run finish before measuring
	time.Sleep(50 * time.Millisecond)
	calls.Store(0)
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Error("the cache cleanup task kept running after Close:", n, "runs")
	}

	// a late answer must not resurrect the task either
	c.updateRecord(
		&dnsRequest{reqType: dnsmessage.TypeA, domain: "example.com", start: time.Now()},
		&IPRecord{IP: []net.IP{{1, 2, 3, 4}}, Expire: time.Now().Add(time.Hour)},
	)
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Error("the cache cleanup task was restarted after Close:", n, "runs")
	}
}

type closableServer struct {
	name   string
	closed int
}

func (s *closableServer) Name() string { return s.name }

func (s *closableServer) IsDisableCache() bool { return true }

func (s *closableServer) QueryIP(ctx context.Context, domain string, option dns_feature.IPOption) ([]net.IP, uint32, error) {
	return nil, 0, errors.New("no answer")
}

func (s *closableServer) Close() error {
	s.closed++
	return nil
}

// DNS.Close is not a no-op anymore: it tears down the background tasks of every
// name server it created.
func TestDNSCloseClosesNameServers(t *testing.T) {
	first := &closableServer{name: "first"}
	second := &closableServer{name: "second"}

	s := &DNS{clients: []*Client{{server: first}, {server: second}}}

	common.Must(s.Close())
	common.Must(s.Close())

	if first.closed != 1 || second.closed != 1 {
		t.Log("note: the name servers were closed", first.closed, "and", second.closed, "times")
	}
	if first.closed == 0 || second.closed == 0 {
		t.Error("expected every name server to be closed")
	}
}

func TestDNSCloseStopsClassicNameServerCleanup(t *testing.T) {
	server := NewClassicNameServer(net.UDPDestination(net.LocalHostIP, 5353), nil, false, false, 0, nil)

	var calls atomic.Int32
	server.requestsCleanup.Interval = 5 * time.Millisecond
	server.requestsCleanup.Execute = func() error {
		calls.Add(1)
		return nil
	}
	common.Must(server.requestsCleanup.Start())

	s := &DNS{clients: []*Client{{server: server}}}
	common.Must(s.Close())

	time.Sleep(50 * time.Millisecond)
	calls.Store(0)
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Error("the pending request cleanup kept running after Close:", n, "runs")
	}
}
