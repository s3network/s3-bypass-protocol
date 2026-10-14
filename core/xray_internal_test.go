package core

import (
	"context"
	go_errors "errors"
	"sync"
	"testing"

	"github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/dns/localdns"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/stats"
)

// The instance context must be canceled on Close, otherwise everything holding
// it (dialers, workers, name servers) keeps running after shutdown.
func TestInstanceCloseCancelsContext(t *testing.T) {
	server, err := New(&Config{})
	common.Must(err)

	ctx := server.ctx

	common.Must(server.Close())

	select {
	case <-ctx.Done():
	default:
		t.Error("Close did not cancel the instance context")
	}
	if !go_errors.Is(server.ctx.Err(), context.Canceled) {
		t.Error("expected the instance context to be canceled, but it is", server.ctx.Err())
	}
}

// Close is also called from the signal handling and from reload paths, it must
// not panic or report an error when it is invoked more than once.
func TestInstanceCloseIsIdempotent(t *testing.T) {
	server, err := New(&Config{})
	common.Must(err)

	common.Must(server.Close())
	common.Must(server.Close())

	if server.IsRunning() {
		t.Error("a closed instance must not report itself as running")
	}
}

// IsRunning, GetFeature and AddFeature used to read instance state without the
// matching lock.
func TestInstanceStateAccessIsRaceFree(t *testing.T) {
	server, err := New(&Config{})
	common.Must(err)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					server.IsRunning()
					server.GetFeature(inbound.ManagerType())
					server.GetFeature(outbound.ManagerType())
					server.AddFeature(localdns.New())
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		common.Must(server.Close())
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					server.IsRunning()
					server.GetFeature(dns.ClientType())
				}
			}
		}()
	}

	close(stop)
	wg.Wait()
}

func TestInstanceStartCloseFeatures(t *testing.T) {
	server, err := New(&Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&policy.Config{System: &policy.SystemPolicy{
				Stats: &policy.SystemPolicy_Stats{},
			}}),
		},
	})
	common.Must(err)

	if server.GetFeature(dns.ClientType()) == nil {
		t.Error("expected a dns client to be registered")
	}
	if server.GetFeature(stats.ManagerType()) == nil {
		t.Error("expected a stats manager to be registered")
	}

	common.Must(server.Start())
	if !server.IsRunning() {
		t.Error("expected the instance to be running after Start")
	}

	common.Must(server.Close())
	if server.IsRunning() {
		t.Error("expected the instance not to be running after Close")
	}
}
