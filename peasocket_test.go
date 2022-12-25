package peasocket

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/bits"
	"testing"
)

// The go generate command below adds the fuzz corpus in the cache to git VCS.
//
//go:generate mv $(go env GOCACHE)/fuzz/github.com/soypat/peasocket/. testdata/fuzz/

func FuzzMaskedReader(f *testing.F) {
	testCases := []struct {
		data    []byte
		maskKey uint32
	}{
		{data: []byte("asd\x00ASd\xff\xf0"), maskKey: 0xa2312434},
		{data: []byte("asd\x00ASd\xff\xf0"), maskKey: 0},
	}
	for _, tc := range testCases {
		f.Add(tc.data, tc.maskKey)
	}
	f.Fuzz(func(t *testing.T, data []byte, maskKey uint32) {
		if len(data) == 0 {
			return
		}
		mr := maskedReader{
			R:       bytes.NewBuffer(data),
			MaskKey: maskKey,
		}
		expect := append([]byte{}, data...)
		expectKey := mask(maskKey, expect)
		got1 := make([]byte, len(data)/2+1)
		expect1 := expect[:len(got1)]
		n, err := mr.Read(got1)
		if err != nil || n != len(got1) {
			t.Fatal(err, n, len(got1))
		}
		if !bytes.Equal(expect1, got1) {
			t.Errorf("expected %q, got %q", expect1, got1)
		}

		// Proceed with next mask op
		got2 := make([]byte, len(expect)-len(got1))
		expect2 := expect[len(got1):]
		n, err = mr.Read(got2)
		if err != nil || n != len(got2) {
			t.Fatal(err, n, len(got2))
		}
		if !bytes.Equal(expect2, got2) {
			t.Errorf("second mask op expected %q, got %q", expect2, got2)
		}
		if expectKey != mr.MaskKey {
			t.Errorf("bad mask key, expect %v, got %v", expectKey, mr.MaskKey)
		}
	})
}

func FuzzLoopbackMessage(f *testing.F) {
	testCases := []struct {
		data    []byte
		maskKey uint32
	}{
		{data: []byte("asd\x00ASd\xff\xf0"), maskKey: 0xa2312434},
		{data: []byte("asd\x00ASd\xff\xf0"), maskKey: 0},
	}
	for _, tc := range testCases {
		f.Add(tc.data, tc.maskKey)
	}
	f.Fuzz(func(t *testing.T, data []byte, maskKey uint32) {
		if len(data) == 0 {
			return
		}
		datacp := append([]byte{}, data...)
		var loop bytes.Buffer
		tx := &TxBuffered{
			trp: &closer{Writer: &loop},
		}
		rx := Rx{
			trp: io.NopCloser(&loop),
		}
		written, err := tx.WriteMessage(maskKey, datacp)
		if err != nil {
			t.Fatal(err)
		}
		actualWritten := loop.Len()
		if written != actualWritten {
			t.Error("written bytes not match result of WriteMessage", written, loop.Len())
		}
		callbackCalled := false
		rx.RxCallbacks.OnPayload = func(rx *Rx, r io.Reader) error {
			callbackCalled = true
			b, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			pl := rx.LastReceivedHeader.PayloadLength
			if uint64(len(b)) != pl {
				t.Error("expected payload length not match read", len(b), pl)
			}
			if !bytes.Equal(b, data) {
				dataMasked := append([]byte{}, data...)
				maskWS(maskKey, dataMasked)
				t.Errorf("data loopback failed for %v\ngot:\n\t%q\nexpect:\n\t%q\nexpect masked:\n\t%q", rx.LastReceivedHeader.String(), b, data, dataMasked)
			}
			return nil
		}
		n, err := rx.ReadNextFrame()
		if err != nil {
			t.Error(err)
		}
		if !callbackCalled {
			t.Error("callback not called")
		}
		if n != written {
			t.Error("read bytes not match bytes written over loopback", n, actualWritten)
		}
	})
}

func TestLoopback(t *testing.T) {

}

type closer struct {
	io.Writer
	closed bool
}

func (c *closer) Close() error {
	c.closed = true
	return nil
}

// mask applies the WebSocket masking algorithm to p
// with the given key.
// See https://tools.ietf.org/html/rfc6455#section-5.3
//
// The returned value is the correctly rotated key to
// to continue to mask/unmask the message.
//
// It is optimized for LittleEndian and expects the key
// to be in little endian.
//
// See https://github.com/golang/go/issues/31586
func mask(key uint32, b []byte) uint32 {
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

	return key
}
