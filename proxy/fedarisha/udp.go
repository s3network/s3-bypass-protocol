package fedarisha

import (
	"encoding/binary"
	"io"

	"github.com/xtls/xray-core/common/buf"
	xraynet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/xudp"
)

const targetHeaderUDPFlag uint16 = 0x8000

// maxPacketMetaBytes bounds the destination block that precedes a packet's
// payload. writePacketDestination emits one byte of network tag plus a
// destination — at most a 255-byte domain and a port — so this is the ceiling
// on what a well-formed frame can carry.
const maxPacketMetaBytes = 512

type fedarishaPacketWriter struct {
	writer buf.Writer
	dest   xraynet.Destination
}

func newFedarishaPacketWriter(writer buf.Writer, dest xraynet.Destination) *fedarishaPacketWriter {
	return &fedarishaPacketWriter{
		writer: writer,
		dest:   dest,
	}
}

func (w *fedarishaPacketWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	defer buf.ReleaseMulti(mb)

	frames := make(buf.MultiBuffer, 0, len(mb))
	// Every frame built so far belongs to this writer until the writer is
	// handed them. An error part-way through — or a writer that refuses the
	// batch — used to drop up to 64 pooled buffers on the floor, where they are
	// garbage collected outside the pool instead of going back to it.
	defer func() {
		if frames != nil {
			buf.ReleaseMulti(frames)
		}
	}()
	for _, payload := range mb {
		if payload.IsEmpty() {
			continue
		}

		frame := buf.New()
		dest := w.dest
		if payload.UDP != nil {
			dest = *payload.UDP
		}

		if dest.Network == xraynet.Network_UDP {
			if err := writePacketDestination(frame, dest); err != nil {
				frame.Release()
				return err
			}
		} else {
			frame.Write([]byte{0, 0})
		}

		if err := binary.Write(frame, binary.BigEndian, uint16(payload.Len())); err != nil {
			frame.Release()
			return err
		}
		frame.Write(payload.Bytes())
		frames = append(frames, frame)
	}

	if frames.IsEmpty() {
		frames = nil
		return nil
	}

	out := frames
	frames = nil // ownership passes to w.writer from here on
	return w.writer.WriteMultiBuffer(out)
}

type fedarishaPacketReader struct {
	reader      io.Reader
	defaultDest xraynet.Destination
}

func newFedarishaPacketReader(reader io.Reader, defaultDest xraynet.Destination) *fedarishaPacketReader {
	return &fedarishaPacketReader{
		reader:      reader,
		defaultDest: defaultDest,
	}
}

func (r *fedarishaPacketReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r.reader, lenBuf[:]); err != nil {
		return nil, err
	}

	metaLen := binary.BigEndian.Uint16(lenBuf[:])
	// Bounded BEFORE reading, the way payloadLen is. The header word is
	// attacker-controlled: at anything above buf.Size ReadFullFrom refuses
	// without consuming a byte, so the length prefix of the payload is read as
	// the first two bytes of the payload and the stream desynchronises from
	// there on. Refusing up front is the same failure the oversized payload
	// already produces, only deterministically.
	if metaLen > maxPacketMetaBytes {
		return nil, io.ErrUnexpectedEOF
	}
	var dest *xraynet.Destination
	if metaLen > 0 {
		meta := buf.New()
		if _, err := meta.ReadFullFrom(r.reader, int32(metaLen)); err != nil {
			meta.Release()
			return nil, err
		}
		parsed, err := readPacketDestination(meta)
		meta.Release()
		if err != nil {
			return nil, err
		}
		dest = parsed
	} else if r.defaultDest.Network == xraynet.Network_UDP {
		defaultDest := r.defaultDest
		dest = &defaultDest
	}

	if _, err := io.ReadFull(r.reader, lenBuf[:]); err != nil {
		return nil, err
	}
	payloadLen := binary.BigEndian.Uint16(lenBuf[:])
	if payloadLen == 0 || int32(payloadLen) > buf.Size {
		return nil, io.ErrUnexpectedEOF
	}

	payload := buf.New()
	if _, err := payload.ReadFullFrom(r.reader, int32(payloadLen)); err != nil {
		payload.Release()
		return nil, err
	}
	payload.UDP = dest

	return buf.MultiBuffer{payload}, nil
}

func writePacketDestination(frame *buf.Buffer, dest xraynet.Destination) error {
	meta := buf.New()
	defer meta.Release()

	meta.WriteByte(byte(2))
	if err := xudp.AddrParser.WriteAddressPort(meta, dest.Address, dest.Port); err != nil {
		return err
	}

	if err := binary.Write(frame, binary.BigEndian, uint16(meta.Len())); err != nil {
		return err
	}
	frame.Write(meta.Bytes())
	return nil
}

func readPacketDestination(meta *buf.Buffer) (*xraynet.Destination, error) {
	if meta.IsEmpty() {
		return nil, nil
	}

	network := meta.Byte(0)
	meta.Advance(1)

	addr, port, err := xudp.AddrParser.ReadAddressPort(nil, meta)
	if err != nil {
		return nil, err
	}
	if network != 2 {
		return nil, nil
	}

	dest := xraynet.UDPDestination(addr, port)
	return &dest, nil
}
