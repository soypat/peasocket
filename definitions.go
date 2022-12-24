package peasocket

const (
	// MaxControlPayload is the maximum length of a control frame payload.
	// See https://tools.ietf.org/html/rfc6455#section-5.5.
	MaxControlPayload = 125
	maxOpcode         = 0b1111
)

// Non control frame https://tools.ietf.org/html/rfc6455#section-11.8.
const (
	OpContinuation Opcode = iota
	OpText
	OpBinary
	// 3 - 7 are reserved for further non-control frames.
	_
	_
	_
	_
	_
	_numNCFrames
)

// Control frames https://tools.ietf.org/html/rfc6455#section-11.8.
const (
	OpClose Opcode = iota + _numNCFrames
	// A Ping frame may serve either as a keepalive or as a means to verify that
	// the remote endpoint is still responsive.
	// An endpoint MAY send a Ping frame any time after the connection is
	// established and before the connection is closed.
	OpPing
	// A Pong frame sent in response to a Ping frame must have identical
	// "Application data" as found in the message body of the Ping frame
	// being replied to.
	OpPong
	// 11-16 are reserved for further control frames.
)

// StatusCode represents a [WebSocket status code].
//
// [WebSocket status code]: https://tools.ietf.org/html/rfc6455#section-7.4
type StatusCode int

// These are only the [status codes defined by the protocol].
//
// You can define custom codes in the 3000-4999 range.
// The 3000-3999 range is reserved for use by libraries, frameworks and applications.
// The 4000-4999 range is reserved for private use.
//
// [status codes defined by the protocol]: https://www.iana.org/assignments/websocket/websocket.xhtml#close-code-number
const (
	StatusNormalClosure   StatusCode = 1000
	StatusGoingAway       StatusCode = 1001
	StatusProtocolError   StatusCode = 1002
	StatusUnsupportedData StatusCode = 1003
	statusReserved        StatusCode = 1004 // 1004 is reserved and so unexported.

	// StatusNoStatusRcvd cannot be sent in a close message.
	// It is reserved for when a close message is received without
	// a status code.
	StatusNoStatusRcvd StatusCode = 1005

	// StatusAbnormalClosure is exported for use only with Wasm.
	// In non Wasm Go, the returned error will indicate whether the
	// connection was closed abnormally.
	StatusAbnormalClosure StatusCode = 1006

	StatusInvalidFramePayloadData StatusCode = 1007
	StatusPolicyViolation         StatusCode = 1008
	StatusMessageTooBig           StatusCode = 1009
	StatusMandatoryExtension      StatusCode = 1010
	StatusInternalError           StatusCode = 1011
	StatusServiceRestart          StatusCode = 1012
	StatusTryAgainLater           StatusCode = 1013
	StatusBadGateway              StatusCode = 1014

	// StatusTLSHandshake is only exported for use with Wasm.
	// In non Wasm Go, the returned error will indicate whether there was
	// a TLS handshake failure.
	StatusTLSHandshake StatusCode = 1015
)

func (sc StatusCode) String() (s string) {
	switch sc {
	case StatusNormalClosure:
		s = "normal closure"
	case StatusGoingAway:
		s = "going away"
	case StatusProtocolError:
		s = "protocol error"
	case StatusUnsupportedData:
		s = "unsupported data"
	case statusReserved:
		s = "reserved"
	case StatusNoStatusRcvd:
		s = "no status received"
	case StatusAbnormalClosure:
		s = "abnormal closure"
	case StatusInvalidFramePayloadData:
		s = "invalid frame payload data"
	case StatusPolicyViolation:
		s = "policy violation"
	case StatusMessageTooBig:
		s = "message too big"
	case StatusMandatoryExtension:
		s = "mandatory extension"
	case StatusInternalError:
		s = "internal error"
	case StatusServiceRestart:
		s = "service restart"
	case StatusTryAgainLater:
		s = "try again later :)"
	case StatusBadGateway:
		s = "bad gateway"
	case StatusTLSHandshake:
		s = "TLS handshake"
	default:
		s = "unknown websocket status code"
	}
	return s
}