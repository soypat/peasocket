package peasocket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/bits"
)

// rxtx.go will contain basic frame marshalling and unmarshalling logic
// agnostic to the data being sent and the websocket control flow.

// Rx provides a way to handle a Websocket transport.
type Rx struct {
	LastReceivedHeader Header
	trp                io.ReadCloser
	RxCallbacks        RxCallbacks
}

type RxCallbacks struct {
	// OnError executed when a decoding error is encountered after
	// consuming a non-zero amoung of bytes from the underlying transport.
	// If this callback is set then it becomes the responsability of the callback
	// to close the underlying transport.
	OnError func(rx *Rx, err error)

	// If OnCtl is not nil then this is executed on every control frame received.
	// These may or may not have a payload that can be readily read from r.
	OnCtl func(rx *Rx, payload io.Reader) error
	// If OnMessage is not nil it is called on the Rx for every application
	// message received (non-control frames).
	// The Application Message is contained in message which will return EOF
	// when the whole payload has been read.
	// The payload bytes are unmasked unless unmasking explicitly disabled on the Rx.
	OnMessage func(rx *Rx, message io.Reader) error
}

// SetRxTransport sets the underlying transport of rx.
func (rx *Rx) SetRxTransport(rc io.ReadCloser) {
	rx.trp = rc
}

// ReadNextFrame reads a frame from the underlying transport of rx.
// This method is meant to be used after setting the rx's callbacks.
// If the callback is not set then the payload data will be read and discarded.
func (rx *Rx) ReadNextFrame() (int, error) {
	var reader io.Reader = rx.trp
	h, n, err := DecodeHeader(reader)
	if err != nil {
		if n > 0 || errors.Is(err, io.EOF) {
			// EOF means the connection is no longer usable, must close.
			rx.handleErr(err)
		}
		return n, err
	}
	rx.LastReceivedHeader = h
	// Perform basic frame validation.
	op := h.FrameType()
	isFin := h.Fin()
	isCtl := op.IsControl()
	if isCtl {
		if !isFin {
			err = errors.New("control frame needs FIN bit set")
		} else if h.PayloadLength > MaxControlPayload {
			err = errors.New("payload too large for control frame (>255)")
		}
	}
	if err != nil {
		rx.handleErr(err)
		return n, err
	}
	// Wrap reader to limit to payload length and unmask the received bytes if payload is masked.
	if h.IsMasked() {
		reader = newMaskedReader(reader, h.Mask)
	}
	lr := &io.LimitedReader{R: reader, N: int64(h.PayloadLength)}
	if isCtl && rx.RxCallbacks.OnCtl != nil {
		err = rx.RxCallbacks.OnCtl(rx, lr)
	} else if rx.RxCallbacks.OnMessage != nil {
		err = rx.RxCallbacks.OnMessage(rx, lr)
	} else {
		// No callback to handle message, we discard data.
		var discard [256]byte
		for err == nil {
			_, err = lr.Read(discard[:])
		}
		if err == io.EOF {
			err = nil
		}
	}
	if err == nil && lr.N != 0 {
		err = errors.New("callback did not consume all bytes in frame")
	}
	if err != nil {
		rx.handleErr(err)
	}
	return n + int(h.PayloadLength-uint64(lr.N)), err
}

// handleErr is a convenience wrapper for RxCallbacks.OnError. If RxCallbacks.OnError
// is not defined then handleErr just closes the connection.
func (rx *Rx) handleErr(err error) {
	if rx.RxCallbacks.OnError != nil {
		rx.RxCallbacks.OnError(rx, err)
		return
	}
	rx.trp.Close()
}

// TxBuffered handles the marshalling of frames over a underlying transport.
type TxBuffered struct {
	buf         bytes.Buffer
	trp         io.WriteCloser
	TxCallbacks TxCallbacks
}

// NewTxBuffered creates a new TxBuffered ready for use.
func NewTxBuffered(wc io.WriteCloser) *TxBuffered {
	return &TxBuffered{trp: wc}
}

// SetTxTransport sets the underlying TxBuffered transport writer.
func (tx *TxBuffered) SetTxTransport(wc io.WriteCloser) {
	tx.trp = wc
}

// TxCallbacks stores functions to be called on events during marshalling of websocket frames.
type TxCallbacks struct {
	OnError func(tx *TxBuffered, err error)
}

