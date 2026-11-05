package folder

import (
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
