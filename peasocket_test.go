package peasocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math/bits"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestMask(t *testing.T) {
	for _, tc := range []struct {
		desc        string
		maskKey     uint32
		msg, expect string
	}{
		{desc: "zero message becomes mask", maskKey: 0xff00_ff00, msg: "\x00\x00\x00\x00", expect: "\xff\x00\xff\x00"},
		{desc: "zero mask no effect", maskKey: 0, msg: "\x00\xff\x00\xf0", expect: "\x00\xff\x00\xf0"},
		{desc: "proper xor", maskKey: 0xff16ff39, msg: "\xf0\xfa\xaa\x1e", expect: "\x0f\xec\x55\x27"},
		{desc: "fifth byte xor by last key byte", maskKey: 0xff16ff39, msg: "\xf0\xfa\xaa\x1e\xd8", expect: "\x0f\xec\x55\x27\x27"},
		{desc: "correct key rotation", maskKey: 0xff16ff39, msg: "\xf0\xfa\xaa\x1e\x1e\xfa", expect: "\x0f\xec\x55\x27\xe1\xec"},
	} {
		got := []byte(tc.msg)
		gotLE := []byte(tc.msg)
		le := tc.maskKey // binary.BigEndian.Uint32((*[4]byte)(unsafe.Pointer(&tc.maskKey))[:])
		expectKey := maskWS(tc.maskKey, got)
		_ = mask(le, gotLE)
		if string(got) != tc.expect {
			t.Errorf("%v: key=%#X\n\tmessage:  %q\n\texpected: %q\n\tgot:      %q", tc.desc, tc.maskKey, tc.msg, tc.expect, got)
		}
		if string(gotLE) != tc.expect {
			t.Errorf("%v: LE key=%#X\n\tmessage:  %q\n\texpected: %q\n\tgot:      %q", tc.desc, le, tc.msg, tc.expect, gotLE)
		}

		split1 := []byte(tc.msg[:len(tc.msg)/2+1])
		split2 := []byte(tc.msg[len(split1):])
		intermediateKey := maskWS(tc.maskKey, split1)
		finalKey := maskWS(intermediateKey, split2)
		if finalKey != expectKey {
			t.Error("split message key and full message keyt not match:", finalKey, expectKey)
		}
		got = append(split1, split2...)
		if string(got) != tc.expect {
			t.Errorf("2STEP %v: key=%#X\n\tmessage:  %q\n\texpected: %q\n\tgot:      %q", tc.desc, tc.maskKey, tc.msg, tc.expect, got)
		}
	}
}

func TestFragment(t *testing.T) {
	loopback := bytes.NewBuffer(nil)
	tx := TxBuffered{}
	rx := Rx{}
	tx.SetTxTransport(&closer{Writer: loopback})
	rx.SetRxTransport(io.NopCloser(loopback))
	// e := defaultEntropy()
	const message = "hello world!"
	buffer := make([]byte, 24)
	rd := strings.NewReader(message)
	n, err := tx.WriteFragmentedMessage(0, rd, buffer)
	wireLen := loopback.Len()
	t.Log(loopback.String())
	if err != nil {
		t.Fatal(err)
	} else if n != wireLen {
		t.Error("returned bytes written of WriteFragmentedMessage not match written to loopback", n, wireLen)
	}
	done := false
	var buf bytes.Buffer
	rx.RxCallbacks.OnMessage = func(rx *Rx, message io.Reader) error {
		done = rx.LastReceivedHeader.Fin()
		buf.ReadFrom(message)
		return nil
	}
	i := 0
	for !done {
		i++
		n, err = rx.ReadNextFrame()
		if err != nil || n != wireLen {
			t.Error("error reading frame", n, wireLen, err)
		}
		if i > 8 {
			t.Error("too many cycles, fin bit may have been incorrectly set")
			break
		}
	}
	got := buf.String()
	if got != message {
		t.Errorf("message garbled/incorrect after transferring with fragmentation\n\tgot:%q\n\twant:%q", got, message)
	}
}

