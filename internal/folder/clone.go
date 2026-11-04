package folder

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
)

// cloneInto copies exactly size bytes and rejects a source whose length differs.
func cloneInto(src, dst *os.File, size int64) error {
	return cloneIntoWith(src, dst, size, platformClone)
}

func cloneIntoWith(src, dst *os.File, size int64, clone func(*os.File, *os.File) error) error {
	if size < 0 {
		return os.ErrInvalid
	}
	if err := clone(src, dst); err == nil {
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

// cloneFile creates only an exclusive leaf beneath a pinned, no-follow parent.
func (r *rootFS) cloneFile(src *os.File, name string, size int64) (_ *os.File, err error) {
	p, leaf, err := r.parent(name, true)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	before, err := src.Stat()
	if err != nil {
		return nil, err
	}
	if size < 0 || before.Size() != size {
		return nil, errCloneChanged
	}
	created, err := platformCloneAt(src, p, leaf)
	if err != nil && !cloneUnsupported(err) {
		return nil, err
	}
	var dst *os.File
	if created {
		dst, err = openRegularAt(p, leaf, unix.O_RDWR, 0)
	} else {
		dst, err = openRegularAt(p, leaf, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			dst.Close()
			_ = unix.Unlinkat(int(p.Fd()), leaf, 0)
		}
	}()
	if !created {
		if err = cloneInto(src, dst, size); err != nil {
			return nil, err
		}
	}
	after, e := src.Stat()
	if e != nil {
		return nil, e
	}
	if scanStatFromInfo(before) != scanStatFromInfo(after) {
		return nil, errCloneChanged
	}
	if err = dst.Chmod(0600); err != nil {
		return nil, err
	}
	return dst, nil
}

var errCloneChanged = errors.New("clone source changed")
