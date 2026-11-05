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

func queueFiles(prefix string, n int) []folder.FileInfo {
	files := make([]folder.FileInfo, n)
	for i := range files {
		files[i] = queueInfo(fmt.Sprintf("%s%05d", prefix, i), 1)
	}
	return files
}

func requireCursorUnavailable(t testing.TB, q *peerQueue, peer, message string) {
	t.Helper()
	if _, _, ok := q.cursor(peer); ok {
		t.Fatal(message)
	}
}

func requireQueueDueCount(t testing.TB, q *peerQueue, peer string, now time.Time, want int, message string) {
	t.Helper()
	got := q.due(peer, now, 0)
	if len(got) != want {
		t.Fatalf("%s: got %d want %d", message, len(got), want)
	}
}

func requireQueueCursor(t testing.TB, q *peerQueue, peer, wantEpoch string, wantCursor uint64, checkEpoch bool, message string) {
	t.Helper()
	epoch, cursor, ok := q.cursor(peer)
	if !ok || cursor != wantCursor || (checkEpoch && epoch != wantEpoch) {
		t.Fatalf("%s: %q/%d/%v, want %q/%d", message, epoch, cursor, ok, wantEpoch, wantCursor)
	}
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
	requireCursorUnavailable(t, q, "p", "cursor advanced before queued records were applied")
	if q.canPoll("p") {
		t.Fatal("poll allowed during retry backoff for an already accepted index")
	}
	requireQueueDueCount(t, q, "p", now, 63, "immediately due")
	delay := peerRetryDelay(1)
	if delay < peerRetryMin || delay > 300*time.Millisecond {
		t.Fatalf("first backoff = %v", delay)
	}
	requireQueueDueCount(t, q, "p", now.Add(300*time.Millisecond), 64, "due after backoff")
	for _, fi := range files {
		q.applied("p", "epoch", fi)
	}
	requireQueueCursor(t, q, "p", "epoch", 64, true, "applied cursor")
	for attempt := uint(1); attempt <= 7; attempt++ {
		d := peerRetryDelay(attempt)
		base := peerRetryMin << (attempt - 1)
		if d < base || d > base+base/5 {
			t.Fatalf("backoff(%d) = %v", attempt, d)
		}
	}
	for attempt := uint(8); attempt <= 32; attempt++ {
		d := peerRetryDelay(attempt)
		if d < 24*time.Second || d > 30*time.Second {
			t.Fatalf("capped backoff(%d) = %v, want 24s..30s", attempt, d)
		}
	}
	if d := peerRetryDelay(2); d < 500*time.Millisecond || d > 600*time.Millisecond {
		t.Fatalf("second retry did not back off exponentially: %v", d)
	}
	if d := peerRetryDelay(3); d < time.Second || d > 1200*time.Millisecond {
		t.Fatalf("third retry did not back off exponentially: %v", d)
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
	if !q.accept("p", "epoch", 2, false, []folder.FileInfo{old, newer, old, newer}) {
		t.Fatal("newer index not accepted")
	}
	got := q.due("p", now, 0)
	if len(got) != 1 || got[0].Hash != newer.Hash || got[0].Version.Compare(newer.Version) != folder.Equal {
		t.Fatalf("pending after replacement = %#v", got)
	}
	q.applied("p", "epoch", old) // stale completion must not delete newer work
	requireEqual(t, len(q.due("p", now, 0)), 1, "stale completion removed newer version")
	q.applied("p", "epoch", newer)
	requireEqual(t, len(q.due("p", now, 0)), 0, "durably applied version remains pending")
	requireQueueCursor(t, q, "p", "epoch", 2, true, "cursor after duplicate records")
}

func TestPeerQueueOverflowContinuationAppliesEachRecordOnce(t *testing.T) {
	q := newPeerQueue()
	files := queueFiles("f", 2*peerPendingLimit+1)
	if q.accept("p", "epoch", 11, false, files) {
		t.Fatal("oversized index accepted before all records fit")
	}
	if q.canPoll("p") {
		t.Fatal("poll allowed while accepted work fills the queue")
	}
	requireCursorUnavailable(t, q, "p", "cursor advanced on incomplete index")
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
			requireEqualf(t, seen[fi.Name], 1, "%s queued/applied %d times", fi.Name, seen[fi.Name])
			q.applied("p", "epoch", fi)
		}
		if len(seen) < len(files) {
			if q.canPoll("p") == false {
				t.Fatal("poll remained blocked after capacity was released")
			}
			reset := cycle == 1
			accepted := q.accept("p", "epoch", 11, reset, files)
			wantAccepted := len(files)-len(seen) <= peerPendingLimit
			requireEqualf(t, accepted, wantAccepted, "continuation accepted = %v, want %v with %d unapplied records", accepted, wantAccepted, len(files)-len(seen))
			requireCursorUnavailable(t, q, "p", "cursor advanced before the continuation was applied")
		}
	}
	if cycle < 3 {
		t.Fatalf("only %d capacity-release cycles, want several", cycle)
	}
	requireQueueCursor(t, q, "p", "", 11, false, "cursor after applying continuation")
	requireQueueDueCount(t, q, "p", time.Now(), 0, "applied records remained queued")
	q.mu.Lock()
	if len(q.pending) > peerPendingLimit || q.indexes["p"].remaining != 0 {
		q.mu.Unlock()
		t.Fatalf("completed window retained record metadata: pending=%d continuation=%+v", len(q.pending), q.indexes["p"])
	}
	q.mu.Unlock()
	if !q.accept("p", "epoch", 11, true, files) {
		t.Fatal("replayed completed index was not accepted")
	}
	requireQueueDueCount(t, q, "p", time.Now(), 0, "completed replay re-queued applied records")
}

