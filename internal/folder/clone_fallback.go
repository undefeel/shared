//go:build !linux && !darwin

package folder

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func platformClone(src, dst *os.File) error { return unix.ENOTSUP }
func cloneUnsupported(err error) bool {
	return errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL)
}
