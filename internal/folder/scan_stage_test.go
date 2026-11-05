package folder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Changes after collection invalidate the entire candidate, including deletions.
func TestStagedScanRejectsFilesystemChangesBeforeCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *Folder)
	}{
		{"candidate", func(t *testing.T, f *Folder) {
			if err := os.WriteFile(filepath.Join(f.Root, "a.txt"), []byte("changed candidate"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"parent", func(t *testing.T, f *Folder) {
			if err := os.WriteFile(filepath.Join(f.Root, "extra"), []byte("extra"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"ignore", func(t *testing.T, f *Folder) {
			if err := os.WriteFile(filepath.Join(f.Root, ignoreFile), []byte("a.txt\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := atomicScanFixture(t)
			before, clock, seq := scanState(f)
			ops := f.rootScanIO()
			ops.beforeCommit = func() { tc.mutate(t, f) }
			if err := f.scanWith(ops); !errors.Is(err, ErrScanPending) {
				t.Fatalf("scan = %v, want ErrScanPending", err)
			}
			after, gotClock, gotSeq := scanState(f)
			if !reflect.DeepEqual(before, after) || clock != gotClock || seq != gotSeq {
				t.Fatalf("rejected candidate changed index: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestStagedScanConcurrentApply(t *testing.T) {
	n := applySeed(t)
	entered, release := make(chan struct{}), make(chan struct{})
	ops := n.f.rootScanIO()
	base := ops.openRead
	ops.openRead = func(r *rootFS, p string) (*os.File, error) {
		if p == "x" {
			close(entered)
			<-release
		}
		return base(r, p)
	}
	done := make(chan error, 1)
	go func() { done <- n.f.scanWith(ops) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("scan did not reach hash")
	}
	applied := make(chan [2]int, 1)
	go func() {
		a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{incoming("new", Vector{"A": 1, "B": 1}, 200e9)}, bodyFetch("new"), nil)
		applied <- [2]int{a, b}
	}()
	select {
	case result := <-applied:
		if result != [2]int{1, 0} {
			close(release)
			t.Fatalf("apply = %v", result)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("apply blocked behind hashing")
	}
	before, clock, seq := scanState(n.f)
	close(release)
	if err := <-done; !errors.Is(err, ErrScanPending) {
		t.Fatalf("scan = %v, want pending", err)
	}
	after, gotClock, gotSeq := scanState(n.f)
	if !reflect.DeepEqual(before, after) || clock != gotClock || seq != gotSeq {
		t.Fatal("stale scan changed applied index")
	}
	assertBody(t, n, "new")
}

func TestStagedScanCloseWaitsForLease(t *testing.T) {
	f := rootScanFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	ops := f.rootScanIO()
	base := ops.openRead
	ops.openRead = func(r *rootFS, p string) (*os.File, error) {
		if p == "item" {
			close(entered)
			<-release
		}
		return base(r, p)
	}
	done := make(chan error, 1)
	go func() { done <- f.scanWith(ops) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("scan did not reach hash")
	}
	closed := make(chan error, 1)
	go func() { closed <- f.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("Close returned before lease release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("scan deadlocked")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close deadlocked")
	}
}

func TestStagedScanRebindWaitsForLease(t *testing.T) {
	f := rootScanFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	ops := f.rootScanIO()
	base := ops.openRead
	ops.openRead = func(r *rootFS, p string) (*os.File, error) {
		if p == "item" {
			close(entered)
			<-release
		}
		return base(r, p)
	}
	done := make(chan error, 1)
	go func() { done <- f.scanWith(ops) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("scan did not reach hash")
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "other"), []byte("other"), 0600); err != nil {
		close(release)
		t.Fatal(err)
	}
	_ = f.SetConfiguredRoot(other)
	token := f.Blocked().Token
	confirmed := make(chan error, 1)
	go func() { confirmed <- f.Confirm(token, "rebind", other) }()
	select {
	case err := <-confirmed:
		close(release)
		t.Fatalf("confirm returned before lease release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("scan deadlocked")
	}
	select {
	case err := <-confirmed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("confirm deadlocked")
	}
	if fi, _, ok := f.Lookup("other"); !ok || fi.Deleted {
		t.Fatalf("rebind lost file: %+v", fi)
	}
}
