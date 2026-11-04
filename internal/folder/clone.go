package folder

import (
	"io"
	"os"
)

// cloneInto copies exactly size bytes and rejects a source whose length differs.
func cloneInto(src, dst *os.File, size int64) error {
	if size < 0 {
		return os.ErrInvalid
	}
	if err := platformClone(src, dst); err == nil {
		return nil
	} else if !cloneUnsupported(err) {
		return err
	}
	if _, err := src.Seek(0, 0); err != nil {
		return err
	}
	if err := dst.Truncate(0); err != nil {
		return err
	}
	if _, err := dst.Seek(0, 0); err != nil {
		return err
	}
	n, err := io.CopyN(dst, src, size)
	if err != nil {
		return err
	}
	if n != size {
		return io.ErrUnexpectedEOF
	}
	var extra [1]byte
	nExtra, err := src.Read(extra[:])
	if nExtra != 0 {
		return io.ErrShortWrite
	}
	if err != io.EOF {
		return err
	}
	return nil
}
