package app

import (
	"fmt"
	"testing"
	"time"

	"sharedir/internal/folder"
)

func queueInfo(name string, version uint64) folder.FileInfo {
	return folder.FileInfo{Name: name, Hash: fmt.Sprintf("%064x", version), Version: folder.Vector{"peer": version}}
}

func TestPeerQueueFailureDoesNotRepeatAcceptedIndex(t *testing.T) {
	q := newPeerQueue()
	files := make([]folder.FileInfo, 64)
	for i := range files {
		files[i] = queueInfo(fmt.Sprintf("f%02d", i), 1)
	}
	now := time.Unix(100, 0)
	if !q.accept("p", "epoch", 64, false, files) {
		t.Fatal("index not accepted")
	}
	if q.canPoll("p") {
		t.Fatal("poll allowed while the accepted index still has queued work")
	}
	failed := files[17]
	q.failed("p", "epoch", failed, now)
	if _, _, ok := q.cursor("p"); ok {
		t.Fatal("cursor advanced before queued records were applied")
	}
	if q.canPoll("p") {
		t.Fatal("poll allowed during retry backoff for an already accepted index")
	}
	if got := q.due("p", now, 0); len(got) != 63 {
		t.Fatalf("immediately due = %d, want 63", len(got))
	}
	delay := peerRetryDelay(1)
	if delay < peerRetryMin || delay > 300*time.Millisecond {
		t.Fatalf("first backoff = %v", delay)
	}
	if got := q.due("p", now.Add(300*time.Millisecond), 0); len(got) != 64 {
		t.Fatalf("due after backoff = %d, want 64", len(got))
	}
	for _, fi := range files {
		q.applied("p", "epoch", fi)
	}
	if epoch, cursor, ok := q.cursor("p"); !ok || epoch != "epoch" || cursor != 64 {
		t.Fatalf("applied cursor = %q/%d/%v", epoch, cursor, ok)
	}
	for attempt := uint(1); attempt < 16; attempt++ {
		d := peerRetryDelay(attempt)
		if d < peerRetryMin || d > peerRetryMax {
			t.Fatalf("backoff(%d) = %v", attempt, d)
		}
	}
}

func TestPeerQueueNewerVersionReplacesPending(t *testing.T) {
	q := newPeerQueue()
	now := time.Unix(200, 0)
	old := queueInfo("a", 1)
	newer := queueInfo("a", 2)
	if !q.accept("p", "epoch", 1, false, []folder.FileInfo{old}) {
		t.Fatal("index not accepted")
	}
	q.failed("p", "epoch", old, now)
	if !q.accept("p", "epoch", 2, false, []folder.FileInfo{old, newer}) {
		t.Fatal("newer index not accepted")
	}
	got := q.due("p", now, 0)
	if len(got) != 1 || got[0].Hash != newer.Hash || got[0].Version.Compare(newer.Version) != folder.Equal {
		t.Fatalf("pending after replacement = %#v", got)
	}
	q.applied("p", "epoch", old) // stale completion must not delete newer work
	if len(q.due("p", now, 0)) != 1 {
		t.Fatal("stale completion removed newer version")
	}
	q.applied("p", "epoch", newer)
	if len(q.due("p", now, 0)) != 0 {
		t.Fatal("durably applied version remains pending")
	}
}

