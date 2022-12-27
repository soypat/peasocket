package peasocket

import (
	"errors"
	"fmt"
	"io"
)

// b2u8 converts a bool to an uint8. true==1, false==0.
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

// writeFull is a convenience function to perform dst.Write and detect
// bad writer implementations while at it.
func writeFull(dst io.Writer, src []byte) (int, error) {
	n, err := dst.Write(src)
	if n != len(src) && err == nil {
		err = fmt.Errorf("bad writer implementation %T", dst)
	}
	return n, err
}
