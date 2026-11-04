package folder

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func rootScanFixture(t *testing.T) *Folder {
	t.Helper()
	dir := t.TempDir()
	f, err := Open(filepath.Join(dir, "root"), filepath.Join(dir, "index.json"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := os.WriteFile(filepath.Join(f.Root, "item"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	return f
}

func assertRootEmptyBlocked(t *testing.T, f *Folder) {
	t.Helper()
	before, clock, seq := scanState(f)
	err := f.Scan()
	if err == nil || f.Blocked() == nil || f.Blocked().Code != "root_empty" {
		t.Fatalf("Scan error=%v blocked=%+v, want root_empty", err, f.Blocked())
	}
	after, gotClock, gotSeq := scanState(f)
	if !reflect.DeepEqual(before, after) || gotClock != clock || gotSeq != seq {
		t.Fatalf("empty scan published state: before=%+v after=%+v clock=%d/%d seq=%d/%d", before, after, clock, gotClock, seq, gotSeq)
	}
}

func TestScanEmptyRootAfterNonemptyIndexBlocks(t *testing.T) {
	f := rootScanFixture(t)
	if err := os.Remove(filepath.Join(f.Root, "item")); err != nil {
		t.Fatal(err)
	}
	assertRootEmptyBlocked(t, f)
}

func TestScanEmptyRootWithOnlyTombstoneBlocks(t *testing.T) {
	f := rootScanFixture(t)
	f.mu.Lock()
	old := f.files["item"]
	f.files = map[string]*FileInfo{"gone": {Name: "gone", Deleted: true, Version: old.Version.Copy()}}
	f.mu.Unlock()
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.Root, "item")); err != nil {
		t.Fatal(err)
	}
	assertRootEmptyBlocked(t, f)
}

func TestScanUnmountSimulationBlocksBeforePublishing(t *testing.T) {
	f := rootScanFixture(t)
	if err := os.Rename(f.Root, f.Root+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	before, clock, seq := scanState(f)
	if err := f.Scan(); err == nil {
		t.Fatal("Scan succeeded with configured root unavailable")
	}
	if e := f.Blocked(); e == nil || e.Code != "root_unavailable" {
		t.Fatalf("blocked=%+v, want root_unavailable", e)
	}
	after, gotClock, gotSeq := scanState(f)
	if !reflect.DeepEqual(before, after) || gotClock != clock || gotSeq != seq {
		t.Fatal("unavailable root changed published index")
	}
}

func TestScanRemovingLastFileBlocks(t *testing.T) {
	f := rootScanFixture(t)
	if err := os.Remove(filepath.Join(f.Root, "item")); err != nil {
		t.Fatal(err)
	}
	assertRootEmptyBlocked(t, f)
}

func TestScanIgnoreAllBlocksEmptyCandidate(t *testing.T) {
	f := rootScanFixture(t)
	if err := os.WriteFile(filepath.Join(f.Root, ignoreFile), []byte("*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	assertRootEmptyBlocked(t, f)
}

func TestScanNonemptyDoesNotClearRootLatch(t *testing.T) {
	f := rootScanFixture(t)
	if err := f.blockRoot("root_empty", "awaiting confirmation"); err == nil {
		t.Fatal("blockRoot did not return latch")
	}
	token := f.Blocked().Token
	if err := f.Scan(); err == nil {
		t.Fatal("Scan succeeded while latch was set")
	}
	if got := f.Blocked(); got == nil || got.Token != token {
		t.Fatalf("Scan changed latch: got=%+v want token=%q", got, token)
	}
}
