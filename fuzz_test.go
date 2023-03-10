package peasocket

import (
	"bytes"
	"io"
	"math"
	"testing"
)

// The go generate command below adds the fuzz corpus in the cache to git VCS.
//
//go:generate mv $(go env GOCACHE)/fuzz/github.com/soypat/peasocket/. testdata/fuzz/
func FuzzHeaderPut(f *testing.F) {
	testCases := []struct {
		payloadLen uint64
		maskKey    uint32
	}{
		{maskKey: 0, payloadLen: 20},
		{maskKey: 0xa2312434, payloadLen: math.MaxUint16 + 20},
		{maskKey: 0, payloadLen: math.MaxUint16 + 20},
		{maskKey: 0xa2312434, payloadLen: 20},
	}
	for _, tc := range testCases {
		f.Add(tc.payloadLen, tc.maskKey)
	}
	f.Fuzz(func(t *testing.T, payloadLen uint64, mask uint32) {
		if payloadLen == 0 {
			return
		}
		h := newHeader(FrameBinary, payloadLen, mask, false)
		var buf [MaxHeaderSize]byte
		n, err := h.Put(buf[:])
		if err != nil {
			panic(err)
		}
		hgot, n2, err := DecodeHeader(bytes.NewReader(buf[:n]))
		if err != nil {
			panic(err)
		}
		if n2 != n {
			panic("lengths not equal over wire")
		}
		if h != hgot {
			panic("encoding not match decoding")
		}
	})
}
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
	f.Fuzz(func(t *testing.T, dataCanon []byte, maskKey uint32) {
		if len(dataCanon) == 0 {
			return
		}
		trp := &transport{}
		loopBuf := &trp.buf
		datacp := append([]byte{}, dataCanon...)
		tx := &TxBuffered{
			trp: trp,
		}
		rx := Rx{
			trp: trp,
		}
		written, err := tx.WriteMessage(maskKey, datacp)
		if err != nil {
			t.Fatal(err)
		}
		actualWritten := loopBuf.Len()
		if written != actualWritten {
			t.Error("written bytes not match result of WriteMessage", written, loopBuf.Len())
		}
		callbackCalled := false
		rx.RxCallbacks.OnMessage = func(rx *Rx, r io.Reader) error {
			callbackCalled = true
			b, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			pl := rx.LastReceivedHeader.PayloadLength
			if uint64(len(b)) != pl {
				t.Error("expected payload length not match read", len(b), pl)
			}
			if !bytes.Equal(b, dataCanon) {
				dataMasked := append([]byte{}, dataCanon...)
				maskWS(maskKey, dataMasked)
				t.Errorf("data loopback failed for %v\ngot:\n\t%q\nexpect:\n\t%q\nexpect masked:\n\t%q", rx.LastReceivedHeader.String(), b, dataCanon, dataMasked)
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
