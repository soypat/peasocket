package peasocket

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unsafe"
)

// flag error for when server gracefully closes the websocket connection with a Close frame.
var (
	errServerGracefulClose = errors.New("server closure")
	errClientGracefulClose = errors.New("client closure")
)

// NewClient creates a new Client ready to connect to a websocket enabled server.
// serverURL can start with wss:// or ws:// to enable TLS (secure websocket protocol).
// entropy is a function that returns randomized 32 bit integers from a high-entropy source.
// If entropy is nil then a default will be used.
func NewClient(serverURL string, entropy func() uint32) *Client {
	if entropy == nil {
		entropy = defaultEntropy
	}

	c := &Client{
		serverURL: serverURL,
		entropy:   entropy,
		state: clientState{
			closed:             true,
			pendingPingOrClose: make([]byte, 0, MaxControlPayload),
			expectedPong:       make([]byte, 0, MaxControlPayload),
			closeErr:           net.ErrClosed,
		},
	}
	rxc, txc := c.state.callbacks()
	c.Rx.RxCallbacks = rxc
	c.Tx.TxCallbacks = txc
	return c
}

// Client is a client websocket implementation.
type Client struct {
	txlock    sync.Mutex
	Tx        TxBuffered
	Rx        Rx
	serverURL string
	// entropy is assured to be defined after the first websocket connection.
	entropy func() uint32

	state clientState
}

// Dial not yet tested. Performs websocket handshake over net.Conn.
func (c *Client) Dial(conn net.Conn, overwriteHeaders http.Header) error {
	if !c.state.IsClosed() {
		return errors.New("must first close existing connection before starting a new one")
	}
	secureWebsocketKey := c.secureKeyString()
	req, err := c.makeRequest(context.Background(), secureWebsocketKey, overwriteHeaders)
	if err != nil {
		return err
	}
	err = req.Write(conn) // Write HTTP/1.1 request with challenge.
	if err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReaderSize(conn, 1024), req)
	if err != nil {
		return err
	}
	if err := validateServerResponse(resp, secureWebsocketKey); err != nil {
		return err
	}
	c.Tx.SetTxTransport(conn)
	c.Rx.SetRxTransport(conn)
	c.state.closeErr = nil
	c.state.closed = false
	return nil
}

// DialViaHTTPClient completes a websocket handshake and returns an error if
// the handshake failed. After a successful handshake the Client is ready to begin
// communicating with the server via websocket protocol.
func (c *Client) DialViaHTTPClient(ctx context.Context, overwriteHeaders http.Header) error {
	if !c.state.IsClosed() {
		return errors.New("must first close existing connection before starting a new one")
	}
	if ctx == nil {
		return errors.New("nil context")
	}
	secureWebsocketKey := c.secureKeyString()
	req, err := c.makeRequest(ctx, secureWebsocketKey, overwriteHeaders)
	if err != nil {
		return err
	}
	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed handshake GET: %w", err)
	}
	if err := validateServerResponse(resp, secureWebsocketKey); err != nil {
		return err
	}

	// TODO(soypat): use net.Conn instead.
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		return fmt.Errorf("response body not an io.ReadWriteCloser: %T", resp.Body)
	}
	c.Tx.SetTxTransport(rwc)
	c.Rx.SetRxTransport(rwc)
	c.state.closeErr = nil
	c.state.closed = false
	return nil
}

// ReadNextFrame reads next frame in connection. Should be called in a loop
func (c *Client) ReadNextFrame() error {
	if c.state.IsClosed() {
		return c.Err()
	}
	c.txlock.Lock()
	err := c.state.ReplyOutstandingFrames(&c.Tx)
	c.txlock.Unlock()
	if err != nil {
		c.CloseConn(err)
		return err
	}
	_, err = c.Rx.ReadNextFrame()
	return err
}

// NextMessageReader returns a reader to a complete websocket message.
// This message may have been fragmented over the wire.
func (c *Client) NextMessageReader() (io.Reader, error) {
	return c.state.NextMessage()
}

// WriteMessage writes a binary message over the websocket connection.
func (c *Client) WriteMessage(payload []byte) error {
	c.txlock.Lock()
	defer c.txlock.Unlock()
	err := c.state.ReplyOutstandingFrames(&c.Tx)
	if err != nil {
		c.CloseConn(err)
		return err
	}
	_, err = c.Tx.writeMessage(c.entropy(), payload, false)
	return err
}

// WriteTextMessage writes a utf-8 message over the websocket connection.
func (c *Client) WriteTextMessage(payload []byte) error {
	c.txlock.Lock()
	defer c.txlock.Unlock()
	err := c.state.ReplyOutstandingFrames(&c.Tx)
	if err != nil {
		c.CloseConn(err)
		return err
	}
	_, err = c.Tx.writeMessage(c.entropy(), payload, true)
	return err
}

