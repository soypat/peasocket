package peasocket

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// Header represents a [WebSocket frame header]. A header can occupy
// 2 to 10 bytes on the wire depending on payload size and if content is masked.
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
func NewHeader(op FrameType, payload int, mask uint, fin bool) (Header, error) {
	if op > 0b1111 {
		return Header{}, errors.New("invalid opcode")
	}
	if payload < 0 {
		return Header{}, errors.New("negative payload length")
	}
	if mask > math.MaxUint32 {
		return Header{}, errors.New("negative mask or mask exceeds 32bit unsigned integer range")
	}
	return newHeader(op, uint64(payload), uint32(mask), fin), nil
}

// newHeader is a convenience function for easily creating Headers skipping the validation step.
func newHeader(op FrameType, payload uint64, mask uint32, fin bool) Header {
	return Header{firstByte: byte(op) | b2u8(fin)<<7, Mask: mask, PayloadLength: payload}
}

var (
	errReadingFirstHeaderByte = errors.New("reading first byte in header")
)

// DecodeHeader reads a header from the io.Reader. It assumes the first byte corresponds
// to the header's first byte.
func DecodeHeader(r io.Reader) (Header, int, error) {
	var vbuf [1]byte
	_, err := r.Read(vbuf[:])
	if err != nil {
		return Header{}, 0, fmt.Errorf("%s: %w", errReadingFirstHeaderByte, err)
	}
	firstByte := vbuf[0]
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
		ngot, err := io.ReadFull(r, buf[:4])
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
		ngot, err = io.ReadFull(r, buf[0:2])
		v = uint64(binary.BigEndian.Uint16(buf[:2]))
	case 127:
		ngot, err = io.ReadFull(r, buf[:8])
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
	var buf [MaxHeaderSize]byte
	n, err := h.Put(buf[:])
	if err != nil {
		return 0, err
	}
	return writeFull(w, buf[:n])
}

// Put encodes the header into the start of the byte slice.
func (h *Header) Put(b []byte) (int, error) {
	masked := h.IsMasked()
	hsize := headerSizeFromMessageSize(int(h.PayloadLength), masked)
	if len(b) < hsize {
		return 0, io.ErrShortBuffer
	}
	b[0] = h.firstByte
	n := h.putPayloadLength(b[1:]) + 1
	if masked {
		binary.BigEndian.PutUint32(b[n:], h.Mask)
		n += 4
	}
	// TODO(soypat): Remove once tests cover all cases.
	if hsize != n {
		panic("bug in Header.Put")
	}
	return n, nil
}

// putPayloadLength needs buf to be of length 9.
func (h *Header) putPayloadLength(buf []byte) (n int) {
	mask := b2u8(h.IsMasked()) << 7
	pl := h.PayloadLength
	switch {
	case pl < 126:
		buf[0] = mask | byte(pl)
		n = 1
	case pl <= math.MaxUint16:
		buf[0] = mask | 126
		binary.BigEndian.PutUint16(buf[1:3], uint16(pl))
		n = 3

	default:
		buf[0] = mask | 127
		binary.BigEndian.PutUint64(buf[1:9], pl)
		n = 9
	}
	return n
}

func defaultEntropy() uint32 {
	var output [4]byte
	_, err := io.ReadFull(rand.Reader, output[:])
	if err != nil {
		panic(err)
	}
	return binary.BigEndian.Uint32(output[:])
}

// b2u8 converts a bool to an uint8. true==1, false==0.
func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// ExponentialBackoff implements a [Exponential Backoff]
// delay algorithm to prevent saturation network or processor
// with failing tasks. An ExponentialBackoff with a non-zero MaxWait is ready for use.
//
// [Exponential Backoff]: https://en.wikipedia.org/wiki/Exponential_backoff
type ExponentialBackoff struct {
	// Wait defines the amount of time that Miss will wait on next call.
	Wait time.Duration
	// Maximum allowable value for Wait.
	MaxWait time.Duration
	// StartWait is the value that Wait takes after a call to Hit.
	StartWait time.Duration
	// ExpMinusOne is the shift performed on Wait minus one, so the zero value performs a shift of 1.
	ExpMinusOne uint32
}

// Hit sets eb.Wait to the StartWait value.
func (eb *ExponentialBackoff) Hit() {
	if eb.MaxWait == 0 {
		panic("MaxWait cannot be zero")
	}
	eb.Wait = eb.StartWait
}

// Miss sleeps for eb.Wait and increases eb.Wait exponentially.
func (eb *ExponentialBackoff) Miss() {
	const k = 1
	wait := eb.Wait
	maxWait := eb.MaxWait
	exp := eb.ExpMinusOne + 1
	if maxWait == 0 {
		panic("MaxWait cannot be zero")
	}
	time.Sleep(wait)
	wait |= time.Duration(k)
	wait <<= exp
	if wait > maxWait {
		wait = maxWait
	}
	eb.Wait = wait
}
