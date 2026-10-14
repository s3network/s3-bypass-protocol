package dns

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	dns_feature "github.com/xtls/xray-core/features/dns"
)

// A name server that never answers must not keep parallelQuery waiting
// forever. With the cache disabled there is no cached answer to fall back on,
// so the query has to carry a deadline of its own.
func TestParallelQueryTimesOutWithDisabledCache(t *testing.T) {
	queries := make(chan struct{}, 8)

	dnsServer := &DNS{
		ctx:                 context.Background(),
		enableParallelQuery: true,
		clients: []*Client{{
			server:    &neverAnswerServer{queried: queries},
			timeoutMs: 300 * time.Millisecond,
			ipOption:  &dns_feature.IPOption{IPv4Enable: true, IPv6Enable: true},
			tag:       "never",
		}},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		dnsServer.parallelQuery("example.com", dns_feature.IPOption{IPv4Enable: true, IPv6Enable: true})
	}()

	select {
	case <-queries:
	case <-time.After(5 * time.Second):
		t.Fatal("the query never reached the name server")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("parallelQuery did not return: the query got no deadline")
	}
}

// neverAnswerServer blocks until its context is done, like a server that
// silently drops the query.
type neverAnswerServer struct {
	queried chan<- struct{}
}

func (s *neverAnswerServer) Name() string { return "never" }

func (s *neverAnswerServer) IsDisableCache() bool { return true }

func (s *neverAnswerServer) QueryIP(ctx context.Context, domain string, option dns_feature.IPOption) ([]net.IP, uint32, error) {
	select {
	case s.queried <- struct{}{}:
	default:
	}

	<-ctx.Done()
	return nil, 0, ctx.Err()
}
