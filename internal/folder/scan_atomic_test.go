package folder

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func atomicScanFixture(t *testing.T) *Folder {
	t.Helper()
	f := newNode(t, "scan-test").f
	if err := os.WriteFile(filepath.Join(f.Root, "old.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.Root, "old.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Root, "a.txt"), []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func assertFailedScanUntouchedThenRetry(t *testing.T, f *Folder, ioops scanIO) {
	t.Helper()
	before, seq := f.Snapshot()
	clock := f.clock
	if err := f.scanWith(ioops); err == nil {
		t.Fatal("expected injected scan failure")
	}
	after, gotSeq := f.Snapshot()
	if gotSeq != seq || f.clock != clock || len(before) != len(after) || before[0].Name != after[0].Name || before[0].Deleted != after[0].Deleted {
		t.Fatalf("failed scan published state: before=%+v after=%+v seq=%d/%d clock=%d/%d", before, after, seq, gotSeq, clock, f.clock)
	}
	if err := f.Scan(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	items, _ := f.Snapshot()
	byName := map[string]FileInfo{}
	for _, fi := range items {
		byName[fi.Name] = fi
	}
	if !byName["old.txt"].Deleted || byName["a.txt"].Deleted {
		t.Fatalf("retry did not publish complete scan: %+v", items)
	}
}

func TestScanAtomicSubdirectoryPermissionFailure(t *testing.T) {
	f := atomicScanFixture(t)
	if err := os.Mkdir(filepath.Join(f.Root, "denied"), 0700); err != nil {
		t.Fatal(err)
	}
	ioops := f.rootScanIO()
	base := ioops.openDir
	ioops.openDir = func(r *rootFS, p string) (*os.File, error) {
		if p == "denied" {
			return nil, os.ErrPermission
		}
		return base(r, p)
	}
	assertFailedScanUntouchedThenRetry(t, f, ioops)
}

func TestScanAtomicInfoFailure(t *testing.T) {
	f := atomicScanFixture(t)
	if err := os.WriteFile(filepath.Join(f.Root, "z.txt"), []byte("z"), 0600); err != nil {
		t.Fatal(err)
	}
	ioops := f.rootScanIO()
	base := ioops.info
	ioops.info = func(dir *os.File, d os.DirEntry) (os.FileInfo, error) {
		if d.Name() == "z.txt" {
			return nil, errors.New("injected EIO")
		}
		return base(dir, d)
	}
	assertFailedScanUntouchedThenRetry(t, f, ioops)
}

func TestScanAtomicHashFailureAfterSuccess(t *testing.T) {
	f := atomicScanFixture(t)
	if err := os.WriteFile(filepath.Join(f.Root, "z.txt"), []byte("z"), 0600); err != nil {
		t.Fatal(err)
	}
	ioops := f.rootScanIO()
	base := ioops.openRead
	ioops.openRead = func(r *rootFS, p string) (*os.File, error) {
		if p == "z.txt" {
			return nil, errors.New("injected hash EIO")
		}
		return base(r, p)
	}
	assertFailedScanUntouchedThenRetry(t, f, ioops)
}

func TestScanAtomicIgnoreReadFailureAfterSuccess(t *testing.T) {
	f := atomicScanFixture(t)
	if err := os.WriteFile(filepath.Join(f.Root, ".sharedirignore"), []byte("ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ioops := f.rootScanIO()
	base := ioops.openRead
	ioops.openRead = func(r *rootFS, p string) (*os.File, error) {
		if p == ignoreFile {
			return nil, errors.New("injected ignore EIO")
		}
		return base(r, p)
	}
	assertFailedScanUntouchedThenRetry(t, f, ioops)
}