// WriteTextMessage writes an UTF-8 encoded message over the transport.
// This implementation does not check if payload is valid UTF-8. This can be done
// with the unicode/utf8 Valid function.
// payload's bytes will be masked and thus mutated if mask is non-zero.
func (tx *TxBuffered) WriteTextMessage(mask uint32, payload []byte) (int, error) {
	return tx.writeMessage(mask, payload, true)
}

// WriteTextMessage writes an arbitrary message over the transport.
// payload's bytes will be masked and thus mutated if mask is non-zero.
func (tx *TxBuffered) WriteMessage(mask uint32, payload []byte) (int, error) {
	return tx.writeMessage(mask, payload, false)
}

func (tx *TxBuffered) writeMessage(mask uint32, payload []byte, isUTF8 bool) (int, error) {
	frameType := FrameBinary
	if isUTF8 {
		frameType = FrameText
	}
	tx.buf.Reset()
	h := newHeader(frameType, uint64(len(payload)), mask, true)
	_, err := h.Encode(&tx.buf)
	if err != nil {
		return 0, err
	}
	if h.IsMasked() {
		_ = maskWS(mask, payload)
	}
	_, err = tx.buf.Write(payload)
	if err != nil {
		return 0, err
	}
	n, err := tx.buf.WriteTo(tx.trp)
	if err != nil && n > 0 {
		tx.handleError(err)
	}
	return int(n), err
}

// WriteFragmentedMessage writes a fragmented message over websocket without allocating
// memory. It reads the application message from payload and masks it with mask.
func (tx *TxBuffered) WriteFragmentedMessage(mask uint32, payload io.Reader, userBuffer []byte) (int, error) {
	if userBuffer == nil {
		userBuffer = make([]byte, 1024)
	}
	if len(userBuffer) < 12 {
		return 0, errors.New("buffer too small to write messages")
	}
	buflen := len(userBuffer)
	masked := mask != 0
	hSize := headerSizeFromTransportSize(buflen, masked)
	maxMessageSize := uint64(buflen - hSize)

	h := newHeader(FrameBinary, maxMessageSize, mask, false)

	isEOF := false
	var n int
	for !isEOF {
		h.Put(userBuffer)
		if n == 0 {
			h = newHeader(FrameContinuation, maxMessageSize, mask, false)
		}
		var nBuf int
		for !isEOF && hSize+nBuf < len(userBuffer) {
			ngot, err := payload.Read(userBuffer[hSize+nBuf:])
			nBuf += ngot
			isEOF = errors.Is(err, io.EOF)
			if err != nil && !isEOF {
				if n > 0 {
					tx.handleError(err)
				}
				return n, err
			}
		}

		if nBuf != int(maxMessageSize) {
			h.PayloadLength = uint64(nBuf)
			newHsize := headerSizeFromMessageSize(nBuf, masked)
			// newHsize will always be lesser than hSize.
			if newHsize != hSize {
				for i := 0; i < nBuf; i++ {
					userBuffer[newHsize+i] = userBuffer[hSize+i]
				}
				hSize = newHsize
			}
			h = newHeader(FrameContinuation, uint64(nBuf), mask, true)
			h.Put(userBuffer)
		}
		maskWS(mask, userBuffer[hSize:hSize+nBuf])
		ngot, err := tx.trp.Write(userBuffer[:hSize+nBuf])
		n += ngot
		if err != nil {
			if n > 0 {
				tx.handleError(err)
			}
			return n, err
		}
	}
	return n, nil
}

func headerSizeFromMessageSize(mSize int, masked bool) (hSize int) {
	maskSize := 0
	if masked {
		maskSize = 4
	}
	switch {
	case mSize < 126:
		// Smallest case, 2 byte header for application message under 126 bytes.
		hSize = 2 + maskSize
	case mSize <= math.MaxUint16:
		// 2 bytes same as last case + 16bit payload length
		hSize = (2 + 2) + maskSize
	default:
		hSize = (2 + 8) + maskSize
	}
	return hSize
}

func headerSizeFromTransportSize(tSize int, masked bool) (hSize int) {
	maskSize := 0
	if masked {
		maskSize = 4
	}
	switch {
	case tSize < 128-maskSize:
		// Smallest case, 2 byte header for application message under 126 bytes.
		hSize = 2 + maskSize
	case tSize < math.MaxUint16-maskSize:
		// 2 bytes same as last case + 16bit payload length
		hSize = (2 + 2) + maskSize
	default:
		hSize = (2 + 8) + maskSize
	}
	return hSize
}

