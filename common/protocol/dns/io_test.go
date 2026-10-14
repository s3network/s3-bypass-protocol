package dns_test

import (
	"io"
	"runtime"
	"testing"

	"github.com/xtls/xray-core/common/buf"
	. "github.com/xtls/xray-core/common/protocol/dns"
	"github.com/xtls/xray-core/common/serial"
)

// A hostile DNS server can announce a message and then close the connection.
// The buffer of that message has to go back to the pool, otherwise every
// truncated message costs the process another buf.Size of garbage.
func TestTCPReaderTruncatedMessage(t *testing.T) {
	// Reference cost of the very same error path, with the message buffer
	// released by hand. ReadMessage must not be more expensive than that.
	reference := allocatedPerCall(func() {
		reader := &buf.BufferedReader{Reader: buf.NewReader(&truncatedSource{})}
		size, err := serial.ReadUint16(reader)
		if err != nil {
			return
		}
		message := buf.New()
		if _, err := message.ReadFullFrom(reader, int32(size)); err != nil {
			message.Release()
			return
		}
		message.Release()
	})

	truncated := allocatedPerCall(func() {
		reader := NewTCPReader(buf.NewReader(&truncatedSource{}))
		message, err := reader.ReadMessage()
		if err == nil {
			t.Errorf("expected an error for a truncated message, but got %v", message)
		}
	})

	if limit := reference + buf.Size/2; truncated > limit {
		t.Fatalf("truncated messages leak the message buffer: %.0f bytes per message, want at most %.0f", truncated, limit)
	}
}

func allocatedPerCall(fn func()) float64 {
	const rounds = 2000

	fn()
	runtime.GC()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < rounds; i++ {
		fn()
	}
	runtime.ReadMemStats(&after)

	return float64(after.TotalAlloc-before.TotalAlloc) / rounds
}

// truncatedSource delivers one message length prefix and then EOF.
type truncatedSource struct {
	sent bool
}

func (s *truncatedSource) Read(p []byte) (int, error) {
	if s.sent {
		return 0, io.EOF
	}
	s.sent = true
	return copy(p, []byte{0x00, 0x10}), nil
}
