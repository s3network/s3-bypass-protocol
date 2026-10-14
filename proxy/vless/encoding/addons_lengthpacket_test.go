package encoding_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xtls/xray-core/common/buf"
	. "github.com/xtls/xray-core/proxy/vless/encoding"
)

func TestLengthPacketReader(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 3*buf.Size+17)
	raw := append([]byte{byte(len(payload) >> 8), byte(len(payload))}, payload...)

	mb, err := NewLengthPacketReader(bytes.NewReader(raw)).ReadMultiBuffer()
	require.NoError(t, err)
	assert.Equal(t, string(payload), mb.String())
	buf.ReleaseMulti(mb)
}

// A truncated packet must fail, and must not leave pooled buffers behind: the
// length prefix is client controlled, so leaking on error drains the pool.
func TestLengthPacketReaderReleasesBuffersOnTruncatedPacket(t *testing.T) {
	// Advertise a large length, then send only a few bytes.
	raw := []byte{0xff, 0xff}
	raw = append(raw, bytes.Repeat([]byte("B"), 1024)...)

	reader := NewLengthPacketReader(bytes.NewReader(raw))
	for range 200 {
		mb, err := reader.ReadMultiBuffer()
		assert.Error(t, err, "a truncated packet must not succeed")
		assert.Nil(t, mb)
	}
}

// The reader must return the packets as a single stream of MultiBuffers.
func TestLengthPacketReaderSuccessivePackets(t *testing.T) {
	first := []byte("hello ")
	second := []byte("world")
	raw := []byte{0, byte(len(first))}
	raw = append(raw, first...)
	raw = append(raw, 0, byte(len(second)))
	raw = append(raw, second...)

	reader := NewLengthPacketReader(bytes.NewReader(raw))
	for _, want := range []string{string(first), string(second)} {
		mb, err := reader.ReadMultiBuffer()
		require.NoError(t, err)
		assert.Equal(t, want, mb.String())
		buf.ReleaseMulti(mb)
	}

	_, err := reader.ReadMultiBuffer()
	assert.ErrorIs(t, err, io.EOF)
}
