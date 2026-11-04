package folder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

func captureFixture(t *testing.T, name string) (*rootFS, *capture, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := openRootFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	c, err := r.prepareCapture(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return r, c, dir
}
func captureBytes(t *testing.T, dir, name, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || string(b) != want {
		t.Fatalf("%s = %q, %v; want %q", name, b, err, want)
	}
}
func mustCapture(t *testing.T, c *capture) {
	t.Helper()
	if err := c.take(); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureConflictCollision(t *testing.T) {
	_, c, dir := captureFixture(t, "file.txt")
	mustCapture(t, c)
	leaf := conflictLeaf(c.Original, "peer", "hash", "collision")
	if err := os.WriteFile(filepath.Join(dir, leaf), []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	n := 0
	got, err := c.promoteWithToken("peer", "hash", func() string {
		n++
		if n == 1 {
			return "collision"
		}
		return "unique"
	})
	if err != nil {
		t.Fatal(err)
	}
	captureBytes(t, dir, leaf, "previous")
	captureBytes(t, dir, got, "old")
}
func TestCaptureRepeatedHashAndTime(t *testing.T) {
	r, c, dir := captureFixture(t, "file.txt")
	mustCapture(t, c)
	st, err := os.Stat(filepath.Join(dir, c.Recovery))
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.promote("peer", "samehash")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, c.Original), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(filepath.Join(dir, c.Original), st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	second, err := r.prepareCapture(c.Original)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	mustCapture(t, second)
	next, err := second.promote("peer", "samehash")
	if err != nil {
		t.Fatal(err)
	}
	if first == next {
		t.Fatal("reused conflict name")
	}
	captureBytes(t, dir, first, "old")
	captureBytes(t, dir, next, "old")
}
func TestCaptureLongUnicodeBase(t *testing.T) {
	_, c, dir := captureFixture(t, strings.Repeat("界", 70)+".txt")
	mustCapture(t, c)
	got, err := c.promote("peer", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 255 || !utf8.ValidString(got) || !norm.NFC.IsNormalString(got) {
		t.Fatalf("invalid conflict %q", got)
	}
	captureBytes(t, dir, got, "old")
}
func TestCaptureOpenWriter(t *testing.T) {
	_, c, dir := captureFixture(t, "file")
	f, err := os.OpenFile(filepath.Join(dir, c.Original), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mustCapture(t, c)
	if _, err = f.WriteAt([]byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	captureBytes(t, dir, c.Recovery, "new")
	got, err := c.restore("peer", "hash")
	if err != nil {
		t.Fatal(err)
	}
	captureBytes(t, dir, got, "new")
}
func TestCaptureTargetCreatedInBetween(t *testing.T) {
	_, c, dir := captureFixture(t, "file")
	mustCapture(t, c)
	if err := os.WriteFile(filepath.Join(dir, c.Original), []byte("incoming"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := c.restore("peer", "hash")
	if err != nil {
		t.Fatal(err)
	}
	captureBytes(t, dir, c.Original, "incoming")
	captureBytes(t, dir, got, "old")
}
func TestCaptureENOSPC(t *testing.T) {
	_, c, dir := captureFixture(t, "file")
	real := c.ops
	c.ops.rename = func(int, string, int, string) error { return unix.ENOSPC }
	if err := c.take(); !errors.Is(err, unix.ENOSPC) {
		t.Fatal(err)
	}
	captureBytes(t, dir, c.Original, "old")
	c.ops = real
	mustCapture(t, c)
	if err := os.WriteFile(filepath.Join(dir, c.Original), []byte("incoming"), 0600); err != nil {
		t.Fatal(err)
	}
	c.ops.rename = func(int, string, int, string) error { return unix.ENOSPC }
	if _, err := c.promote("peer", "hash"); !errors.Is(err, unix.ENOSPC) {
		t.Fatal(err)
	}
	captureBytes(t, dir, c.Recovery, "old")
	captureBytes(t, dir, c.Original, "incoming")
}
func TestCaptureRecoveryCollisionAndDiscard(t *testing.T) {
	_, c, dir := captureFixture(t, "file")
	if err := os.WriteFile(filepath.Join(dir, c.Recovery), []byte("prior"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.take(); !errors.Is(err, unix.EEXIST) {
		t.Fatal(err)
	}
	captureBytes(t, dir, c.Original, "old")
	captureBytes(t, dir, c.Recovery, "prior")
	if err := c.discard(); err == nil {
		t.Fatal("discard accepted an uncaptured entry")
	}
	if err := os.Remove(filepath.Join(dir, c.Recovery)); err != nil {
		t.Fatal(err)
	}
	mustCapture(t, c)
	if err := c.discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, c.Recovery)); !os.IsNotExist(err) {
		t.Fatal(err)
	}

}

func TestCaptureDiscardReplacedRecovery(t *testing.T) {
	_, c, dir := captureFixture(t, "file")
	mustCapture(t, c)
	// Keep the captured inode alive to prevent inode number reuse in this test.
	held, err := os.Open(filepath.Join(dir, c.Recovery))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.Remove(filepath.Join(dir, c.Recovery)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, c.Recovery), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.discard(); err == nil {
		t.Fatal("discard accepted a replacement inode")
	}
	captureBytes(t, dir, c.Recovery, "replacement")
}
