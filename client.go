package peasocket

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
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
	errUnexpectedPong      = errors.New("unexpected pong")
)

// Client is a client websocket implementation.
//
// Users should use Client in no more than two goroutines unless calling the
// explicitly stated as concurrency safe methods i.e: Err, IsConnected.
// These two goroutines should be separated by responsability:
//   - The Read goroutine should call methods that read received messages
//   - Write goroutine should call methods that write to the connection.
//
// Client automatically takes care of incoming Ping and Close frames during ReadNextFrame
type Client struct {
	// Tx mutex.
	muTx      sync.Mutex
	tx        TxBuffered
	state     connState
	rx        Rx
	ServerURL string
	entropy   func() uint32
}

// NewClient creates a new Client ready to connect to a websocket enabled server.
// serverURL can start with wss:// or ws:// to enable TLS (secure websocket protocol).
// entropy is a function that returns randomized 32 bit integers from a high-entropy source.
// userBuffer will be used as scratch space to copy messages.
//
// If entropy is nil then a default will be used.
// If userBuffer is zero-lengthed one will be automatically allocated.
func NewClient(serverURL string, userBuffer []byte, entropy func() uint32) *Client {
	if entropy == nil {
		entropy = defaultEntropy
	}
	L := len(userBuffer)
	if userBuffer == nil {
		userBuffer = make([]byte, 32*1024)
	} else if L < MaxControlPayload*3 {
		panic("user buffer length too short, must be at least 3*125")
	}
	ctl1 := userBuffer[0:0:MaxControlPayload]
	ctl2 := userBuffer[MaxControlPayload : MaxControlPayload : 2*MaxControlPayload]
	c := &Client{
		ServerURL: serverURL,
		entropy:   entropy,
		state: connState{
			pendingPingOrClose: ctl1,
			expectedPong:       ctl2,
			closeErr:           errClientGracefulClose,
			copyBuf:            userBuffer[2*MaxControlPayload:],
		},
	}
	rxc, txc := c.state.callbacks(true)
	c.rx.RxCallbacks = rxc
	c.tx.TxCallbacks = txc
	return c
}

// Dial not yet tested. Performs websocket handshake over net.Conn.
func (c *Client) Dial(ctx context.Context, overwriteHeaders http.Header) error {
	if !c.state.IsClosed() {
		return errors.New("must first close existing connection before starting a new one")
	}

	remoteAddr, err := net.ResolveTCPAddr("tcp", strings.TrimPrefix(c.ServerURL, "ws://"))
	if err != nil {
		return err
	}
	conn, err := net.DialTCP("tcp", nil, remoteAddr)
	if err != nil {
		return err
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
	resp, err := http.ReadResponse(bufio.NewReaderSize(conn, 256), req)
	if err != nil {
		return err
	}
	if err := validateServerResponse(resp, secureWebsocketKey); err != nil {
		return err
	}
	c.tx.SetTxTransport(conn)
	c.rx.SetRxTransport(conn)
	c.state.closeErr = nil
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
	c.tx.SetTxTransport(rwc)
	c.rx.SetRxTransport(rwc)
	c.state.OnConnect()
	return nil
}

// IsConnected returns true if the underlying transport of the websocket is ready to send/receive messages.
// It is shorthand for c.Err() == nil.
//
// IsConnected is safe for concurrent use.
func (c *Client) IsConnected() bool { return c.state.IsConnected() }

// Err returns the error that closed the connection.
// It only returns an error while the connection is closed.
// One can test if the server gracefully closed the connection with
//
//	errors.As(err, &peasocket.CloseErr{})
//
// Err is safe for concurrent use.
func (c *Client) Err() error {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.state.closeErr == errServerGracefulClose {
		return fmt.Errorf("server closed websocket: %w", c.state.makeCloseErr())
	}
	return c.state.closeErr
}

// BufferedMessages returns the amount of received messages N that are ready to be read
// such that the user is guaranteed to not get an error calling NextMessageReader N times.
//
// BufferedMessages is safe for concurrent use.
func (c *Client) BufferedMessages() int {
	return c.state.BufferedMessages()
}

// ReadNextFrame reads next frame in connection. Should be called in a loop
func (c *Client) ReadNextFrame() error {
	if !c.IsConnected() {
		return c.Err()
	}
	_, err := c.rx.ReadNextFrame()
	if err != nil {
		return err
	}
	c.muTx.Lock()
	err = c.state.ReplyOutstandingFrames(c.entropy(), &c.tx)
	c.muTx.Unlock()
	if err != nil {
		c.CloseConn(err)
		return err
	}
	return nil
}

// NextMessageReader returns a reader to a complete websocket message. It copies
// the whole contents of the received message to a new buffer which is returned
// to the caller.
// The returned message may have been fragmented over the wire.
func (c *Client) NextMessageReader() (io.Reader, FrameType, error) {
	return c.state.NextMessage()
}

// WriteNextMessageTo writes the next received message in the queue to the argument writer.
func (c *Client) WriteNextMessageTo(w io.Writer) (FrameType, int64, error) {
	return c.state.WriteNextMessageTo(w)
}

// WriteFragmentedMessage writes contents of r over the wire using the userBuffer
// as scratch memory. This function does not allocate memory unless userBuffer is nil.
func (c *Client) WriteFragmentedMessage(r io.Reader, userBuffer []byte) (int, error) {
	c.muTx.Lock()
	defer c.muTx.Unlock()
	err := c.state.ReplyOutstandingFrames(c.entropy(), &c.tx)
	if err != nil {
		c.CloseConn(err)
		return 0, err
	}
	return c.tx.WriteFragmentedMessage(c.entropy(), r, userBuffer)
}

// WriteMessage writes a binary message over the websocket connection.
func (c *Client) WriteMessage(payload []byte) error {
	c.muTx.Lock()
	defer c.muTx.Unlock()
	err := c.state.ReplyOutstandingFrames(c.entropy(), &c.tx)
	if err != nil {
		c.CloseConn(err)
		return err
	}
	_, err = c.tx.writeMessage(c.entropy(), payload, false)
	return err
}

// WriteTextMessage writes a utf-8 message over the websocket connection.
func (c *Client) WriteTextMessage(payload []byte) error {
	c.muTx.Lock()
	defer c.muTx.Unlock()
	err := c.state.ReplyOutstandingFrames(c.entropy(), &c.tx)
	if err != nil {
		c.CloseConn(err)
		return err
	}
	_, err = c.tx.writeMessage(c.entropy(), payload, true)
	return err
}

// makeRequest creates the HTTP handshake required to initiate a websocket connection
// with a server.
func (c *Client) makeRequest(ctx context.Context, secureWebsocketKey string, overwriteHeaders http.Header) (*http.Request, error) {
	u, err := url.Parse(c.ServerURL)
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
		Proto:      httpProto,
		ProtoMajor: httpProtoMajor,
		ProtoMinor: httpProtoMinor,
		Header:     make(http.Header),
		Host:       u.Host,
	}
	req = req.WithContext(ctx)
	hd, err := makeWSHeader(overwriteHeaders)
	if err != nil {
		return nil, err
	}
	hd["Sec-WebSocket-Key"] = []string{secureWebsocketKey}
	req.Header = hd
	return req, nil
}

