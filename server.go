//go:build !tinygo

package peasocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

var serverMaxBackoff = 500 * time.Millisecond

// Server is a stateful websocket single-connection server implementation.
// It is only partly safe for concurrent use.
//
// Users should use Server in no more than two goroutines unless calling the
// explicitly stated as concurrency safe methods i.e: Err, IsConnected.
// These two goroutines should be separated by responsability: The Read goroutine
// should call methods that read received messages and the Write goroutine should
// call methods that write to the connection.
//
// Server automatically takes care of incoming Ping and Close frames during ReadNextFrame
type Server struct {
	// Tx mutex.
	muTx  sync.Mutex
	tx    TxBuffered
	rx    Rx
	state connState
}

// NewClient creates a new Client ready to connect to a websocket enabled server.
// serverURL can start with wss:// or ws:// to enable TLS (secure websocket protocol).
// entropy is a function that returns randomized 32 bit integers from a high-entropy source.
// If entropy is nil then a default will be used.
func NewServer(rxCopyBuf []byte) *Server {
	if len(rxCopyBuf) == 0 {
		rxCopyBuf = make([]byte, 32*1024)
	}
	L := len(rxCopyBuf)
	if rxCopyBuf == nil {
		rxCopyBuf = make([]byte, 32*1024)
	} else if L < MaxControlPayload*3 {
		panic("user buffer length too short, must be at least 3*125")
	}
	ctl1 := rxCopyBuf[0:0:MaxControlPayload]
	ctl2 := rxCopyBuf[MaxControlPayload : MaxControlPayload : 2*MaxControlPayload]
	sv := &Server{
		state: connState{
			pendingPingOrClose: ctl1,
			expectedPong:       ctl2,
			closeErr:           errServerGracefulClose,
			copyBuf:            rxCopyBuf[2*MaxControlPayload:],
		},
	}
	sv.rx.RxCallbacks, sv.tx.TxCallbacks = sv.state.callbacks(false)
	return sv
}

// ReadNextFrame reads next frame in connection and takes care of
// incoming requests. Should be called in a loop separate from write goroutines.
func (sv *Server) ReadNextFrame() error {
	if !sv.IsConnected() {
		return sv.Err()
	}
	_, err := sv.rx.ReadNextFrame()
	if err != nil {
		return err
	}
	sv.muTx.Lock()
	err = sv.state.ReplyOutstandingFrames(0, &sv.tx)
	sv.muTx.Unlock()
	if err != nil {
		sv.CloseConn(err)
		return err
	}
	return nil
}

// ListenAndServe is the websocket counterpart to http.ListenAndServe. It sets up a
// tcp.Listener on the argument address and begins listening for incoming websocket
// client connections. On a succesful webosocket connection the handler is called
// with a newly allocated Server instance.
func ListenAndServe(mainCtx context.Context, address string, handler func(ctx context.Context, sv *Server)) error {
	var cancel func()
	mainCtx, cancel = context.WithCancel(mainCtx)
	defer cancel()
	addrport, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	listener, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(addrport))
	if err != nil {
		return err
	}
	defer listener.Close()
	if dl, ok := mainCtx.Deadline(); ok {
		err = listener.SetDeadline(dl)
		if err != nil {
			return err
		}
	}
	var sendBuf bytes.Buffer
	for {
		if err := mainCtx.Err(); err != nil {
			return err
		}
		conn, err := listener.AcceptTCP()
		if err != nil {
			return err
		}
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			conn.Close()
			continue
		}
		resp := http.Response{
			Status:     http.StatusText(http.StatusSwitchingProtocols),
			StatusCode: http.StatusSwitchingProtocols,
			Proto:      httpProto,
			ProtoMajor: httpProtoMajor,
			ProtoMinor: httpProtoMinor,
			Request:    req,
		}
		challengeAccept, sc, err := validateClientUpgrade(req)
		resp.StatusCode = sc
		if err != nil {
			resp.Status = http.StatusText(sc)
			resp.Write(conn)
			conn.Close()
			continue
		}
		resp.Header, _ = makeWSHeader(nil)
		resp.Header["Sec-WebSocket-Accept"] = []string{challengeAccept}
		resp.Write(&sendBuf)
		_, err = sendBuf.WriteTo(conn)
		sendBuf.Reset()
		if err != nil {
			conn.Close()
			continue
		}
		go func() {
			ctx, cancel := context.WithCancel(mainCtx)
			defer cancel()
			sv := NewServer(make([]byte, 1024))
			sv.rx.SetRxTransport(conn)
			sv.tx.SetTxTransport(conn)
			sv.state.OnConnect()
			defer conn.Close()
			log.Println("start handler")
			handler(ctx, sv)
			log.Println("end handler")
		}()
	}
}

// Ping blocks until a call to ReadNextFrame receives the corresponding Pong message.
// Should be called in a goroutine separate to the Server Read goroutine.
func (sv *Server) Ping(ctx context.Context, message []byte) error {
	if !sv.IsConnected() {
		return sv.Err()
	}
	sv.muTx.Lock()

	err := sv.state.ReplyOutstandingFrames(0, &sv.tx)
	if err != nil {
		sv.muTx.Unlock()
		return err
	}
	sv.state.mu.Lock()
	sv.state.expectedPong = message
	sv.state.mu.Unlock()
	_, err = sv.tx.WritePing(0, message)
	sv.muTx.Unlock()
	if err != nil {
		return err
	}
	backoff := ExponentialBackoff{MaxWait: serverMaxBackoff}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sv.state.mu.Lock()
		if sv.state.expectedPong == nil {
			sv.state.mu.Unlock()
			break
		}
		sv.state.mu.Unlock()
		backoff.Miss()
	}
	return nil
}