func TestServerClientPingPong(t *testing.T) {
	const waitTimes = 1e7 * time.Millisecond
	svRxloopback := &transport{}
	svTxloopback := &transport{}
	sv := NewServer(make([]byte, 1024))
	cl := NewClient("abc", make([]byte, 1024), nil)
	sv.rx.trp = svRxloopback
	sv.tx.trp = svTxloopback
	sv.state.OnConnect()
	cl.rx.trp = svTxloopback
	cl.tx.trp = svRxloopback
	cl.state.OnConnect()
	mainCtx := context.Background()
	svPingMessage := []byte("hello, buenos dias")
	for i := 0; i < 5; i++ {
		doneSv := false
		go func() {
			ctx, cancel := context.WithTimeout(mainCtx, waitTimes)
			defer cancel()
			err := sv.Ping(ctx, svPingMessage)
			if err != nil {
				panic(err)
			}
			doneSv = true
		}()
		time.Sleep(50 * time.Millisecond)
		err := cl.HandleNextFrame()
		if err != nil {
			t.Fatal(err)
		}
		err = sv.HandleNextFrame()
		if err != nil {
			t.Error("reading next frame", err)
		}
		time.Sleep(50 * time.Millisecond)
		if sv.state.PendingAction() {
			t.Error("server pending action")
		}
		if !doneSv {
			t.Fatalf("server did not get response rx=%q tx=%q", svRxloopback.buf.String(), svTxloopback.buf.String())
		}
	}
}

