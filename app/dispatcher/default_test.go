package dispatcher_test

import (
	"context"
	"testing"

	. "github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport"
)

// An invalid destination must be reported as an error, like DispatchLink does,
// instead of bringing the whole process down with a panic.
func TestDispatchInvalidDestination(t *testing.T) {
	d := new(DefaultDispatcher)

	if _, err := d.Dispatch(context.Background(), net.Destination{}); err == nil {
		t.Error("Dispatch: expected an error for an invalid destination, but got nil")
	}

	if err := d.DispatchLink(context.Background(), net.Destination{}, &transport.Link{}); err == nil {
		t.Error("DispatchLink: expected an error for an invalid destination, but got nil")
	}
}
