package peasocket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Header represents a [WebSocket frame header].
//
//	0                   1                   2                   3
//	0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-------+-+-------------+-------------------------------+
//	|F|R|R|R| opcode|M| Payload len |    Extended payload length    |
//	|I|S|S|S|  (4)  |A|     (7)     |             (16/64)           |
//	|N|V|V|V|       |S|             |   (if payload len==126/127)   |
//	| |1|2|3|       |K|             |                               |
//	+-+-+-+-+-------+-+-------------+ - - - - - - - - - - - - - - - +
//	|     Extended payload length continued, if payload len == 127  |
//	+ - - - - - - - - - - - - - - - +-------------------------------+
//	|                               |Masking-key, if MASK set to 1  |
//	+-------------------------------+-------------------------------+
//	| Masking-key (continued)       |          Payload Data         |
//	+-------------------------------- - - - - - - - - - - - - - - - +
//	:                     Payload Data continued ...                :
//	+ - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - +
//	|                     Payload Data continued ...                |
//	+---------------------------------------------------------------+
//
// [WebSocket frame header]: https://tools.ietf.org/html/rfc6455#section-5.2.
type Header struct {
	firstByte byte
	// All frames sent from the client to the server are masked by a
	// 32-bit value that is contained within the frame.  This field is
	// present if the mask bit is set to 1 and is absent if the mask bit
	// is set to 0.  See Section 5.3 for further information on client-
	// to-server masking.
	Mask uint32 // A mask key of zero has no effect in XOR operations, thus zero value is no mask.

	// Defines whether the "Payload data" is masked.  If set to 1, a
	// masking key is present in masking-key, and this is used to unmask
	// the "Payload data" as per Section 5.3.  All frames sent from
	// client to server have this bit set to 1.
	// Masked bool
	// The length of the "Payload data", in bytes: if 0-125, that is the
	// payload length.  If 126, the following 2 bytes interpreted as a
	// 16-bit unsigned integer are the payload length.  If 127, the
	// following 8 bytes interpreted as a 64-bit unsigned integer (the
	// most significant bit MUST be 0) are the payload length.
	PayloadLength uint64
}

// Defines the interpretation of the "Payload data".  If an unknown
// opcode is received, the receiving endpoint MUST Fail the
// WebSocket Connection.
type FrameType uint8

// IsControl returns true if the frame is valid and is a control frame (ping, pong or close).
func (op FrameType) IsControl() bool { return op <= maxOpcode && op&(1<<3) != 0 }

// FrameType returns the header's operation code as per [section 11].
//
// [section 11]: https://tools.ietf.org/html/rfc6455#section-11.8.
func (h Header) FrameType() FrameType {
	return FrameType(h.firstByte & 0b1111)
}

// IsMasked returns true if payload is masked.
func (h Header) IsMasked() bool { return h.Mask != 0 }

// Fin returns true if the FIN bit in the frame header is set. Always should be set for control frames.
func (h Header) Fin() bool { return h.firstByte&(1<<7) != 0 }

// Rsv1 returns true if the first frame header reserved bit is set. Used for extensions.
func (h Header) Rsv1() bool { return h.firstByte&(1<<6) != 0 }

// Rsv2 returns true if the second frame header reserved bit is set. Used for extensions.
func (h Header) Rsv2() bool { return h.firstByte&(1<<5) != 0 }

// Rsv3 returns true if the third frame header reserved bit is set. Used for extensions.
func (h Header) Rsv3() bool { return h.firstByte&(1<<4) != 0 }

// String returns a human readable representation of the Frame header in the following format:
//
//	Frame:<frame type> (payload=<payload length decimal>) FIN:<true|false> <RSV if set>
func (h Header) String() string {
	fin, rsv1, rsv2, rsv3 := h.Fin(), h.Rsv1(), h.Rsv2(), h.Rsv3()
	if rsv1 || rsv2 || rsv3 {
		return fmt.Sprintf("Frame:%v (payload=%v) FIN:%t  RSV:%v|%v|%v", h.FrameType().String(), h.PayloadLength, fin, rsv1, rsv2, rsv3)
	}
	return fmt.Sprintf("Frame:%v (payload=%v) FIN:%t", h.FrameType().String(), h.PayloadLength, fin)
}