// mask applies the WebSocket masking algorithm to b
// with the given key. Taken from nhooyr's implementation.
// See https://tools.ietf.org/html/rfc6455#section-5.3
//
// The returned value is the correctly rotated key to
// to continue to mask/unmask the message.
//
// It is optimized for LittleEndian architectures.
//
// See https://github.com/golang/go/issues/31586
func mask(key uint32, b []byte) uint32 {
	// Convert key to little endian:
	key = binary.BigEndian.Uint32((*[4]byte)(unsafe.Pointer(&key))[:])
	if len(b) >= 8 {
		key64 := uint64(key)<<32 | uint64(key)

		// At some point in the future we can clean these unrolled loops up.
		// See https://github.com/golang/go/issues/31586#issuecomment-487436401

		// Then we xor until b is less than 128 bytes.
		for len(b) >= 128 {
			v := binary.LittleEndian.Uint64(b)
			binary.LittleEndian.PutUint64(b, v^key64)
			v = binary.LittleEndian.Uint64(b[8:16])
			binary.LittleEndian.PutUint64(b[8:16], v^key64)
			v = binary.LittleEndian.Uint64(b[16:24])
			binary.LittleEndian.PutUint64(b[16:24], v^key64)
			v = binary.LittleEndian.Uint64(b[24:32])
			binary.LittleEndian.PutUint64(b[24:32], v^key64)
			v = binary.LittleEndian.Uint64(b[32:40])
			binary.LittleEndian.PutUint64(b[32:40], v^key64)
			v = binary.LittleEndian.Uint64(b[40:48])
			binary.LittleEndian.PutUint64(b[40:48], v^key64)
			v = binary.LittleEndian.Uint64(b[48:56])
			binary.LittleEndian.PutUint64(b[48:56], v^key64)
			v = binary.LittleEndian.Uint64(b[56:64])
			binary.LittleEndian.PutUint64(b[56:64], v^key64)
			v = binary.LittleEndian.Uint64(b[64:72])
			binary.LittleEndian.PutUint64(b[64:72], v^key64)
			v = binary.LittleEndian.Uint64(b[72:80])
			binary.LittleEndian.PutUint64(b[72:80], v^key64)
			v = binary.LittleEndian.Uint64(b[80:88])
			binary.LittleEndian.PutUint64(b[80:88], v^key64)
			v = binary.LittleEndian.Uint64(b[88:96])
			binary.LittleEndian.PutUint64(b[88:96], v^key64)
			v = binary.LittleEndian.Uint64(b[96:104])
			binary.LittleEndian.PutUint64(b[96:104], v^key64)
			v = binary.LittleEndian.Uint64(b[104:112])
			binary.LittleEndian.PutUint64(b[104:112], v^key64)
			v = binary.LittleEndian.Uint64(b[112:120])
			binary.LittleEndian.PutUint64(b[112:120], v^key64)
			v = binary.LittleEndian.Uint64(b[120:128])
			binary.LittleEndian.PutUint64(b[120:128], v^key64)
			b = b[128:]
		}

		// Then we xor until b is less than 64 bytes.
		for len(b) >= 64 {
			v := binary.LittleEndian.Uint64(b)
			binary.LittleEndian.PutUint64(b, v^key64)
			v = binary.LittleEndian.Uint64(b[8:16])
			binary.LittleEndian.PutUint64(b[8:16], v^key64)
			v = binary.LittleEndian.Uint64(b[16:24])
			binary.LittleEndian.PutUint64(b[16:24], v^key64)
			v = binary.LittleEndian.Uint64(b[24:32])
			binary.LittleEndian.PutUint64(b[24:32], v^key64)
			v = binary.LittleEndian.Uint64(b[32:40])
			binary.LittleEndian.PutUint64(b[32:40], v^key64)
			v = binary.LittleEndian.Uint64(b[40:48])
			binary.LittleEndian.PutUint64(b[40:48], v^key64)
			v = binary.LittleEndian.Uint64(b[48:56])
			binary.LittleEndian.PutUint64(b[48:56], v^key64)
			v = binary.LittleEndian.Uint64(b[56:64])
			binary.LittleEndian.PutUint64(b[56:64], v^key64)
			b = b[64:]
		}

		// Then we xor until b is less than 32 bytes.
		for len(b) >= 32 {
			v := binary.LittleEndian.Uint64(b)
			binary.LittleEndian.PutUint64(b, v^key64)
			v = binary.LittleEndian.Uint64(b[8:16])
			binary.LittleEndian.PutUint64(b[8:16], v^key64)
			v = binary.LittleEndian.Uint64(b[16:24])
			binary.LittleEndian.PutUint64(b[16:24], v^key64)
			v = binary.LittleEndian.Uint64(b[24:32])
			binary.LittleEndian.PutUint64(b[24:32], v^key64)
			b = b[32:]
		}

		// Then we xor until b is less than 16 bytes.
		for len(b) >= 16 {
			v := binary.LittleEndian.Uint64(b)
			binary.LittleEndian.PutUint64(b, v^key64)
			v = binary.LittleEndian.Uint64(b[8:16])
			binary.LittleEndian.PutUint64(b[8:16], v^key64)
			b = b[16:]
		}

		// Then we xor until b is less than 8 bytes.
		for len(b) >= 8 {
			v := binary.LittleEndian.Uint64(b)
			binary.LittleEndian.PutUint64(b, v^key64)
			b = b[8:]
		}
	}

	// Then we xor until b is less than 4 bytes.
	for len(b) >= 4 {
		v := binary.LittleEndian.Uint32(b)
		binary.LittleEndian.PutUint32(b, v^key)
		b = b[4:]
	}

	// xor remaining bytes.
	for i := range b {
		b[i] ^= byte(key)
		key = bits.RotateLeft32(key, -8)
	}
	// Convert key back to big endian.
	key = binary.BigEndian.Uint32((*[4]byte)(unsafe.Pointer(&key))[:])
	return key
}

type closer struct {
	io.Writer
	closed bool
}

func (c *closer) Close() error {
	c.closed = true
	return nil
}

type transport struct {
	buf    bytes.Buffer
	closed bool
}

func (c *transport) Close() error {
	c.closed = true
	return nil
}

func (c *transport) Read(b []byte) (int, error) {
	if c.closed {
		return 0, io.EOF
	}
	return c.buf.Read(b)
}

func (c *transport) Write(b []byte) (int, error) {
	if c.closed {
		return 0, io.EOF
	}
	return c.buf.Write(b)
}
