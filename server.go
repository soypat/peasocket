package peasocket

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type Server struct {
	c *Client
}

// NewClient creates a new Client ready to connect to a websocket enabled server.
// serverURL can start with wss:// or ws:// to enable TLS (secure websocket protocol).
// entropy is a function that returns randomized 32 bit integers from a high-entropy source.
// If entropy is nil then a default will be used.
func NewServer(rxCopyBuf []byte) *Server {
	if len(rxCopyBuf) == 0 {
		rxCopyBuf = make([]byte, 32*1024)
	}
	c := NewClient("", rxCopyBuf, defaultEntropy)
	return &Server{c: c}
}

func (sv *Server) Accept(w http.ResponseWriter, r *http.Request) error {
	panic("not implemented yet")
	if err := validateClientUpgrade(r); err != nil {
		return err
	}
	h, ok := w.(http.Hijacker)
	if !ok {
		return errors.New("response writer does not implement http.Hijacker interface")
	}
	_ = h
	return nil
}

func validateClientUpgrade(r *http.Request) error {
	challenge := r.Header.Get("Sec-Websocket-Key")
	if challenge == "" || !strings.EqualFold(r.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		r.Header.Get("Sec-Websocket-Version") != "13" {
		return fmt.Errorf(`invalid header field(s) "Sec-Websocket-Version:13" "Connection:Upgrade", "Upgrade:websocket" or "Sec-WebSocket-Accept":<secure concatenated hash>, got %v`, r.Header)
	}
	if !checkSameOrigin(r) {
		return fmt.Errorf("request origin denied: Host=%q, Origin=%q", r.Host, r.Header.Get("Origin"))
	}
	return nil
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
