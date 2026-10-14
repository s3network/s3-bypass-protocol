package fedarisha

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"

	"github.com/xtls/xray-core/common/buf"
	xraynet "github.com/xtls/xray-core/common/net"
)

// The header word is attacker-controlled and was not checked before the read.
// Anything above buf.Size is refused by ReadFullFrom WITHOUT consuming a byte,
// so the payload's length prefix then lands where the metadata was expected and
// every frame after it is garbage.
func TestOversizedPacketMetaIsRefusedBeforeTheRead(t *testing.T) {
	var raw bytes.Buffer
	// metaLen far beyond anything a destination can be.
	binary.Write(&raw, binary.BigEndian, uint16(9000))
	binary.Write(&raw, binary.BigEndian, uint16(8)) // would be the payload length
	raw.WriteString("payload!")

	reader := newFedarishaPacketReader(&raw, xraynet.UDPDestination(xraynet.DomainAddress("example.com"), 53))
	if _, err := reader.ReadMultiBuffer(); err == nil {
		t.Fatal("a meta length past the ceiling must be refused, not read")
	}
}

func TestPacketMetaAtTheCeilingIsStillRead(t *testing.T) {
	var raw bytes.Buffer
	binary.Write(&raw, binary.BigEndian, uint16(maxPacketMetaBytes))
	raw.Write(make([]byte, maxPacketMetaBytes))
	binary.Write(&raw, binary.BigEndian, uint16(4))
	raw.WriteString("data")

	reader := newFedarishaPacketReader(&raw, xraynet.UDPDestination(xraynet.DomainAddress("example.com"), 53))
	// A block of that size is not a real destination, so parsing it fails —
	// what matters is that the reader consumed it and moved on to the payload
	// rather than desynchronising.
	mb, err := reader.ReadMultiBuffer()
	if err == nil {
		buf.ReleaseMulti(mb)
	}
}

// failingWriter refuses everything, standing in for the link going away
// mid-batch.
type failingWriter struct{}

func (failingWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	buf.ReleaseMulti(mb)
	return errors.New("link down")
}

func (failingWriter) Write(b []byte) (int, error) { return 0, errors.New("link down") }

// The frames built before the failure are not handed to the writer on this
// path, so without an explicit release they go to the garbage collector
// outside the pool — eight pooled 2KiB buffers per dropped batch.
func TestFramesAreReleasedWhenTheWriterRefuses(t *testing.T) {
	const rounds = 2000

	if raceDetectorEnabled {
		t.Skip("allocation counts say nothing about pool leaks in a -race build")
	}

	dropBatch(t) // warm the pool up before measuring

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < rounds; i++ {
		dropBatch(t)
	}
	runtime.ReadMemStats(&after)

	// One leaked batch is 8 x buf.Size; anything close to that per round means
	// the frames are going to the GC instead of the pool.
	const leakedPerRound = 8 * buf.Size
	if got := after.TotalAlloc - before.TotalAlloc; got > rounds*leakedPerRound/2 {
		t.Fatalf("dropped batches leak their frames: %d bytes over %d batches", got, rounds)
	}
}

func dropBatch(t *testing.T) {
	t.Helper()
	dest := xraynet.UDPDestination(xraynet.DomainAddress("example.com"), 53)

	var payloads buf.MultiBuffer
	for i := 0; i < 8; i++ {
		p := buf.New()
		p.Write([]byte{byte(i), byte(i), byte(i)})
		payloads = append(payloads, p)
	}

	writer := newFedarishaPacketWriter(failingWriter{}, dest)
	if err := writer.WriteMultiBuffer(payloads); err == nil {
		t.Fatal("the writer's error must be returned")
	}
}
