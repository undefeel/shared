package folder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestIncrementalCreateEditDeleteRename(t *testing.T) {
	f := rootScanFixture(t)
	f.integrationStability().interval = time.Millisecond
	for _, d := range []string{"one", "two"} {
		if err := os.Mkdir(filepath.Join(f.Root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.Root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(paths ...string) {
		t.Helper()
		if err := f.ScanPaths(context.Background(), paths); err != nil {
			t.Fatal(err)
		}
	}
	check := func(name string, deleted bool) {
		t.Helper()
		fi, _, ok := f.Lookup(name)
		if !ok || fi.Deleted != deleted {
			t.Fatalf("%s = %+v, found %v", name, fi, ok)
		}
	}
	write("one/a", "first")
	scan("one")
	check("one/a", false)
	before, _, _ := f.Lookup("one/a")
	write("one/a", "edited body")
	scan("one/a")
	after, _, _ := f.Lookup("one/a")
	if before.Hash == after.Hash {
		t.Fatal("edit was missed")
	}
	if err := os.Rename(filepath.Join(f.Root, "one/a"), filepath.Join(f.Root, "two/b")); err != nil {
		t.Fatal(err)
	}
	scan("one", "two")
	check("one/a", true)
	check("two/b", false)
	if err := os.Remove(filepath.Join(f.Root, "two/b")); err != nil {
		t.Fatal(err)
	}
	scan("two")
	check("two/b", true)
	check("item", false)
}

func TestIncrementalHashCacheAndFullScan(t *testing.T) {
	f := rootScanFixture(t)
	f.integrationStability().interval = time.Millisecond
	// Together with the fixture file this makes 2092 live files.
	for i := 0; i < 2091; i++ {
		if err := os.WriteFile(filepath.Join(f.Root, fmt.Sprintf("file-%04d", i)), []byte("initial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Root, "file-0000"), []byte("changed body"), 0600); err != nil {
		t.Fatal(err)
	}
	ops := f.rootScanIO()
	base := ops.openRead
	var reads atomic.Int32
	ops.openRead = func(r *rootFS, p string) (*os.File, error) {
		if p != ignoreFile {
			reads.Add(1)
		}
		return base(r, p)
	}
	if err := f.scanPathsWith(context.Background(), []string{"file-0000"}, ops); err != nil {
		t.Fatal(err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("incremental hashes=%d want 1", got)
	}
	reads.Store(0)
	if err := f.scanWith(ops); err != nil {
		t.Fatal(err)
	}
	if got := reads.Load(); got != 2092 {
		t.Fatalf("full hashes=%d want 2092", got)
	}
}

func TestIncrementalLastDeletionBlocks(t *testing.T) {
	f := rootScanFixture(t)
	f.integrationStability().interval = time.Millisecond
	before, clock, seq := scanState(f)
	if err := os.Remove(filepath.Join(f.Root, "item")); err != nil {
		t.Fatal(err)
	}
	if err := f.ScanPaths(context.Background(), []string{"item"}); err == nil {
		t.Fatal("last deletion accepted")
	}
	if b := f.Blocked(); b == nil || b.Code != "root_empty" {
		t.Fatalf("blocked=%+v", b)
	}
	after, c, s := scanState(f)
	if !reflect.DeepEqual(before, after) || clock != c || seq != s {
		t.Fatal("empty scan changed index")
	}
	root, state := f.Root, f.statePath
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, state, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if b := reopened.Blocked(); b == nil || b.Code != "root_empty" {
		t.Fatalf("reopened blocked=%+v", b)
	}
	persisted, c, _ := scanState(reopened)
	if !reflect.DeepEqual(before, persisted) || clock != c {
		t.Fatal("empty scan changed durable index")
	}
}

func TestIncrementalScopeFailurePreservesAllRecords(t *testing.T) {
	for _, failure := range []string{"directory", "readdir", "stat", "hash"} {
		t.Run(failure, func(t *testing.T) {
			f := rootScanFixture(t)
			f.integrationStability().interval = time.Millisecond
			for _, d := range []string{"a", "b"} {
				if err := os.Mkdir(filepath.Join(f.Root, d), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.Root, d, "old"), []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.Scan(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(f.Root, "a", "old")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.Root, "b", "new"), []byte("new"), 0600); err != nil {
				t.Fatal(err)
			}
			before, clock, seq := scanState(f)
			ops := f.rootScanIO()
			switch failure {
			case "directory":
				base := ops.openDir
				ops.openDir = func(r *rootFS, p string) (*os.File, error) {
					if p == "b" {
						return nil, os.ErrPermission
					}
					return base(r, p)
				}
			case "readdir":
				base := ops.readDir
				ops.readDir = func(d *os.File) ([]os.DirEntry, error) {
					if filepath.Base(d.Name()) == "b" {
						return nil, os.ErrPermission
					}
					return base(d)
				}
			case "stat":
				base := ops.info
				ops.info = func(d *os.File, e os.DirEntry) (os.FileInfo, scanStat, error) {
					if e.Name() == "new" {
						return nil, scanStat{}, os.ErrPermission
					}
					return base(d, e)
				}
			case "hash":
				base := ops.openRead
				ops.openRead = func(r *rootFS, p string) (*os.File, error) {
					if p == "b/new" {
						return nil, os.ErrPermission
					}
					return base(r, p)
				}
			}
			if err := f.scanPathsWith(context.Background(), []string{"a", "b"}, ops); err == nil {
				t.Fatal("expected failure")
			}
			after, c, s := scanState(f)
			if !reflect.DeepEqual(before, after) || clock != c || seq != s {
				t.Fatal("failed scan changed index")
			}
		})
	}
}

func TestIncrementalOutsideScopeNormalizationCollision(t *testing.T) {
	f := rootScanFixture(t)
	f.integrationStability().interval = time.Millisecond
	if err := os.Mkdir(filepath.Join(f.Root, "a"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Root, "a", "é"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	// On filesystems that normalize names, this cannot be represented.
	collision := filepath.Join(f.Root, "a", "e\u0301")
	if _, err := os.Stat(collision); err == nil {
		t.Skip("filesystem normalizes filenames")
	}
	if err := os.WriteFile(collision, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	before, clock, seq := scanState(f)
	if err := f.ScanPaths(context.Background(), []string{"a/e\u0301"}); err == nil {
		t.Fatal("collision accepted")
	}
	after, c, s := scanState(f)
	if !reflect.DeepEqual(before, after) || clock != c || seq != s {
		t.Fatal("collision changed index")
	}
}

func TestIncrementalDirectoryScopeIncludesSubtree(t *testing.T) {
	f := rootScanFixture(t)
	f.integrationStability().interval = time.Millisecond
	dir := filepath.Join(f.Root, "dir", "nested")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("nested"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.ScanPaths(context.Background(), []string{"dir"}); err != nil {
		t.Fatal(err)
	}
	fi, _, ok := f.Lookup("dir/nested/file")
	if !ok || fi.Deleted {
		t.Fatalf("subtree entry=%+v found=%v", fi, ok)
	}
}
