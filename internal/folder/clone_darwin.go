//go:build darwin

package folder

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// x/sys/unix does not expose a safe fd-to-existing-fd clone primitive on
// Darwin. Returning ENOTSUP selects the bounded copy from the already rooted fd.
func platformClone(src, dst *os.File) error { return unix.ENOTSUP }
func cloneUnsupported(err error) bool {
	return errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL)
}
