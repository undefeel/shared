package folder

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// rootFS owns its root descriptor. Callers must drain operations before Close.
// Returned files (including parent handles) belong to the caller. Disk paths may
// be NFD; canonical peer-name validation belongs at the protocol boundary.
type rootFS struct{ root *os.File }

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

func openRootFS(name string) (*rootFS, error) {
	fd, err := unix.Open(filepath.Clean(name), dirFlags, 0)
	if err != nil {
		return nil, err
	}
	return &rootFS{os.NewFile(uintptr(fd), name)}, nil
}

func (r *rootFS) Close() error { return r.root.Close() }

func diskName(name string) bool {
	return name != "" && name != "." && !path.IsAbs(name) && path.Clean(name) == name &&
		!strings.ContainsAny(name, "\\\x00") && name != ".." && !strings.HasPrefix(name, "../")
}

// openDir walks one component at a time, never following a symlink. Opening
// "." creates an independent directory stream, unlike dup(2).
func (r *rootFS) openDir(name string, create bool) (*os.File, error) {
	if name != "." && !diskName(name) {
		return nil, fmt.Errorf("invalid disk path %q", name)
	}
	fd, err := unix.Openat(int(r.root.Fd()), ".", dirFlags, 0)
	if err != nil {
		return nil, err
	}
	if name != "." {
		for _, part := range strings.Split(name, "/") {
			next, e := unix.Openat(fd, part, dirFlags, 0)
			if e == unix.ENOENT && create {
				e = unix.Mkdirat(fd, part, 0o755)
				if e == nil || e == unix.EEXIST {
					next, e = unix.Openat(fd, part, dirFlags, 0)
				}
			}
			unix.Close(fd)
			if e != nil {
				return nil, e
			}
			fd = next
		}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// parent pins the directory for subsequent *at operations. A later replacement
// of its pathname cannot redirect an operation into a symlink target.
func (r *rootFS) parent(name string, create bool) (*os.File, string, error) {
	if !diskName(name) {
		return nil, "", fmt.Errorf("invalid disk path %q", name)
	}
	f, err := r.openDir(path.Dir(name), create)
	return f, path.Base(name), err
}

func openRegularAt(parent *os.File, leaf string, flags int, mode uint32) (*os.File, error) {
	// Only a single component is permitted, even for callers holding a parent fd.
	if !diskName(leaf) || strings.Contains(leaf, "/") {
		return nil, fmt.Errorf("invalid leaf %q", leaf)
	}
	fd, err := unix.Openat(int(parent.Fd()), leaf, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), leaf)
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s: unsupported file type %s", leaf, st.Mode())
	}
	return f, nil
}

func (r *rootFS) openRead(name string) (*os.File, error) {
	p, leaf, err := r.parent(name, false)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return openRegularAt(p, leaf, unix.O_RDONLY, 0)
}

// create exclusively reserves a new file. Existing objects are never truncated.
func (r *rootFS) create(name string) (*os.File, error) {
	p, leaf, err := r.parent(name, true)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return openRegularAt(p, leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
}

// unlink removes only one entry, or an empty directory; never a subtree.
func (r *rootFS) unlink(name string, directory bool) error {
	p, leaf, err := r.parent(name, false)
	if err != nil {
		return err
	}
	defer p.Close()
	flags := 0
	if directory {
		flags = unix.AT_REMOVEDIR
	}
	return unix.Unlinkat(int(p.Fd()), leaf, flags)
}
