package folder

import (
	"encoding/json"
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

func TestScanRootOpenFailureBlocksWithoutPublishing(t *testing.T) {
	f := rootScanFixture(t)
	indexBefore, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var before persisted
	if err := json.Unmarshal(indexBefore, &before); err != nil {
		t.Fatal(err)
	}
	ioops := f.rootScanIO()
	baseOpenDir := ioops.openDir
	ioops.openDir = func(r *rootFS, name string) (*os.File, error) {
		if name == "." {
			return nil, os.ErrNotExist
		}
		return baseOpenDir(r, name)
	}
	if err := f.scanWith(ioops); err == nil {
		t.Fatal("Scan succeeded after injected root open failure")
	}
	if e := f.Blocked(); e == nil || e.Code != "root_unavailable" {
		t.Fatalf("blocked=%+v, want root_unavailable", e)
	}
	indexAfter, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var after persisted
	if err := json.Unmarshal(indexAfter, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Files, after.Files) || before.Clock != after.Clock {
		t.Fatal("root open failure changed indexed files or clock")
	}
}

func TestScanEmptyDecisionUsesIndexAtCommit(t *testing.T) {
	dir := t.TempDir()
	f, err := Open(filepath.Join(dir, "root"), filepath.Join(dir, "index.json"), "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	ioops := f.rootScanIO()
	ioops.beforeCommit = func() {
		f.mu.Lock()
		f.files["prior"] = &FileInfo{Name: "prior", Deleted: true, Version: Vector{"me": 1}}
		f.mu.Unlock()
		if err := f.save(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.scanWith(ioops); err == nil {
		t.Fatal("empty candidate committed despite index becoming nonempty")
	}
	if e := f.Blocked(); e == nil || e.Code != "root_empty" {
		t.Fatalf("blocked=%+v, want root_empty", e)
	}
	if fi, _, ok := f.Lookup("prior"); !ok || !fi.Deleted {
		t.Fatalf("hook index entry was not preserved: fi=%+v ok=%v", fi, ok)
	}
}

func TestScanReplacementRootBlocksWithoutPublishingTombstones(t *testing.T) {
	f := rootScanFixture(t)
	indexBefore, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var before persisted
	if err := json.Unmarshal(indexBefore, &before); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.Root, f.Root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.Root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err == nil {
		t.Fatal("Scan succeeded with a replacement root")
	}
	if e := f.Blocked(); e == nil || e.Code != "root_changed" {
		t.Fatalf("blocked=%+v, want root_changed", e)
	}
	indexAfter, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var after persisted
	if err := json.Unmarshal(indexAfter, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Files, after.Files) || before.Clock != after.Clock {
		t.Fatalf("replacement scan published index changes: before=%+v after=%+v", before, after)
	}
	for _, fi := range after.Files {
		if fi.Deleted {
			t.Fatalf("replacement scan wrote tombstone: %+v", fi)
		}
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

func TestConfirmResetPreservesFiles(t *testing.T) {
	f := rootScanFixture(t)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "kept"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = f.SetConfiguredRoot(other)
	token := f.Blocked().Token
	if err := f.Confirm(token, "reset", other); err != nil {
		t.Fatal(err)
	}
	if len(f.files) != 0 {
		t.Fatal("reset retained index")
	}
	if _, err := os.Stat(filepath.Join(other, "kept")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.statePath + ".reset-" + token); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmRebindEmptyOnce(t *testing.T) {
	f := rootScanFixture(t)
	other := t.TempDir()
	_ = f.SetConfiguredRoot(other)
	if err := f.Confirm(f.Blocked().Token, "rebind", other); err != nil {
		t.Fatal(err)
	}
	if !f.files["item"].Deleted {
		t.Fatal("rebind did not publish deletion")
	}
	if err := f.Scan(); err == nil {
		t.Fatal("empty bypass reused")
	}
}

func TestConfirmStaleToken(t *testing.T) {
	f := rootScanFixture(t)
	other := t.TempDir()
	_ = f.SetConfiguredRoot(other)
	token := f.Blocked().Token
	if err := f.Confirm(token, "reset", other); err != nil {
		t.Fatal(err)
	}
	if err := f.Confirm(token, "reset", other); err != ErrStaleToken {
		t.Fatal(err)
	}
}

func TestConfirmRejectsMissingRoot(t *testing.T) {
	f := rootScanFixture(t)
	other := filepath.Join(t.TempDir(), "missing")
	_ = f.SetConfiguredRoot(other)
	token := f.Blocked().Token
	if err := f.Confirm(token, "reset", other); err == nil {
		t.Fatal("accepted missing root")
	}
	if f.Blocked().Token != token {
		t.Fatal("consumed token")
	}
}

func TestScanErrorClearsAfterSuccess(t *testing.T) {
	f := rootScanFixture(t)
	// Change stat to force a read rather than a cache hit.
	if err := os.WriteFile(filepath.Join(f.Root, "item"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	ops := f.rootScanIO()
	read := ops.openRead
	ops.openRead = func(r *rootFS, name string) (*os.File, error) {
		if name == ignoreFile {
			return read(r, name)
		}
		return nil, os.ErrPermission
	}
	if err := f.scanWith(ops); err == nil {
		t.Fatal("expected read error")
	}
	if e := f.LastScanError(); e == nil || e.Path != "item" || e.Time.IsZero() {
		t.Fatal(e)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	if e := f.LastScanError(); e != nil {
		t.Fatal(e)
	}
}