func TestPeerQueueContinuationUsesNewerVector(t *testing.T) {
	q := newPeerQueue()
	files := queueFiles("f", peerPendingLimit+1)
	if q.accept("p", "epoch", 12, false, files) {
		t.Fatal("oversized index unexpectedly accepted")
	}
	newer := queueInfo(files[0].Name, 2)
	files[0] = newer
	if q.accept("p", "epoch", 13, true, files) {
		t.Fatal("continuation unexpectedly fit before releasing capacity")
	}
	got := q.due("p", time.Now(), 0)
	requireEqualf(t, len(got), peerPendingLimit, "pending after same-index reset replay = %d, want %d", len(got), peerPendingLimit)
	for _, fi := range got {
		if fi.Name == newer.Name && !samePeerVersion(fi, newer) {
			t.Fatalf("pending vector for %q = %#v, want newer %#v", fi.Name, fi, newer)
		}
		q.applied("p", "epoch", fi)
	}
	requireCursorUnavailable(t, q, "p", "cursor advanced while oversized suffix remained")
	if !q.accept("p", "epoch", 13, false, files) {
		t.Fatal("final suffix record was not accepted")
	}
	last := q.due("p", time.Now(), 0)
	if len(last) != 1 || last[0].Name != files[len(files)-1].Name {
		t.Fatalf("continuation suffix = %#v, want only %q", last, files[len(files)-1].Name)
	}
	q.applied("p", "epoch", last[0])
	requireQueueCursor(t, q, "p", "", 13, false, "cursor after newer vector continuation")
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
	requireCursorUnavailable(t, q, "p", "stale cursor remained visible before the new epoch was applied")
}

func TestPeerQueueEpochResetUnblocksOtherPeer(t *testing.T) {
	q := newPeerQueue()
	full := queueFiles("a", peerPendingLimit)
	if !q.accept("a", "epoch-1", 1, false, full) {
		t.Fatal("full peer index not accepted")
	}
	if q.accept("b", "epoch", 1, false, []folder.FileInfo{queueInfo("b", 1)}) {
		t.Fatal("peer b should be blocked by peer a's full queue")
	}
	if q.canPoll("b") {
		t.Fatal("peer b unexpectedly pollable while the queue is full")
	}
	if !q.accept("a", "epoch-2", 2, false, nil) {
		t.Fatal("empty new-epoch index not accepted")
	}
	if !q.canPoll("b") {
		t.Fatal("peer b remained blocked after epoch reset drained the queue")
	}
}
