//go:build !tinygo

package peasocket

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

type Server struct {
	rx    Rx
	tx    TxBuffered
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
	sv := &Server{
		state: connState{
			pendingPingOrClose: make([]byte, 0, MaxControlPayload),
			expectedPong:       make([]byte, 0, MaxControlPayload),
			closeErr:           errServerGracefulClose,
			copyBuf:            rxCopyBuf,
		},
	}
	return sv
}

func ListenAndServe(ctx context.Context, addr string, handler func(*Server)) error {
	addrport, err := netip.ParseAddrPort(addr)
	if err != nil {
		return err
	}
	listener, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(addrport))
	if err != nil {
		return err
	}
	defer listener.Close()
	if dl, ok := ctx.Deadline(); ok {
		err = listener.SetDeadline(dl)
		if err != nil {
			return err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := listener.AcceptTCP()
		if err != nil {
			continue
		}
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			conn.Close()
			continue
		}
		resp := http.Response{
			Status:     http.StatusText(200),
			StatusCode: 200,
			Proto:      httpProto,
			ProtoMajor: httpProtoMajor,
			ProtoMinor: httpProtoMinor,
			Request:    req,
		}
		if sc, err := validateClientUpgrade(req); err != nil {
			resp.StatusCode = sc
			resp.Status = err.Error()
			resp.Write(conn)
			conn.Close()
			continue
		}
		resp.Header, _ = makeWSHeader(nil)
		resp.Write(conn)
		go func() {

			sv := NewServer(make([]byte, 1024))
			sv.rx.SetRxTransport(conn)
			sv.tx.SetTxTransport(conn)
			sv.state.closeErr = nil
			defer conn.Close()
			handler(sv)
		}()
	}
}

// func (sv *Server) AcceptHTTP(w http.ResponseWriter, r *http.Request) error {
// 	if _, err := validateClientUpgrade(r); err != nil {
// 		return err
// 	}
// 	h, ok := w.(http.Hijacker)
// 	if !ok {
// 		return errors.New("response writer does not implement http.Hijacker interface")
// 	}
// 	_ = h
// 	return nil
// }

func validateClientUpgrade(r *http.Request) (int, error) {
	challenge := r.Header.Get("Sec-Websocket-Key")
	if challenge == "" || !strings.EqualFold(r.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		r.Header.Get("Sec-Websocket-Version") != "13" {
		return 500, fmt.Errorf(`invalid header field(s) "Sec-Websocket-Version:13" "Connection:Upgrade", "Upgrade:websocket" or "Sec-WebSocket-Accept":<secure concatenated hash>, got %v`, r.Header)
	}
	if !checkSameOrigin(r) {
		return 500, fmt.Errorf("request origin denied: Host=%q, Origin=%q", r.Host, r.Header.Get("Origin"))
	}
	return 200, nil
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
