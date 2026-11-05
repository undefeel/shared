package folder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func fakeTracker() (*stabilityTracker, *time.Time) {
	now := time.Unix(100, 0)
	s := newStabilityTracker()
	s.now = func() time.Time { return now }
	return s, &now
}

func TestStabilityExactIntervalAndRetainedReady(t *testing.T) {
	s, now := fakeTracker()
	stat := scanStat{dev: 1, ino: 2, size: 3, mtime: 4, mode: 0100755}
	if s.probe("a", stat) || s.pending() != 1 {
		t.Fatal("first observation must be pending")
	}
	*now = now.Add(time.Second - time.Nanosecond)
	if s.probe("a", stat) {
		t.Fatal("probe became ready before one second")
	}
	*now = now.Add(time.Nanosecond)
	if !s.probe("a", stat) || s.pending() != 0 {
		t.Fatal("probe should be ready at exactly one second")
	}
	if !s.probe("a", stat) {
		t.Fatal("unchanged ready evidence should persist")
	}
}

func TestStabilityChangesResetDeadline(t *testing.T) {
	for _, mutate := range []func(*scanStat){
		func(s *scanStat) { s.size++ },
		func(s *scanStat) { s.mtime++ },
		func(s *scanStat) { s.ino++ },
		func(s *scanStat) { s.dev++ },
		func(s *scanStat) { s.mode ^= 0100 },
		func(s *scanStat) { s.ctime++ },
	} {
		s, now := fakeTracker()
		stat := scanStat{ino: 1}
		s.probe("a", stat)
		*now = now.Add(time.Second)
		if !s.probe("a", stat) {
			t.Fatal("initial probe not ready")
		}
		mutated := stat
		mutate(&mutated)
		if s.probe("a", mutated) || s.pending() != 1 {
			t.Fatal("changed metadata must reset stability")
		}
	}
}

func TestStabilityOverflowBoundAndReconcile(t *testing.T) {
	s, _ := fakeTracker()
	for i := 0; i < maxStabilityProbes; i++ {
		s.probe(string(rune(i+1)), scanStat{ino: uint64(i + 1)})
	}
	if len(s.probes) != maxStabilityProbes {
		t.Fatalf("retained %d probes", len(s.probes))
	}
	if s.probe("overflow", scanStat{}) || !s.needsFullRescan() || len(s.probes) != maxStabilityProbes {
		t.Fatal("overflow must be bounded and request a full scan")
	}
	present := map[string]struct{}{"keep": {}}
	s.reconcile(present)
	if s.needsFullRescan() || len(s.probes) != 0 {
		t.Fatal("reconcile did not prune and clear overflow")
	}
	if s.probe("after", scanStat{}) || len(s.probes) != 1 {
		t.Fatal("tracker did not resume after reconcile")
	}
}

func TestFolderStabilityAccessors(t *testing.T) {
	f := &Folder{stability: newStabilityTracker()}
	if f.PendingStability() != 0 || f.StabilityFullRescanNeeded() {
		t.Fatal("unexpected initial state")
	}
	f.stability.probe("pending", scanStat{})
	if f.PendingStability() != 1 {
		t.Fatal("pending count not exposed")
	}
}

func TestEndlessWriterAndOverflowProgress(t *testing.T) {
	s, now := fakeTracker()
	for i := 0; i < 20; i++ {
		*now = now.Add(time.Second)
		if s.probe("writer", scanStat{size: int64(i)}) || s.pending() != 1 {
			t.Fatal("writer became stable")
		}
	}
	s, now = fakeTracker()
	for i := 0; i < maxStabilityProbes+1; i++ {
		s.probe(fmt.Sprint(i), scanStat{ino: uint64(i)})
	}
	if !s.needsFullRescan() || s.pending() != maxStabilityProbes {
		t.Fatal("overflow unbounded")
	}
	*now = now.Add(time.Second)
	for i := 0; i < maxStabilityProbes; i++ {
		if !s.probe(fmt.Sprint(i), scanStat{ino: uint64(i)}) {
			t.Fatal("tracked probe blocked")
		}
	}
	s.probe(fmt.Sprint(maxStabilityProbes), scanStat{ino: maxStabilityProbes})
	*now = now.Add(time.Second)
	if !s.probe(fmt.Sprint(maxStabilityProbes), scanStat{ino: maxStabilityProbes}) {
		t.Fatal("overflow cannot progress")
	}
}

func TestUnstableScanPreservesWholeIndex(t *testing.T) {
	n := newDefaultNode(t, "A")
	defer n.f.Close()
	n.write(t, "item", "old", time.Now())
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	before, seq := n.f.Snapshot()
	tracker, now := fakeTracker()
	n.f.stability = tracker
	// First probe is metadata collection; mutate immediately before the locked pass.
	ops := n.f.rootScanIO()
	info := ops.info
	calls := 0
	ops.info = func(d *os.File, e os.DirEntry) (os.FileInfo, scanStat, error) {
		if e.Name() == "item" {
			calls++
			if calls == 2 {
				if err := os.WriteFile(filepath.Join(n.f.Root, "item"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return info(d, e)
	}
	file, err := n.f.root.openRead("item")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := file.Stat()
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	tracker.probe("item", scanStatFromInfo(meta))
	*now = now.Add(time.Second)
	if err := n.f.scanWith(ops); !errors.Is(err, ErrScanPending) {
		t.Fatal("want pending", err)
	}
	after, afterSeq := n.f.Snapshot()
	if !reflect.DeepEqual(before, after) || seq != afterSeq || n.f.PendingStability() != 1 {
		t.Fatal("unstable scan published")
	}
	*now = now.Add(time.Second)
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	after, _ = n.f.Snapshot()
	if after[0].Hash == before[0].Hash {
		t.Fatal("retry lost change")
	}
}

func TestStabilityWaitCancellation(t *testing.T) {
	n := newDefaultNode(t, "A")
	defer n.f.Close()
	n.write(t, "item", "x", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.f.preflightStability(ctx, n.f.rootScanIO()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestOverflowDropsDisappearedPending(t *testing.T) {
	s, _ := fakeTracker()
	for i := 0; i < maxStabilityProbes; i++ {
		s.probe(fmt.Sprint(i), scanStat{})
	}
	s.probe("new", scanStat{})
	s.forgetAbsent(map[string]scanStat{"new": {}})
	if s.pending() != 0 || !s.needsFullRescan() {
		t.Fatal("absent probe cleanup lost overflow state")
	}
	s.probe("new", scanStat{})
	if s.pending() != 1 {
		t.Fatal("stale pending prevents recovery")
	}
}