// Err returns the error that closed the connection.
// It only returns an error while the connection is closed.
// One can test if the server gracefully closed the connection with
//
//	errors.As(err, &peasocket.CloseErr{})
func (c *Client) Err() error {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.state.closeErr == errServerGracefulClose {
		return fmt.Errorf("server closed websocket: %w", c.state.makeCloseErr())
	}
	return c.state.closeErr
}

// makeRequest creates the HTTP handshake required to initiate a websocket connection
// with a server.
func (c *Client) makeRequest(ctx context.Context, secureWebsocketKey string, overwriteHeaders http.Header) (*http.Request, error) {
	u, err := url.Parse(c.serverURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return nil, fmt.Errorf("unexpected url scheme: %q", u.Scheme)
	}

	req := &http.Request{
		Method:     http.MethodGet,
		URL:        u,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Host:       u.Host,
	}
	req = req.WithContext(ctx)
	req.Header["Upgrade"] = []string{"websocket"}
	req.Header["Connection"] = []string{"Upgrade"}
	req.Header["Sec-WebSocket-Version"] = []string{"13"}
	for k, v := range overwriteHeaders {
		if len(v) == 0 {
			return nil, errors.New("overwrite header key " + k + " empty")
		}
		req.Header.Set(k, v[0])
		for _, v0 := range v[1:] {
			req.Header.Add(k, v0)
		}
	}
	req.Header["Sec-WebSocket-Key"] = []string{secureWebsocketKey}
	return req, nil
}

// CloseConn closes the underlying transport without sending websocket frames.
// If err is nil CloseConn will panic.
func (c *Client) CloseConn(err error) error {
	w := c.Tx.trp
	if c.state.IsClosed() {
		return errors.New("no websocket connection to close")
	}
	c.state.CloseConn(err)
	return w.Close()
}

// CloseWebsocket sends a close control frame over the websocket connection. Does
// not close the underlying transport.
func (c *Client) CloseWebsocket(status StatusCode, reason string) error {
	if c.state.IsClosed() {
		return errors.New("no websocket connection to close")
	}
	_, err := c.Tx.WriteClose(c.entropy(), status, []byte(reason))
	if err != nil {
		return err
	}
	c.state.CloseConn(errClientGracefulClose)
	return nil
}

func validateServerResponse(resp *http.Response, secureWebsocketKey string) error {
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("expected %v switching protocol http status, got %v", http.StatusSwitchingProtocols, resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Connection"), "Upgrade") || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!strings.EqualFold(resp.Header.Get("Sec-WebSocket-Accept"), serverProofOfReceipt(secureWebsocketKey)) {
		return fmt.Errorf(`invalid header field(s) "Connection:Upgrade", "Upgrade:websocket" or "Sec-WebSocket-Accept":<secure concatenated hash>, got %v`, resp.Header)
	}
	return nil
}

func defaultEntropy() uint32 {
	var output [4]byte
	_, err := io.ReadFull(rand.Reader, output[:])
	if err != nil {
		panic(err)
	}
	return binary.BigEndian.Uint32(output[:])
}

// On the Sec-WebSocket-Key header field, which the server uses to
// prove it received the handshake:
//
// For this header field, the server has to take the value (as present
// in the header field as sent by client, e.g., the base64-encoded [RFC4648] version minus
// any leading and trailing whitespace) and concatenate this with the
// Globally Unique Identifier (GUID, [RFC4122]) "258EAFA5-E914-47DA-
// 95CA-C5AB0DC85B11" in string form, which is unlikely to be used by
// network endpoints that do not understand the WebSocket Protocol.  A
// SHA-1 hash (160 bits) [FIPS.180-3], base64-encoded (see Section 4 of
// [RFC4648]), of this concatenation is then returned in the server's
// handshake.
var predefinedGUID = []byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11")

