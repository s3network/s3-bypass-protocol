package shadowsocks_2022

import (
	"context"
	gonet "net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/pipe"
)

// pipeDialer hands back a pre-made connection instead of touching the network.
type pipeDialer struct {
	conn net.Conn
}

func (d *pipeDialer) Dial(context.Context, net.Destination) (stat.Connection, error) {
	return stat.Connection(d.conn), nil
}

func (d *pipeDialer) DestIpAddress() net.IP { return net.IP{} }

func (d *pipeDialer) SetOutboundGateway(context.Context, *session.Outbound) {}

var _ internet.Dialer = (*pipeDialer)(nil)

// newTestOutbound builds a real outbound, so Process runs its normal path.
func newTestOutbound(t *testing.T) *Outbound {
	t.Helper()
	o, err := NewClient(context.Background(), &ClientConfig{
		Address: net.NewIPOrDomain(net.LocalHostIP),
		Port:    1080,
		Method:  "2022-blake3-aes-128-gcm",
		Key:     "8JCsPssfgS8tiRwiMlhARg==",
	})
	require.NoError(t, err)
	return o
}

func testProcessCtx() context.Context {
	ctx := session.ContextWithTimeoutOnly(context.Background(), true)
	return session.ContextWithOutbounds(ctx, []*session.Outbound{
		{Target: net.TCPDestination(net.LocalHostIP, 80)},
	})
}

// parkedActivityTimers counts the inactivity watchers still waiting on their
// context. Every one of them is a connection whose context was never released.
func parkedActivityTimers() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "signal.CancelAfterInactivity.func1")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// runProcess drives Process into singbridge.CopyConn and then lets it return,
// so the context it created has to be released on the way out. The caller
// closes the returned conns, so it can measure the state in between.
func runProcess(t *testing.T, o *Outbound) (client, server net.Conn) {
	t.Helper()

	client, server = gonet.Pipe()

	// The proxy server side has to consume whatever Process writes.
	go func() {
		scratch := make([]byte, 4096)
		for {
			if _, err := server.Read(scratch); err != nil {
				return
			}
		}
	}()

	reader, writer := pipe.New(pipe.WithSizeLimit(4096))
	link := &transport.Link{Reader: reader, Writer: writer}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = o.Process(testProcessCtx(), link, &pipeDialer{conn: client})
	}()

	// Let Process get into the copy loop, then tear the link down.
	time.Sleep(100 * time.Millisecond)
	reader.Interrupt()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Process did not return after the link was interrupted")
	}
	return client, server
}

// Process built a fresh context with a cancel func it discarded. The context
// therefore stayed alive for every dialed connection, and the inactivity timer
// built on top of it kept watching a context that could never complete, so the
// watchers piled up one per connection.
func TestProcessReleasesTimeoutOnlyContext(t *testing.T) {
	const runs = 10

	o := newTestOutbound(t)

	// Warm up any lazily started runtime goroutines.
	c, s := runProcess(t, o)
	_ = c.Close()
	_ = s.Close()

	for range runs {
		c, s := runProcess(t, o)
		parked := parkedActivityTimers()
		_ = c.Close()
		_ = s.Close()

		// A watcher that is still winding down is fine; one that never drains
		// is a leaked context, and there is one per connection.
		assert.LessOrEqual(t, parked, runs/2,
			"inactivity watchers accumulated instead of draining (%d parked after %d runs)", parked, runs)
	}
}

// The dial must fail fast rather than block when the server connection is dead.
func TestProcessReturnsOnDeadConnection(t *testing.T) {
	o := newTestOutbound(t)

	client, server := gonet.Pipe()
	require.NoError(t, server.Close())
	defer client.Close()

	reader, writer := pipe.New(pipe.WithSizeLimit(4096))
	link := &transport.Link{Reader: reader, Writer: writer}
	reader.Interrupt()

	done := make(chan error, 1)
	go func() {
		done <- o.Process(testProcessCtx(), link, &pipeDialer{conn: client})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Process blocked on a dead server connection")
	}
}
