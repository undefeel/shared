package folder

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// platformFSOps is passed by value so fault injection never changes global state.
type platformFSOps struct {
	rename   func(int, string, int, string) error
	fsync    func(int) error
	fullSync func(uintptr) error
}

// renameNoReplace operates on pinned parents. Callers serialize commits and
// verify root/parent identity; this primitive does not acquire folder locks.
// It never falls back to a replacing rename, even on unsupported filesystems.
func renameNoReplace(from *os.File, source string, to *os.File, target string) error {
	return platformOps().renameNoReplace(from, source, to, target)
}

func (ops platformFSOps) renameNoReplace(from *os.File, source string, to *os.File, target string) error {
	for _, leaf := range []string{source, target} {
		if !diskName(leaf) || strings.Contains(leaf, "/") {
			return fmt.Errorf("invalid rename leaf %q", leaf)
		}
	}
	return ops.rename(int(from.Fd()), source, int(to.Fd()), target)
}

// syncFile flushes bytes and metadata on the held inode. Unsupported flushes
// are errors; callers must not publish a successful commit after failure.
func syncFile(f *os.File) error { return platformOps().syncFile(f) }

func (ops platformFSOps) syncFile(f *os.File) error {
	if err := ops.fsync(int(f.Fd())); err != nil {
		return err
	}
	if ops.fullSync != nil {
		return ops.fullSync(f.Fd())
	}
	return nil
}

// syncDir must be called for every changed directory, including new parents.
// Directory fsync is separate from the full hardware flush for durable files.
func syncDir(dir *os.File) error { return platformOps().syncDir(dir) }

func platformOps() platformFSOps {
	return platformFSOps{
		rename: func(from int, source string, to int, target string) error {
			return unix.Renameat2(from, source, to, target, unix.RENAME_NOREPLACE)
		},
		fsync: unix.Fsync,
	}
}

func (ops platformFSOps) syncDir(dir *os.File) error { return ops.fsync(int(dir.Fd())) }
