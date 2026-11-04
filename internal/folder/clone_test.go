package folder

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCloneIndependentRooted(t *testing.T) {
	dir := t.TempDir()
	r, e := openRootFS(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if e = os.WriteFile(filepath.Join(dir, "source"), []byte("hello"), 0600); e != nil {
		t.Fatal(e)
	}
	src, e := r.openRead("source")
	if e != nil {
		t.Fatal(e)
	}
	defer src.Close()
	if e = os.Chmod(filepath.Join(dir, "source"), 0400); e != nil {
		t.Fatal(e)
	}
	dst, e := r.cloneFile(src, "copy", 5)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := src.Stat()
	b, _ := dst.Stat()
	if os.SameFile(a, b) {
		t.Fatal("hardlink")
	}
	if _, e = dst.WriteAt([]byte("X"), 0); e != nil {
		t.Fatal(e)
	}
	dst.Close()
	body, _ := os.ReadFile(filepath.Join(dir, "source"))
	if string(body) != "hello" {
		t.Fatal("source mutated")
	}
	if _, e = r.cloneFile(src, "copy", 5); e == nil {
		t.Fatal("replaced existing target")
	}
	os.Symlink(dir, filepath.Join(dir, "link"))
	if _, e = r.cloneFile(src, "link/escape", 5); e == nil {
		t.Fatal("followed symlink parent")
	}
	if _, e = r.cloneFile(src, "wrong-size", 4); e == nil {
		t.Fatal("accepted wrong size")
	}
}

func TestCloneFallbackAndIOErrors(t *testing.T) {
	src, e := os.CreateTemp(t.TempDir(), "src")
	if e != nil {
		t.Fatal(e)
	}
	defer src.Close()
	src.WriteString("hello")
	for _, code := range []error{unix.EXDEV, unix.ENOTSUP, unix.EIO} {
		dst, e := os.CreateTemp(t.TempDir(), "dst")
		if e != nil {
			t.Fatal(e)
		}
		e = cloneIntoWith(src, dst, 5, func(*os.File, *os.File) error { return code })
		if code == unix.EIO {
			if !errors.Is(e, code) {
				t.Fatal(e)
			}
		} else {
			if e != nil {
				t.Fatal(e)
			}
			dst.Seek(0, 0)
			b, _ := io.ReadAll(dst)
			if string(b) != "hello" {
				t.Fatal(string(b))
			}
		}
		dst.Close()
	}
}

func TestCloneCopyRejectsLengthMismatch(t *testing.T) {
	for _, size := range []int64{4, 6} {
		src, _ := os.CreateTemp(t.TempDir(), "src")
		src.WriteString("hello")
		dst, _ := os.CreateTemp(t.TempDir(), "dst")
		e := cloneIntoWith(src, dst, size, func(*os.File, *os.File) error { return unix.EXDEV })
		src.Close()
		dst.Close()
		if e == nil {
			t.Fatalf("accepted length %d", size)
		}
	}
}