func TestPeerQueueOverflowContinuationAppliesEachRecordOnce(t *testing.T) {
	q := newPeerQueue()
	files := make([]folder.FileInfo, 2*peerPendingLimit+1)
	for i := range files {
		files[i] = queueInfo(fmt.Sprintf("f%05d", i), 1)
	}
	if q.accept("p", "epoch", 11, false, files) {
		t.Fatal("oversized index accepted before all records fit")
	}
	if q.canPoll("p") {
		t.Fatal("poll allowed while accepted work fills the queue")
	}
	if _, _, ok := q.cursor("p"); ok {
		t.Fatal("cursor advanced on incomplete index")
	}
	if got := len(q.due("p", time.Now(), 0)); got != peerPendingLimit {
		t.Fatalf("pending = %d, want %d", got, peerPendingLimit)
	}
	seen := make(map[string]int, len(files))
	cycle := 0
	for len(seen) < len(files) {
		cycle++
		batch := q.due("p", time.Now(), 0)
		if len(batch) == 0 {
			t.Fatalf("cycle %d: no queued work before all %d records applied", cycle, len(files))
		}
		for _, fi := range batch {
			seen[fi.Name]++
			if seen[fi.Name] != 1 {
				t.Fatalf("%s queued/applied %d times", fi.Name, seen[fi.Name])
			}
			q.applied("p", "epoch", fi)
		}
		if len(seen) < len(files) {
			if q.canPoll("p") == false {
				t.Fatal("poll remained blocked after capacity was released")
			}
			reset := cycle == 1
			accepted := q.accept("p", "epoch", 11, reset, files)
			wantAccepted := len(files)-len(seen) <= peerPendingLimit
			if accepted != wantAccepted {
				t.Fatalf("continuation accepted = %v, want %v with %d unapplied records", accepted, wantAccepted, len(files)-len(seen))
			}
			if _, _, ok := q.cursor("p"); ok {
				t.Fatal("cursor advanced before the continuation was applied")
			}
		}
	}
	if cycle < 3 {
		t.Fatalf("only %d capacity-release cycles, want several", cycle)
	}
	if _, cursor, ok := q.cursor("p"); !ok || cursor != 11 {
		t.Fatalf("cursor after applying continuation = %d/%v, want 11/true", cursor, ok)
	}
	if got := q.due("p", time.Now(), 0); len(got) != 0 {
		t.Fatalf("applied records remained queued: %d", len(got))
	}
	if !q.accept("p", "epoch", 11, true, files) {
		t.Fatal("replayed completed index was not accepted")
	}
	if got := q.due("p", time.Now(), 0); len(got) != 0 {
		t.Fatalf("completed replay re-queued %d applied records", len(got))
	}
}

func TestPeerQueueContinuationUsesNewerVector(t *testing.T) {
	q := newPeerQueue()
	files := make([]folder.FileInfo, peerPendingLimit+1)
	for i := range files {
		files[i] = queueInfo(fmt.Sprintf("f%05d", i), 1)
	}
	if q.accept("p", "epoch", 12, false, files) {
		t.Fatal("oversized index unexpectedly accepted")
	}
	newer := queueInfo(files[0].Name, 2)
	files[0] = newer
	if q.accept("p", "epoch", 12, true, files) {
		t.Fatal("continuation unexpectedly fit before releasing capacity")
	}
	got := q.due("p", time.Now(), 0)
	if len(got) != peerPendingLimit {
		t.Fatalf("pending after same-index reset replay = %d, want %d", len(got), peerPendingLimit)
	}
	for _, fi := range got {
		if fi.Name == newer.Name && !samePeerVersion(fi, newer) {
			t.Fatalf("pending vector for %q = %#v, want newer %#v", fi.Name, fi, newer)
		}
		q.applied("p", "epoch", fi)
	}
	if _, _, ok := q.cursor("p"); ok {
		t.Fatal("cursor advanced while oversized suffix remained")
	}
	if !q.accept("p", "epoch", 12, false, files) {
		t.Fatal("final suffix record was not accepted")
	}
	last := q.due("p", time.Now(), 0)
	if len(last) != 1 || last[0].Name != files[len(files)-1].Name {
		t.Fatalf("continuation suffix = %#v, want only %q", last, files[len(files)-1].Name)
	}
	q.applied("p", "epoch", last[0])
	if _, cursor, ok := q.cursor("p"); !ok || cursor != 12 {
		t.Fatalf("cursor after newer vector continuation = %d/%v", cursor, ok)
	}
}

func TestPeerQueueEpochChangeClearsOldCursor(t *testing.T) {
	q := newPeerQueue()
	old := queueInfo("old", 1)
	if !q.accept("p", "epoch-1", 1, false, []folder.FileInfo{old}) {
		t.Fatal("old epoch index not accepted")
	}
	q.applied("p", "epoch-1", old)
	if _, _, ok := q.cursor("p"); !ok {
		t.Fatal("old epoch cursor was not recorded")
	}
	if !q.accept("p", "epoch-2", 1, false, []folder.FileInfo{queueInfo("new", 1)}) {
		t.Fatal("new epoch index not accepted")
	}
	if _, _, ok := q.cursor("p"); ok {
		t.Fatal("stale cursor remained visible before the new epoch was applied")
	}
}
