package folder

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T) (*Folder, string, FileInfo) {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, "a")
	if err := os.WriteFile(p, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := Open(root, filepath.Join(t.TempDir(), "index"), "me", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	fi, _, _ := f.Lookup("a")
	return f, p, fi
}
func TestOpenVersionSymlinkOutside(t *testing.T) {
	f, p, fi := readFixture(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p); err != nil {
		t.Fatal(err)
	}
	if _, fd, err := f.OpenVersion("a", fi.Hash); err == nil {
		fd.Close()
		t.Fatal("followed symlink")
	}
}
func TestOpenVersionChangedHash(t *testing.T) {
	f, p, fi := readFixture(t)
	if err := os.WriteFile(p, []byte("xyz"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, fd, err := f.OpenVersion("a", fi.Hash); err == nil {
		fd.Close()
		t.Fatal("served changed bytes")
	}
}
func TestOpenVersionReplacedInode(t *testing.T) {
	f, p, fi := readFixture(t)
	replacement := filepath.Join(f.Root, "replacement")
	if err := os.WriteFile(replacement, []byte("xyz"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, p); err != nil {
		t.Fatal(err)
	}
	if _, fd, err := f.OpenVersion("a", fi.Hash); err == nil {
		fd.Close()
		t.Fatal("served replacement")
	}
}
func TestUnblockDoesNotReviveOldFD(t *testing.T) {
	f, _, fi := readFixture(t)
	gen := f.ConfirmedGeneration()
	_, fd, err := f.OpenVersion("a", fi.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	f.scanMu.Lock()
	_ = f.blockRoot("root_changed", "test")
	f.mu.Lock()
	f.confirmedGeneration++ // model explicit confirmation under scanMu
	f.blocked = nil
	f.mu.Unlock()
	f.scanMu.Unlock()
	if err := f.CheckRead(gen); err == nil {
		t.Fatal("old fd request revived")
	}
}