// Err returns the error that closed the connection.
// It only returns an error while the connection is closed.
// One can test if the client gracefully closed the connection with
//
//	errors.As(err, &peasocket.CloseErr{}) // true on graceful websocket closure
//
// Err is safe for concurrent use.
func (sv *Server) Err() error {
	sv.state.mu.Lock()
	defer sv.state.mu.Unlock()
	if sv.state.closeErr == errClientGracefulClose {
		return fmt.Errorf("client closed websocket: %w", sv.state.makeCloseErr())
	}
	return sv.state.closeErr
}

// CloseConn closes the underlying transport without sending websocket frames.
//
// If err is nil CloseConn will panic.
func (sv *Server) CloseConn(err error) error {
	w := sv.tx.trp
	r := sv.rx.trp
	if w != nil && r != nil {
		sv.state.CloseConn(err)
		w.Close()
		r.Close()
		return nil
	}
	return errors.New("transport(s) nil, will not attempt to close. this may be a bug in peasocket")
}

// IsConnected returns true if the underlying transport of the websocket is ready to send/receive messages.
// It is shorthand for s.Err() == nil.
//
// IsConnected is safe for concurrent use.
func (sv *Server) IsConnected() bool { return sv.state.IsConnected() }

// CloseWebsocket sends a close control frame over the websocket connection. Does
// not attempt to close the underlying transport.
// If CloseWebsocket receives a [*CloseErr] type no memory allocation is performed and it's
// status code and reason is used in the websocket close frame.
//
// If err is nil CloseWebsocket panics.
func (sv *Server) CloseWebsocket(err error) error {
	sv.muTx.Lock()
	defer sv.muTx.Unlock()
	return sv.state.CloseWebsocket(err, 0, &sv.tx)
}

// WriteNextMessageTo writes entire contents the next message in queue
// to the passed in writer.
func (sv *Server) WriteNextMessageTo(w io.Writer) (FrameType, int64, error) {
	return sv.state.WriteNextMessageTo(w)
}

// WriteFragmentedMessage writes contents of r over the wire using the userBuffer
// as scratch memory. This function does not allocate memory unless userBuffer is nil.
func (c *Server) WriteFragmentedMessage(r io.Reader, userBuffer []byte) (int, error) {
	c.muTx.Lock()
	defer c.muTx.Unlock()
	err := c.state.ReplyOutstandingFrames(0, &c.tx)
	if err != nil {
		c.CloseConn(err)
		return 0, err
	}
	return c.tx.WriteFragmentedMessage(0, r, userBuffer)
}

func validateClientUpgrade(r *http.Request) (string, int, error) {
	challenge := r.Header.Get("Sec-Websocket-Key")
	if challenge == "" || !strings.EqualFold(r.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		r.Header.Get("Sec-Websocket-Version") != "13" {
		return "", http.StatusBadRequest, fmt.Errorf(`invalid header field(s) "Sec-Websocket-Version:13" "Connection:Upgrade", "Upgrade:websocket" or "Sec-WebSocket-Accept":<secure concatenated hash>, got %v`, r.Header)
	}
	if !checkSameOrigin(r) {
		return "", http.StatusBadRequest, fmt.Errorf("request origin denied: Host=%q, Origin=%q", r.Host, r.Header.Get("Origin"))
	}
	return serverProofOfReceipt(challenge), http.StatusSwitchingProtocols, nil
}

func checkSameOrigin(r *http.Request) bool {
	origin := r.Header["Origin"]
	if len(origin) == 0 {
		return true
	}
	u, err := url.Parse(origin[0])
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// ENABLE_DEBUG_LOG will NOT stay in this package. It is a temporary
// helper that is why the name is the way it is. Calling this function with enable
// set to true without a enable=false call in between will cause duplicated log output.
//
// Deprecated: ENABLE_DEBUG_LOG is deprecated. Will not stay in package peasocket.
func (sv *Server) ENABLE_DEBUG_LOG(enable bool) {
	sv.state.mu.Lock()
	defer sv.state.mu.Unlock()
	if enable {
		txOnError := sv.tx.TxCallbacks.OnError
		sv.tx.TxCallbacks.OnError = func(tx *TxBuffered, err error) {
			log.Printf("Tx.OnError(%s): %q", err, tx.buf.String())
			txOnError(tx, err)
		}
		rxOnError := sv.rx.RxCallbacks.OnError
		sv.rx.RxCallbacks.OnError = func(rx *Rx, err error) {
			log.Printf("Rx.OnError %s: %s", rx.LastReceivedHeader.String(), err)
			rxOnError(rx, err)
		}
		rxOnCtl := sv.rx.RxCallbacks.OnCtl
		sv.rx.RxCallbacks.OnCtl = func(rx *Rx, payload io.Reader) error {
			b, _ := io.ReadAll(payload)
			log.Printf("Rx.OnCtl:%s: %q", rx.LastReceivedHeader.String(), string(b))
			return rxOnCtl(rx, bytes.NewReader(b))
		}
		rxOnMsg := sv.rx.RxCallbacks.OnMessage
		sv.rx.RxCallbacks.OnMessage = func(rx *Rx, message io.Reader) error {
			b, _ := io.ReadAll(message)
			log.Printf("Rx.OnMsg:%s: %q", rx.LastReceivedHeader.String(), string(b))
			return rxOnMsg(rx, bytes.NewReader(b))
		}
	} else {
		sv.rx.RxCallbacks, sv.tx.TxCallbacks = sv.state.callbacks(false)
	}
}
