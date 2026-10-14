package fragment

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// countConn records every Write so the tests can assert how much data actually
// reached the wire, and can be bounded so a runaway split loop fails fast
// instead of hanging the suite.
type countConn struct {
	net.Conn
	written int
	calls   int
	maxCall int
}

func (c *countConn) Write(p []byte) (int, error) {
	c.calls++
	if c.calls > c.maxCall {
		c.maxCall = c.calls
		return 0, errTooManyWrites
	}
	c.written += len(p)
	return len(p), nil
}

type tooManyWritesError struct{}

func (tooManyWritesError) Error() string { return "too many writes: split loop did not terminate" }

var errTooManyWrites = tooManyWritesError{}

func newCountConn(maxCall int) *countConn {
	return &countConn{maxCall: maxCall}
}

// tlsHello builds a TLS ClientHello record with the given body length, which is
// the shape the fragmenter special-cases in its first branch.
func tlsHello(bodyLen int) []byte {
	body := make([]byte, bodyLen)
	for i := range body {
		body[i] = byte(i)
	}
	rec := []byte{0x16, 0x03, 0x01, byte(bodyLen >> 8), byte(bodyLen)}
	return append(rec, body...)
}

func writeWithTimeout(t *testing.T, c net.Conn, p []byte) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		n, err := c.Write(p)
		if err == nil && n != len(p) {
			err = errTooManyWrites
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Write did not return within 10s: the split loop is not advancing")
		return nil
	}
}

// A configured minimum segment length of 0 must not make the splitter emit
// zero-length records forever. crypto.RandBetween can return the lower bound,
// so before the fix "to" could equal "from" and the loop never terminated.
func TestWriteTerminatesWithZeroLengthMinimum(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *Config
	}{
		{
			name: "tls-hello-branch",
			cfg: &Config{
				PacketsFrom: 0, PacketsTo: 1,
				// RandBetween(0, 1) has to-from == 1 and deterministically
				// returns the lower bound, so "to" always equalled "from".
				LengthsMin: []int64{0}, LengthsMax: []int64{1},
				DelaysMin: []int64{0}, DelaysMax: []int64{0},
			},
		},
		{
			name: "plain-split-branch",
			cfg: &Config{
				PacketsFrom: 1, PacketsTo: 0,
				LengthsMin: []int64{0}, LengthsMax: []int64{1},
				DelaysMin: []int64{0}, DelaysMax: []int64{0},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A body far larger than the call limit: if the loop never
			// advances, this trips the guard instead of running forever.
			payload := tlsHello(4096)
			raw := newCountConn(20000)
			conn, err := NewConnClient(tc.cfg, raw, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeWithTimeout(t, conn, payload); err != nil {
				t.Fatalf("Write: %v (after %d calls, %d bytes)", err, raw.calls, raw.written)
			}
			if raw.written == 0 {
				t.Fatal("nothing was written")
			}
		})
	}
}

// An empty LengthsMin slice used to index at -1.
func TestEmptyLengthsDoesNotPanic(t *testing.T) {
	cfg := &Config{
		PacketsFrom: 1, PacketsTo: 0,
		LengthsMin: nil, LengthsMax: nil,
		DelaysMin: []int64{0}, DelaysMax: []int64{0},
	}
	raw := newCountConn(20000)
	conn, err := NewConnClient(cfg, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x41}, 512)
	if err := writeWithTimeout(t, conn, payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if raw.written != len(payload) {
		t.Fatalf("wrote %d bytes, want %d", raw.written, len(payload))
	}
}
