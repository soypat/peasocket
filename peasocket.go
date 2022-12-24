package peasocket

import (
	"bytes"
	"encoding/binary"
	"errors"
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
type Opcode uint8

func (op Opcode) IsControl() bool { return op < maxOpcode && op&(1<<3) != 0 }

// Opcode returns the header's operation code as per [section 11].
//
// [section 11]: https://tools.ietf.org/html/rfc6455#section-11.8.
func (h Header) Opcode() Opcode {
	return Opcode(h.firstByte & 0b1111)
}

func (h Header) Masked() bool { return h.Mask != 0 }
func (h Header) Fin() bool    { return h.firstByte&(1<<7) != 0 }
func (h Header) Rsv1() bool   { return h.firstByte&(1<<6) != 0 }
func (h Header) Rsv2() bool   { return h.firstByte&(1<<5) != 0 }
func (h Header) Rsv3() bool   { return h.firstByte&(1<<4) != 0 }

func NewHeader(op Opcode, payload, mask int, fin bool) (Header, error) {
	if op > 0b1111 {
		return Header{}, errors.New("invalid opcode")
	}
	if payload < 0 {
		return Header{}, errors.New("negative payload length")
	}
	if mask < 0 || mask > math.MaxUint32 {
		return Header{}, errors.New("negative mask or mask exceeds 32bit unsigned integer range")
	}

	return Header{firstByte: byte(op) | b2u8(fin)<<7, Mask: uint32(mask), PayloadLength: uint64(payload)}, nil
}

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func DecodeHeader(r io.Reader) (Header, int, error) {
	firstByte, err := decodeByte(r)
	if err != nil {
		return Header{}, 0, err
	}
	plen, masked, n, err := decodePayload(r)
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

func decodePayload(r io.Reader) (v uint64, masked bool, n int, err error) {
	var buf [8]byte
	n, err = r.Read(buf[:1]) // Read single byte
	masked = buf[0]&(1<<7) != 0
	buf[0] &^= (1 << 7) // Exclude mask bit from payload.
	if err != nil {
		if n == 0 {
			return 0, masked, n, errors.New("unexpected 0 bytes read from buffer and no error returned")
		} else if n == 1 && errors.Is(err, io.EOF) {
			return uint64(buf[0]), masked, n, nil
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

func decodeByte(r io.Reader) (value byte, err error) {
	var vbuf [1]byte
	n, err := r.Read(vbuf[:])
	if err != nil && n == 1 && errors.Is(err, io.EOF) {
		err = nil // Byte was read successfully albeit with an EOF.
	} else if n == 0 {
		err = errors.New("unexpected 0 bytes read from buffer and no error returned")
	}
	return vbuf[0], err
}

func readFull(src io.Reader, dst []byte) (int, error) {
	n, err := src.Read(dst)
	if err == nil && n != len(dst) {
		var buffer [256]byte
		// TODO(soypat): Avoid heavy heap allocation by implementing lightweight algorithm here.
		i64, err := io.CopyBuffer(bytes.NewBuffer(dst[n:]), src, buffer[:])
		i := int(i64)
		if err != nil && errors.Is(err, io.EOF) && i == len(dst[n:]) {
			err = nil
		}
		return n + i, err
	}
	return n, err
}