// NewHeader creates a new websocket frame header. Set mask to 0 to disable masking
// on the payload.
func NewHeader(op FrameType, payload, mask int, fin bool) (Header, error) {
	if op > 0b1111 {
		return Header{}, errors.New("invalid opcode")
	}
	if payload < 0 {
		return Header{}, errors.New("negative payload length")
	}
	if mask < 0 || mask > math.MaxUint32 {
		return Header{}, errors.New("negative mask or mask exceeds 32bit unsigned integer range")
	}
	return newHeader(op, uint64(payload), uint32(mask), fin), nil
}

// newHeader is a convenience function for easily creating Headers skipping the validation step.
func newHeader(op FrameType, payload uint64, mask uint32, fin bool) Header {
	return Header{firstByte: byte(op) | b2u8(fin)<<7, Mask: mask, PayloadLength: payload}
}

// DecodeHeader reads a header from the io.Reader. It assumes the first byte corresponds
// to the header's first byte.
func DecodeHeader(r io.Reader) (Header, int, error) {
	firstByte, err := decodeByte(r)
	if err != nil {
		return Header{}, 0, err
	}
	plen, masked, n, err := decodePayloadLength(r)
	n++ //add first byte
	if err != nil {
		return Header{}, n, err
	}
	h := Header{
		firstByte:     firstByte,
		PayloadLength: plen,
	}
	if masked {
		var buf [4]byte
		ngot, err := readFull(r, buf[:4])
		n += ngot
		if err != nil {
			return Header{}, n, err
		}
		h.Mask = binary.BigEndian.Uint32(buf[:4])
	}
	return h, n, nil
}

func decodePayloadLength(r io.Reader) (v uint64, masked bool, n int, err error) {
	var buf [8]byte
	n, err = r.Read(buf[:1]) // Read single byte
	masked = buf[0]&(1<<7) != 0
	buf[0] &^= (1 << 7) // Exclude mask bit from payload.
	if err != nil {
		if n == 0 {
			return 0, masked, n, errors.New("unexpected 0 bytes read from buffer and no error returned")
		} else if n == 1 && errors.Is(err, io.EOF) && buf[0] == 0 {
			// No payload. to read.
			return 0, masked, n, nil
		}
		return 0, masked, n, err
	}
	var ngot int
	switch buf[0] {
	case 126:
		ngot, err = readFull(r, buf[0:2])
		v = uint64(binary.BigEndian.Uint16(buf[:2]))
	case 127:
		ngot, err = readFull(r, buf[:8])
		v = binary.BigEndian.Uint64(buf[:8])
	default:
		return uint64(buf[0]), masked, n, nil
	}
	n += ngot
	if err != nil {
		return 0, masked, n, err
	}
	if v < 126 {
		return 0, masked, n, errors.New("payload length protocol violation: minimal number of bytes MUST be used to encode the length")
	}
	return v, masked, n, nil
}

// Encode writes the websocket header to w as it would be sent over the wire.
// It does not encode a payload.
func (h *Header) Encode(w io.Writer) (int, error) {
	err := encodeByte(w, h.firstByte)
	if err != nil {
		return 0, err
	}
	n, err := h.encodePayloadLength(w)
	n++
	if err != nil {
		return n, err
	}
	if h.IsMasked() {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:4], h.Mask)
		ngot, err := writeFull(w, buf[:4])
		n += ngot
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (h *Header) encodePayloadLength(w io.Writer) (n int, err error) {
	var buf [8]byte
	mask := b2u8(h.IsMasked()) << 7
	pl := h.PayloadLength
	switch {
	case pl < 126:
		buf[0] = mask | byte(pl)
		n, err = w.Write(buf[:1])

	case pl <= math.MaxUint16:
		err = encodeByte(w, mask|126)
		if err != nil {
			return n, err
		}
		n++
		binary.BigEndian.PutUint16(buf[:2], uint16(pl))
		n, err = writeFull(w, buf[:2])

	default:
		err = encodeByte(w, mask|127)
		if err != nil {
			return n, err
		}
		n++
		binary.BigEndian.PutUint64(buf[:8], pl)
		n, err = writeFull(w, buf[:8])
	}
	return n, err
}
