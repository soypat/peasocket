package peasocket

import (
	"bytes"
	"errors"
	"io"
)

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func encodeByte(w io.Writer, b byte) error {
	buf := [1]byte{b}
	n, err := w.Write(buf[:1])
	if err == nil && n == 0 {
		return errors.New("unexpected 0 bytes written to buffer and no error returned")
	}
	return err
}

func readFull(src io.Reader, dst []byte) (int, error) {
	n, err := src.Read(dst)
	if err == nil && n != len(dst) {
		var buffer [256]byte
		// TODO(soypat): Avoid heavy heap allocation by implementing lightweight algorithm here.
		i64, err := io.CopyBuffer(bytes.NewBuffer(dst[n:]), src, buffer[:])
		i := int(i64)
		if err != nil && errors.Is(err, io.EOF) && i == len(dst[n:]) {
			err = nil
		}
		return n + i, err
	}
	return n, err
}

func writeFull(dst io.Writer, src []byte) (int, error) {
	// dataPtr := 0
	n, err := dst.Write(src)
	if err == nil && n != len(src) {
		// TODO(soypat): Avoid heavy heap allocation by implementing lightweight algorithm here.
		var buffer [256]byte
		i, err := io.CopyBuffer(dst, bytes.NewBuffer(src[n:]), buffer[:])
		return n + int(i), err
	}
	return n, err
}
