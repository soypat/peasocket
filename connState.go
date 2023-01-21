package peasocket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// connState stores the persisting state of a websocket client connection.
// Since this state is shared between frames it is protected by a mutex so that
// the Client implementation is concurrent-safe.
type connState struct {
	mu       sync.Mutex
	rxBuffer bytes.Buffer
	// currentMessageOpcode is either FrameText or FrameBinary during normal functioning.
	// When messages are discarded currentMessageOpcodeBecomes
	currentMessageOpcode FrameType
	currentMessageSize   uint64
	// messageSizes correspond to the websocket messages
	// in the rxBuffer
	messageMetadata []struct {
		length uint64
		ftype  FrameType
	}
	pendingPingOrClose []byte
	expectedPong       []byte
	copyBuf            []byte
	closeErr           error
}

func (cs *connState) callbacks(isClient bool) (RxCallbacks, TxCallbacks) {
	closureErr := errClientGracefulClose
	if isClient {
		closureErr = errServerGracefulClose
	}
	return RxCallbacks{
			OnMessage: func(rx *Rx, message io.Reader) error {
				cs.mu.Lock()
				defer cs.mu.Unlock()
				if cs.closeErr != nil {
					return cs.closeErr
				}
				n, err := io.CopyBuffer(&cs.rxBuffer, message, cs.copyBuf)
				if err != nil {
					cs.currentMessageOpcode = 0
					cs.currentMessageSize = 0
					return err
				} else if cs.currentMessageSize == 0 {
					cs.currentMessageOpcode = rx.LastReceivedHeader.FrameType()
				}
				cs.currentMessageSize += uint64(n)
				if rx.LastReceivedHeader.Fin() {
					cs.messageMetadata = append(cs.messageMetadata, struct {
						length uint64
						ftype  FrameType
					}{
						length: cs.currentMessageSize,
						ftype:  cs.currentMessageOpcode,
					})
					cs.currentMessageSize = 0
					cs.currentMessageOpcode = 0
				}
				return nil
			},
			OnCtl: func(rx *Rx, payload io.Reader) (err error) {
				op := rx.LastReceivedHeader.FrameType()
				plen := rx.LastReceivedHeader.PayloadLength
				cs.mu.Lock()
				defer cs.mu.Unlock()
				if cs.closeErr != nil {
					return cs.closeErr
				}
				var n int
				switch op {
				case FramePing:
					n, err = io.ReadFull(payload, cs.pendingPingOrClose[:plen])
					if err != nil {
						break
					}
					// Replaces pending ping with new ping.
					cs.pendingPingOrClose = cs.pendingPingOrClose[:n]

				case FramePong:
					var pongBuf [MaxControlPayload]byte
					n, err = io.ReadFull(payload, pongBuf[:plen])
					if err != nil {
						break
					}
					if bytes.Equal(cs.expectedPong, pongBuf[:n]) {
						cs.expectedPong = nil
					} else {
						err = errUnexpectedPong
					}

				case FrameClose:
					n, err = io.ReadFull(payload, cs.pendingPingOrClose[:plen])
					if err != nil {
						break
					}
					cs.closeErr = closureErr
					// Replaces pending ping with new ping.
					cs.pendingPingOrClose = cs.pendingPingOrClose[:n]
				default:
					panic("unknown control FrameType") // This should be unreachable.
				}
				if err != nil {
					err = fmt.Errorf("decoding control frame %s: %w", rx.LastReceivedHeader, err)
				}
				return err
			},
			OnError: func(rx *Rx, err error) {
				cs.CloseConn(err)
			},
		}, TxCallbacks{
			OnError: func(tx *TxBuffered, err error) {
				cs.CloseConn(err)
			},
		}
}

func (cs *connState) PendingAction() bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.closeErr == nil && len(cs.pendingPingOrClose) != 0
}

func (cs *connState) Err() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.closeErr
}

func (cs *connState) IsConnected() bool {
	return cs.Err() == nil
}

func (cs *connState) IsClosed() bool {
	return cs.Err() != nil
}

