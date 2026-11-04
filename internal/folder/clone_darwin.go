//go:build darwin

package folder

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func platformClone(src, dst *os.File) error { return unix.EOPNOTSUPP }
func cloneUnsupported(err error) bool {
	return errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS)
}

func platformCloneAt(src, parent *os.File, leaf string) (bool, error) {
	err := unix.Fclonefileat(int(src.Fd()), int(parent.Fd()), leaf, 0)
	return err == nil, err
}
