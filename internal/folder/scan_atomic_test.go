package folder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
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
	before, clock, seq := scanState(f)
	if err := f.scanWith(ioops); err == nil {
		t.Fatal("expected injected scan failure")
	}
	after, gotClock, gotSeq := scanState(f)
	if !reflect.DeepEqual(before, after) || gotSeq != seq || gotClock != clock {
		t.Fatalf("failed scan published state: before=%+v after=%+v seq=%d/%d clock=%d/%d", before, after, seq, gotSeq, clock, gotClock)
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

func scanState(f *Folder) (map[string]*FileInfo, uint64, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files := make(map[string]*FileInfo, len(f.files))
	for name, fi := range f.files {
		c := *fi
		c.Version = fi.Version.Copy()
		files[name] = &c
	}
	return files, f.clock, f.seq
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
	ioops.info = func(dir *os.File, d os.DirEntry) (os.FileInfo, scanStat, error) {
		if d.Name() == "z.txt" {
			return nil, scanStat{}, errors.New("injected EIO")
		}
		return base(dir, d)
	}
	assertFailedScanUntouchedThenRetry(t, f, ioops)
}

func TestScanAtomicPersistFailureAndConcurrentSave(t *testing.T) {
	f := atomicScanFixture(t)
	if err := os.WriteFile(filepath.Join(f.Root, "new.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	before, clock, _ := scanState(f)
	diskBefore, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	entered, release, saveStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f.persistHooks.marshal = func(v any) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return nil, errors.New("injected persist failure")
		}
		return json.Marshal(v)
	}
	scanDone := make(chan error, 1)
	go func() { scanDone <- f.Scan() }()
	<-entered
	saveDone := make(chan error, 1)
	go func() { close(saveStarted); saveDone <- f.save() }()
	<-saveStarted
	close(release)
	if err := <-scanDone; err == nil {
		t.Fatal("expected persist error")
	}
	if err := <-saveDone; err != nil {
		t.Fatalf("parallel save: %v", err)
	}
	after, gotClock, _ := scanState(f)
	if !reflect.DeepEqual(before, after) || gotClock != clock {
		t.Fatalf("failed commit changed memory: %v / %v", before, after)
	}
	diskAfter, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var oldP, newP persisted
	if err := json.Unmarshal(diskBefore, &oldP); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(diskAfter, &newP); err != nil {
		t.Fatal(err)
	}
	if oldP.Clock != newP.Clock || !reflect.DeepEqual(oldP.Files, newP.Files) {
		t.Fatalf("failed commit changed durable index: %s / %s", diskBefore, diskAfter)
	}
}

func TestFullScanHashesAndRestoredMtimeChange(t *testing.T) {
	f := atomicScanFixture(t)
	var reads atomic.Int32
	ioops := f.rootScanIO()
	base := ioops.openRead
	ioops.openRead = func(r *rootFS, p string) (*os.File, error) {
		if p != ignoreFile {
			reads.Add(1)
		}
		return base(r, p)
	}
	if err := f.scanWith(ioops); err != nil {
		t.Fatal(err)
	}
	reads.Store(0)
	if err := f.scanWith(ioops); err != nil {
		t.Fatal(err)
	}
	if got := reads.Load(); got == 0 {
		t.Fatal("full scan skipped stableHash for unchanged metadata")
	}
	path := filepath.Join(f.Root, "a.txt")
	oldInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("b"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := f.scanWith(ioops); err != nil {
		t.Fatal(err)
	}
	fi, _, ok := f.Lookup("a.txt")
	if !ok {
		t.Fatal("file missing after scan")
	}
	want := sha256.Sum256([]byte("b"))
	if fi.Hash != hex.EncodeToString(want[:]) {
		t.Fatalf("restored-mtime edit missed: hash=%s want=%s", fi.Hash, hex.EncodeToString(want[:]))
	}
	if fi.ModTime != oldInfo.ModTime().UnixNano() {
		t.Fatalf("mtime not restored: got %s want %s", time.Unix(0, fi.ModTime), oldInfo.ModTime())
	}
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