func (cs *connState) PeekNextMessage() (FrameType, int64) {
	if cs.BufferedMessages() == 0 {
		return 0, 0
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	got := cs.messageMetadata[len(cs.messageMetadata)-1]
	return got.ftype, int64(got.length)
}

func (cs *connState) CloseWebsocket(err error, mask uint32, tx *TxBuffered) error {
	if err == nil {
		panic("nil error")
	}
	if cs.IsClosed() {
		return errors.New("no websocket connection to close")
	}
	closeErr, ok := err.(*CloseError)
	if !ok {
		closeErr.Status = StatusAbnormalClosure
		closeErr.Reason = []byte(err.Error())
	}
	cs.mu.Lock()
	cs.closeErr = err
	cs.mu.Unlock()
	_, err = tx.WriteClose(mask, closeErr.Status, closeErr.Reason)
	if err != nil {
		return err
	}

	return nil
}

func (cs *connState) GetServerClosedReason() (sc StatusCode, reason []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closeErr == nil || len(cs.pendingPingOrClose) < 2 {
		return 0, nil
	}
	sc = StatusCode(binary.BigEndian.Uint16(cs.pendingPingOrClose[:2]))
	return sc, cs.pendingPingOrClose[2:]
}

func (cs *connState) Buffered() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.rxBuffer.Len()
}

func (cs *connState) BufferedMessages() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.messageMetadata)
}

// DiscardReadMessages discards all completely read messages.
func (cs *connState) DiscardReadMessages() {
	cs.mu.Lock()
	n := uint64(0)
	for _, msgData := range cs.messageMetadata {
		n += msgData.length
	}
	cs.messageMetadata = nil
	cs.rxBuffer.Next(int(n))
	cs.mu.Unlock()
}

// OnConnect is meant to be called on opening a new connection to delete
// previous connection state.
func (cs *connState) OnConnect() {
	cs.mu.Lock()
	cs.closeErr = nil
	cs.rxBuffer.Truncate(int(cs.currentMessageSize))
	cs.currentMessageOpcode = 0
	cs.currentMessageSize = 0
	cs.expectedPong = cs.expectedPong[:0]
	cs.pendingPingOrClose = cs.pendingPingOrClose[:0]
	cs.mu.Unlock()
}

func (cs *connState) NextMessage() (io.Reader, FrameType, error) {
	var buf bytes.Buffer
	ft, _, err := cs.WriteNextMessageTo(&buf)
	if err != nil {
		return nil, 0, err
	}
	return &buf, ft, nil
}

func (cs *connState) WriteNextMessageTo(w io.Writer) (FrameType, int64, error) {
	buffered := cs.Buffered()
	if buffered == 0 || len(cs.messageMetadata) == 0 {
		return 0, 0, errors.New("no messages in buffer")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	got := cs.messageMetadata[0]
	cs.messageMetadata = cs.messageMetadata[1:]
	if len(cs.messageMetadata) == 0 {
		cs.messageMetadata = nil // release memory allocation to prevent leak.
	}
	n, err := io.CopyBuffer(w, io.LimitReader(&cs.rxBuffer, int64(got.length)), cs.copyBuf)
	if err != nil {
		return 0, n, err // should be unreachable.
	}
	return got.ftype, n, nil
}

func (state *connState) CloseConn(err error) {
	if err == nil {
		panic("close error cannot be nil")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closeErr == nil {
		state.closeErr = err // Will set first error encountered after close.
	}
}

func (state *connState) ReplyOutstandingFrames(mask uint32, tx *TxBuffered) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closeErr != nil || len(state.pendingPingOrClose) == 0 {
		return nil // Nothing to do.
	}
	_, err := tx.WritePong(mask, state.pendingPingOrClose)
	state.pendingPingOrClose = state.pendingPingOrClose[:0]
	if err != nil {
		err = fmt.Errorf("failed while responding pong to incoming ping: %w", err)
	}
	return err
}

func (state *connState) makeCloseErr() *CloseError {
	if len(state.pendingPingOrClose) < 2 {
		return &CloseError{Status: StatusNoStatusRcvd}
	}
	return &CloseError{
		Status: StatusCode(binary.BigEndian.Uint16(state.pendingPingOrClose[:2])),
		Reason: state.pendingPingOrClose[2:],
	}
}
