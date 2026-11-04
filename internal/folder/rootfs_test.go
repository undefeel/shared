package folder

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func rootFixture(t *testing.T) (*rootFS, string, string) {
	t.Helper()
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := openRootFS(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
		b, err := os.ReadFile(filepath.Join(outside, "file"))
		if err != nil || string(b) != "outside" {
			t.Errorf("outside changed: %q, %v", b, err)
		}
	})
	return r, root, outside
}

func TestRootFSParentSymlink(t *testing.T) {
	r, root, outside := rootFixture(t)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if f, err := r.openRead("link/file"); err == nil {
		f.Close()
		t.Fatal("read followed link")
	}
	if f, err := r.create("link/new"); err == nil {
		f.Close()
		t.Fatal("create followed link")
	}
	if err := r.unlink("link/file", false); err == nil {
		t.Fatal("unlink followed link")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestRootFSLeafSymlink(t *testing.T) {
	r, root, outside := rootFixture(t)
	if err := os.Symlink(filepath.Join(outside, "file"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if f, err := r.openRead("link"); err == nil {
		f.Close()
		t.Fatal("read followed link")
	}
	if f, err := r.create("link"); err == nil {
		f.Close()
		t.Fatal("create followed link")
	}
	if err := r.unlink("link", false); err != nil {
		t.Fatal(err)
	}
}

func TestRootFSRootSymlink(t *testing.T) {
	_, root, outside := rootFixture(t)
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if r, err := openRootFS(link); err == nil {
		r.Close()
		t.Fatal("opened symlink root")
	}
}

func TestRootFSParentReplacedAfterOpen(t *testing.T) {
	r, root, outside := rootFixture(t)
	p, leaf, err := r.parent("dir/file", true)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := os.Rename(filepath.Join(root, "dir"), filepath.Join(root, "held")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	f, err := openRegularAt(p, leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("held"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := unix.Unlinkat(int(p.Fd()), leaf, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := r.openRead("dir/file"); err == nil {
		f.Close()
		t.Fatal("reopened through symlink")
	}
}

func TestRootFSSpecialFile(t *testing.T) {
	r, root, _ := rootFixture(t)
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := r.openRead("fifo"); err == nil {
		f.Close()
		t.Fatal("accepted FIFO")
	}
	if f, err := r.openRead("."); err == nil {
		f.Close()
		t.Fatal("accepted directory")
	}
}

func TestRootFSLifecycle(t *testing.T) {
	r, _, _ := rootFixture(t)
	f, err := r.create("a/b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("data"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	f, err = r.openRead("a/b")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(b) != "data" {
		t.Fatalf("%q %v", b, err)
	}
	d, err := r.openDir("a", false)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v %v", entries, err)
	}
	if err := r.unlink("a", true); err == nil {
		t.Fatal("removed nonempty directory")
	}
	if err := r.unlink("a/b", false); err != nil {
		t.Fatal(err)
	}
	if err := r.unlink("a", true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "../file", "a/../file", "/file", "a\\b", "a\x00b"} {
		if f, err := r.create(name); err == nil {
			f.Close()
			t.Fatalf("accepted %q", name)
		}
	}
}
