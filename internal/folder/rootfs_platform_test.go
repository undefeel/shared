//go:build darwin || linux

package folder

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func platformFixture(t *testing.T) (*os.File, string) {
	t.Helper()
	r, root, _ := rootFixture(t)
	for name, data := range map[string]string{"source": "incoming", "target": "local"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return r.root, root
}

func assertRenameSides(t *testing.T, root string) {
	t.Helper()
	for name, want := range map[string]string{"source": "incoming", "target": "local"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
}

func TestRenameNoReplaceExists(t *testing.T) {
	p, root := platformFixture(t)
	if err := renameNoReplace(p, "source", p, "target"); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("want EEXIST, got %v", err)
	}
	assertRenameSides(t, root)
}

func TestRenameNoReplaceUnsupported(t *testing.T) {
	p, root := platformFixture(t)
	ops := platformOps()
	ops.rename = func(int, string, int, string) error { return unix.ENOTSUP }
	if err := ops.renameNoReplace(p, "source", p, "target"); !errors.Is(err, unix.ENOTSUP) {
		t.Fatalf("want ENOTSUP, got %v", err)
	}
	assertRenameSides(t, root)
}

func TestPlatformDurableMove(t *testing.T) {
	p, root := platformFixture(t)
	f, err := openRegularAt(p, "source", unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syncFile(f); err != nil {
		t.Fatal(err)
	}
	r, err := openRootFS(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dest, err := r.openDir("dest", true)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if err := renameNoReplace(p, "source", dest, "installed"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []*os.File{p, dest} {
		if err := syncDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "source")); !os.IsNotExist(err) {
		t.Fatalf("source remains: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "dest", "installed"))
	if err != nil || string(data) != "incoming" {
		t.Fatalf("installed = %q, %v", data, err)
	}
}

func TestFileSyncFault(t *testing.T) {
	p, _ := platformFixture(t)
	ops := platformOps()
	ops.fsync = func(int) error { return unix.EIO }
	ops.fullSync = func(uintptr) error { t.Fatal("full flush after fsync failure"); return nil }
	if err := ops.syncFile(p); !errors.Is(err, unix.EIO) {
		t.Fatalf("want EIO, got %v", err)
	}
}

func TestDirectorySyncFault(t *testing.T) {
	p, _ := platformFixture(t)
	ops := platformOps()
	ops.fsync = func(int) error { return unix.EIO }
	if err := ops.syncDir(p); !errors.Is(err, unix.EIO) {
		t.Fatalf("want EIO, got %v", err)
	}
}

func TestFullSyncFault(t *testing.T) {
	p, _ := platformFixture(t)
	ops := platformOps()
	if ops.fullSync == nil {
		t.Skip("platform uses fsync without a separate full flush")
	}
	ops.fsync = func(int) error { return nil }
	ops.fullSync = func(uintptr) error { return unix.ENOTSUP }
	if err := ops.syncFile(p); !errors.Is(err, unix.ENOTSUP) {
		t.Fatalf("want ENOTSUP, got %v", err)
	}
}

func TestRenameNoReplacePinnedParent(t *testing.T) {
	r, root, outside := rootFixture(t)
	p, _, err := r.parent("dir/source", true)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	f, err := openRegularAt(p, "source", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "dir"), filepath.Join(root, "held")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(p, "source", p, "file"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "held", "file")); err != nil {
		t.Fatal(err)
	}
}