// CloseConn closes the underlying transport without sending websocket frames.
//
// If err is nil CloseConn will panic.
func (c *Client) CloseConn(err error) error {
	w := c.tx.trp
	r := c.rx.trp
	if w != nil && r != nil {
		c.state.CloseConn(err)
		w.Close()
		r.Close()
		return nil
	}
	return errors.New("transport(s) nil, will not attempt to close. this may be a bug in peasocket")
}

// CloseWebsocket sends a close control frame over the websocket connection. Does
// not attempt to close the underlying transport.
// If CloseWebsocket received a [*CloseErr] type no memory allocation is performed and it's
// status code and reason is used in the websocket close frame.
//
// If err is nil CloseWebsocket panics.
func (c *Client) CloseWebsocket(err error) error {
	c.muTx.Lock()
	defer c.muTx.Unlock()
	return c.state.CloseWebsocket(err, c.entropy(), &c.tx)
}

func validateServerResponse(resp *http.Response, secureWebsocketKey string) error {
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("expected %v switching protocol http status, got %v", http.StatusSwitchingProtocols, resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Connection"), "Upgrade") || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!strings.EqualFold(resp.Header.Get("Sec-WebSocket-Accept"), serverProofOfReceipt(secureWebsocketKey)) {
		// resp.Header.Get("Sec-Websocket-Version") != "13" TODO(soypat): Add this once done testing since Nhooyr's library sometimes does not set this field.
		return fmt.Errorf(`invalid header field(s) "Sec-Websocket-Version:13", "Connection:Upgrade", "Upgrade:websocket" or "Sec-WebSocket-Accept":<secure concatenated hash>, got %v`, resp.Header)
	}
	return nil
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

func makeWSHeader(overwriteHeaders http.Header) (http.Header, error) {
	header := make(http.Header)
	header["Upgrade"] = []string{"websocket"}
	header["Connection"] = []string{"Upgrade"}
	header["Sec-WebSocket-Version"] = []string{"13"}
	for k, v := range overwriteHeaders {
		if len(v) == 0 {
			return nil, errors.New("overwrite header key " + k + " empty")
		}
		header.Set(k, v[0])
		for _, v0 := range v[1:] {
			header.Add(k, v0)
		}
	}
	return header, nil
}
