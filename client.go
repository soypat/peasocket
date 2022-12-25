package peasocket

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unsafe"
)

func NewClient(serverURL string) *Client {
	c := &Client{
		serverURL: serverURL,
	}
	return c
}

type Client struct {
	serverURL string
	entropy   func() uint32
	Rx        Rx
	Tx        TxBuffered
}

func (c *Client) DialHandshake(ctx context.Context, overwriteHeaders http.Header) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	u, err := url.Parse(c.serverURL)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return fmt.Errorf("unexpected url scheme: %q", u.Scheme)
	}
	if c.entropy == nil {
		c.entropy = defaultEntropy
	}

	client := http.DefaultClient
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	for k, v := range overwriteHeaders {
		if len(v) == 0 {
			return errors.New("overwrite header key " + k + " empty")
		}
		req.Header.Set(k, v[0])
		for _, v0 := range v[1:] {
			req.Header.Add(k, v0)
		}
	}
	entropyData := c.secureKey()
	secureWebsocketKey := base64.StdEncoding.EncodeToString(entropyData[:])
	req.Header.Set("Sec-WebSocket-Key", secureWebsocketKey)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed handshake GET: %w", err)
	}
	if err := validateServerResponse(resp, secureWebsocketKey); err != nil {
		return err
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		return fmt.Errorf("response body not an io.ReadWriteCloser: %T", resp.Body)
	}
	c.Tx = TxBuffered{
		trp: rwc,
	}
	c.Rx = Rx{
		trp: rwc,
	}
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
	_, err := readFull(rand.Reader, output[:])
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

func (c *Client) secureKey() [16]byte {
	const u32size = unsafe.Sizeof(uint32(0))
	k32 := [4]uint32{c.entropy(), c.entropy(), c.entropy(), c.entropy()}
	return *(*[4 * u32size]byte)(unsafe.Pointer(&k32))
}