func (tx *TxBuffered) handleError(err error) {
	if tx.TxCallbacks.OnError != nil {
		tx.TxCallbacks.OnError(tx, err)
		return
	}
	tx.trp.Close()
}

// WriteClose writes a Close frame over the websocket connection. reason will be mutated
// if mask is non-zero.
func (tx *TxBuffered) WriteClose(mask uint32, status StatusCode, reason []byte) (int, error) {
	if len(reason) > maxCloseReason {
		return 0, errors.New("close reason too long. must be under 123 bytes")
	}
	tx.buf.Reset()
	pl := uint64(2 + len(reason))
	h := newHeader(FrameClose, pl, mask, true)
	_, err := h.Encode(&tx.buf)
	if err != nil {
		return 0, err
	}
	var codebuf [2]byte
	binary.BigEndian.PutUint16(codebuf[:], uint16(status))
	if h.IsMasked() {
		mask = maskWS(mask, codebuf[:])
	}
	_, err = tx.buf.Write(codebuf[:2])
	if err != nil {
		return 0, err
	}
	if h.IsMasked() {
		_ = maskWS(mask, reason)
	}
	_, err = writeFull(&tx.buf, reason)
	if err != nil {
		return 0, err
	}
	n, err := tx.buf.WriteTo(tx.trp)
	if err != nil && n > 0 {
		tx.handleError(err)
	}
	return int(n), err
}

// WritePong writes a pong message over the Tx.
func (tx *TxBuffered) WritePong(message []byte) (int, error) {
	if len(message) > MaxControlPayload {
		return 0, errors.New("pong message too long, must be under 125 bytes")
	}
	return tx.writepingpong(FramePong, 0, message)
}

// WritePing writes a ping message over the Tx.
func (tx *TxBuffered) WritePing(mask uint32, message []byte) (int, error) {
	if len(message) > MaxControlPayload {
		return 0, errors.New("ping message too long, must be under 125 bytes")
	}
	if mask == 0 {
		return 0, errors.New("pings require a non-zero mask")
	}
	return tx.writepingpong(FramePing, mask, message)
}

func (tx *TxBuffered) writepingpong(frame FrameType, mask uint32, message []byte) (int, error) {
	tx.buf.Reset()
	pl := uint64(len(message))
	h := newHeader(frame, pl, mask, true)
	_, err := h.Encode(&tx.buf)
	if err != nil {
		return 0, err
	}
	if h.IsMasked() {
		_ = maskWS(mask, message)
	}
	_, err = writeFull(&tx.buf, message)
	if err != nil {
		return 0, err
	}
	n, err := tx.buf.WriteTo(tx.trp)
	if err != nil && n > 0 {
		tx.handleError(err)
	}
	return int(n), err
}

// newMaskedReader creates a new MaskedReader ready for use.
func newMaskedReader(r io.Reader, maskKey uint32) *maskedReader {
	return &maskedReader{R: r, MaskKey: maskKey}
}

// maskedReader applies the [WebSocket masking algorithm]
// to the underlying reader.
//
// [WebSocket masking algorithm]: https://tools.ietf.org/html/rfc6455#section-5.3
type maskedReader struct {
	R       io.Reader
	MaskKey uint32
}

// Read implements the [io.Reader] interface for the websocket masking algorithm.
func (mr *maskedReader) Read(b []byte) (int, error) {
	lenRead, err := mr.R.Read(b)
	if lenRead == 0 {
		return 0, err
	}
	mr.MaskKey = maskWS(mr.MaskKey, b[:lenRead])
	return lenRead, nil
}

func maskWS(key uint32, b []byte) uint32 {
	if key == 0 {
		return 0
	}
	for len(b) >= 4 {
		v := binary.BigEndian.Uint32(b)
		binary.BigEndian.PutUint32(b, v^key)
		b = b[4:]
	}

	if len(b) != 0 {
		// xor remaining bytes and shift mask.
		for i := range b {
			b[i] ^= byte(key >> 24)
			key = bits.RotateLeft32(key, 8)
		}
	}
	return key
}
