package dns

import (
	"context"
	gonet "net"
	"net/url"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	dns_feature "github.com/xtls/xray-core/features/dns"
)

// A name server that accepts the query but never answers used to keep the
// query goroutine blocked in ReadFullFrom forever: cancelling the context was
// not enough to interrupt the read.
func TestTCPNameServerQueryIsNotStuckOnRead(t *testing.T) {
	listener, err := gonet.Listen("tcp", "127.0.0.1:0")
	common.Must(err)
	defer listener.Close()

	accepted := make(chan struct{}, 4)
	var held []gonet.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
			accepted <- struct{}{}
			// the connections stay open, they never write a response
		}
	}()

	u, err := url.Parse("tcp+local://" + listener.Addr().String())
	common.Must(err)
	s, err := NewTCPLocalNameServer(u, false, false, 0, net.IP(nil))
	common.Must(err)
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 2)
	s.sendQuery(ctx, errCh, "example.com.", dns_feature.IPOption{IPv4Enable: true, IPv6Enable: true})

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the query was never sent")
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected a read error, but got none")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the query goroutine is still blocked on reading the response")
	}
}
