package peasocket

import (
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
)

// rxtx.go will contain basic frame marshalling and unmarshalling logic
// agnostic to the data being sent and the websocket control flow.

type Rx struct {
	LastReceivedHeader Header
	trp                io.ReadCloser
	RxCallbacks        RxCallbacks
}

type RxCallbacks struct {
	// OnError executed when a decoding error is encountered after
	// consuming a non-zero amoung of bytes from the underlying transport.
	// If this callback is set then it becomes the responsability of the callback
	// to close the underlying transport. The Websocket connection must be closed after
	//
	OnError   func(*Rx, error)
	OnCtl     func(*Rx, io.Reader) error
	OnPayload func(*Rx, io.Reader) error
}

func (rx *Rx) ReadNextFrame() (int, error) {
	var reader io.Reader = rx.trp
	h, n, err := DecodeHeader(reader)
	if err != nil {
		if n > 0 {
			rx.RxCallbacks.OnError(rx, err)
		}
		return n, err
	}
	rx.LastReceivedHeader = h
	// Perform basic frame validation.
	op := h.Opcode()
	isFin := h.Fin()
	isCtl := op.IsControl()
	if isCtl {
		if !isFin {
			err = errors.New("control frame needs FIN bit set")
		} else if h.PayloadLength > 255 {
			err = errors.New("payload too large for control frame (>255)")
		}
	}
	if err != nil {
		rx.handleErr(err)
		return n, err
	}
	// Wrap reader to limit to payload length and unmask the received bytes if payload is masked.
	if h.Masked() {
		reader = newMaskedReader(reader, h.Mask)
	}
	lr := &io.LimitedReader{R: reader, N: int64(h.PayloadLength)}
	if isCtl && rx.RxCallbacks.OnCtl != nil {
		err = rx.RxCallbacks.OnCtl(rx, lr)
	} else if rx.RxCallbacks.OnPayload != nil {
		err = rx.RxCallbacks.OnPayload(rx, lr)
	} else {
		// No callback to handle message, we discard data.
		var discard [256]byte
		for err == nil {
			_, err = lr.Read(discard[:])
		}
	}
	if err == nil && lr.N != 0 {
		err = errors.New("callback did not consume all bytes in frame")
	}
	if err != nil {
		rx.handleErr(err)
	}
	return n, err
}

func (rx *Rx) handleErr(err error) {
	if rx.RxCallbacks.OnError != nil {
		rx.RxCallbacks.OnError(rx, err)
		return
	}
	rx.trp.Close()
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
	key := mr.MaskKey
	b = b[:lenRead]
	for len(b) >= 4 {
		v := binary.LittleEndian.Uint32(b)
		binary.LittleEndian.PutUint32(b, v^key)
		b = b[4:]
	}

	if len(b) != 0 {
		// xor remaining bytes and shift mask.
		for i := range b {
			b[i] ^= byte(key)
			key = bits.RotateLeft32(key, -8)
		}
		mr.MaskKey = key
	}

	return lenRead, nil
}