func serverProofOfReceipt(clientSecureWSKey string) string {
	clientSecureWSKey = strings.TrimSpace(clientSecureWSKey)
	h := sha1.New()
	h.Write([]byte(clientSecureWSKey))
	h.Write(predefinedGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (c *Client) secureKeyString() string {
	key := c.secureKey()
	return base64.StdEncoding.EncodeToString(key[:])
}

func (c *Client) secureKey() [16]byte {
	const u32size = unsafe.Sizeof(uint32(0))
	k32 := [4]uint32{c.entropy(), c.entropy(), c.entropy(), c.entropy()}
	return *(*[4 * u32size]byte)(unsafe.Pointer(&k32))
}

// clientState stores the persisting state of a websocket client connection.
// Since this state is shared between frames it is protected by a mutex so that
// the Client implementation is concurrent-safe.
type clientState struct {
	mu                 sync.Mutex
	rbuf               bytes.Buffer
	currentMessageSize uint64
	// unused as of yet.
	messageSizes       []uint64
	pendingPingOrClose []byte
	expectedPong       []byte
	closed             bool
	closeErr           error
}

// TODO(soypat): add this to callbacks.
func (state *clientState) callbacks() (RxCallbacks, TxCallbacks) {
	return RxCallbacks{
			OnMessage: func(rx *Rx, message io.Reader) error {
				state.mu.Lock()
				defer state.mu.Unlock()
				if state.closed {
					return state.closeErr
				}
				n, err := io.Copy(&state.rbuf, message)
				if err != nil {
					state.currentMessageSize = 0
					return err
				}
				state.currentMessageSize += uint64(n)
				if rx.LastReceivedHeader.Fin() {
					state.messageSizes = append(state.messageSizes, state.currentMessageSize)
					state.currentMessageSize = 0
				}
				return nil
			},
			OnCtl: func(rx *Rx, payload io.Reader) (err error) {
				op := rx.LastReceivedHeader.FrameType()
				state.mu.Lock()
				defer state.mu.Unlock()
				if state.closed {
					return state.closeErr
				}
				var n int
				switch op {
				case FramePing:
					n, err = io.ReadFull(payload, state.pendingPingOrClose[:MaxControlPayload])
					if err != nil {
						break
					}
					// Replaces pending ping with new ping.
					state.pendingPingOrClose = state.pendingPingOrClose[:n]

				case FramePong:
					n, err = io.ReadFull(payload, state.pendingPingOrClose[:MaxControlPayload])
					if err != nil {
						break
					}
					state.expectedPong = state.pendingPingOrClose[:n]

				case FrameClose:
					n, err = io.ReadFull(payload, state.pendingPingOrClose[:MaxControlPayload])
					if err != nil {
						break
					}
					state.closed = true
					state.closeErr = errServerGracefulClose
					// Replaces pending ping with new ping.
					state.pendingPingOrClose = state.pendingPingOrClose[:n]
				default:
					panic("unknown control FrameType") // This should be unreachable.
				}
				return err
			},
			OnError: func(rx *Rx, err error) {
				state.CloseConn(err)
			},
		}, TxCallbacks{
			OnError: func(tx *TxBuffered, err error) {
				state.CloseConn(err)
			},
		}
}

func (cs *clientState) PendingAction() bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return !cs.closed && cs.pendingPingOrClose != nil
}

func (cs *clientState) IsClosed() bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.closed
}

func (cs *clientState) GetServerClosedReason() (_ StatusCode, reason []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if !cs.closed || len(cs.pendingPingOrClose) < 2 {
		return 0, nil
	}
	sc := StatusCode(binary.BigEndian.Uint16(cs.pendingPingOrClose[:2]))
	return sc, cs.pendingPingOrClose[2:]
}

func (cs *clientState) Buffered() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.rbuf.Len()
}

func (cs *clientState) NextMessage() (io.Reader, error) {
	buffered := cs.Buffered()
	if buffered == 0 || len(cs.messageSizes) == 0 {
		return nil, errors.New("no messages in buffer")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	size := cs.messageSizes[0]
	cs.messageSizes = cs.messageSizes[1:]
	if len(cs.messageSizes) == 0 {
		cs.messageSizes = nil
	}
	var newbuf bytes.Buffer
	_, err := io.Copy(&newbuf, io.LimitReader(&cs.rbuf, int64(size)))
	if err != nil {
		panic(err) // should be unreachable.
	}
	return &newbuf, nil
}

func (state *clientState) CloseConn(err error) {
	if err == nil {
		panic("close error cannot be nil")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return
	}
	state.closed = true
	state.closeErr = err
}

func (state *clientState) ReplyOutstandingFrames(tx *TxBuffered) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed || len(state.pendingPingOrClose) == 0 {
		return nil // Nothing to do.
	}
	_, err := tx.WritePong(state.pendingPingOrClose)
	state.pendingPingOrClose = state.pendingPingOrClose[:0]
	if err != nil {
		err = fmt.Errorf("failed while responding pong to incoming ping: %w", err)
	}
	return err
}

func (state *clientState) makeCloseErr() *CloseError {
	if len(state.pendingPingOrClose) < 2 {
		return &CloseError{Status: StatusNoStatusRcvd}
	}
	return &CloseError{
		Status: StatusCode(binary.BigEndian.Uint16(state.pendingPingOrClose[:2])),
		Reason: state.pendingPingOrClose[2:],
	}
}

type CloseError struct {
	Status StatusCode
	Reason []byte
}

func (c *CloseError) Error() string {
	return c.Status.String() + ": " + string(c.Reason)
}
