package folder

import (
	"errors"
	"io"
	"os"
)

// cloneInto creates an independent copy of exactly size bytes from src into dst.
// It never follows a pathname: callers provide already-open rooted descriptors.
func cloneInto(src, dst *os.File, size int64) error {
	return cloneWith(src, dst, size, platformClone)
}

func cloneWith(src, dst *os.File, size int64, clone func(*os.File, *os.File) error) error {
	if size < 0 {
		return errors.New("negative clone size")
	}
	before, err := src.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() != size {
		return errors.New("clone source size or type changed")
	}
	if err = dst.Truncate(0); err != nil {
		return err
	}
	if _, err = src.Seek(0, 0); err != nil {
		return err
	}
	if _, err = dst.Seek(0, 0); err != nil {
		return err
	}
	err = clone(src, dst)
	if err != nil {
		if !cloneUnsupported(err) {
			return err
		}
		if err = dst.Truncate(0); err != nil {
			return err
		}
		if _, err = src.Seek(0, 0); err != nil {
			return err
		}
		if _, err = dst.Seek(0, 0); err != nil {
			return err
		}
		n, e := io.CopyN(dst, src, size+1)
		if e != nil && !(e == io.EOF && n == size) {
			return e
		}
		if n != size {
			return errors.New("clone source size changed")
		}
	}
	after, err := src.Stat()
	if err != nil {
		return err
	}
	out, err := dst.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || out.Size() != size {
		return errors.New("clone source changed or output size mismatch")
	}
	return nil
}
