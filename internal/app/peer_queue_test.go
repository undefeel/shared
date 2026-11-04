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
	failed := files[17]
	q.failed("p", "epoch", failed, now)
	if epoch, cursor, ok := q.cursor("p"); !ok || epoch != "epoch" || cursor != 64 {
		t.Fatalf("accepted cursor = %q/%d/%v", epoch, cursor, ok)
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
	if !q.accept("p", "epoch", 2, false, []folder.FileInfo{newer}) {
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

func TestPeerQueueOverflowDoesNotAdvanceCursorOrLoseWork(t *testing.T) {
	q := newPeerQueue()
	files := make([]folder.FileInfo, peerPendingLimit+1)
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
	q.applied("p", "epoch", files[0])
	if !q.canPoll("p") {
		t.Fatal("poll still blocked after capacity freed")
	}
	if !q.accept("p", "epoch", 11, false, files[peerPendingLimit:]) {
		t.Fatal("index not accepted after freeing capacity")
	}
	if _, cursor, _ := q.cursor("p"); cursor != 11 {
		t.Fatalf("cursor after retry = %d, want 11", cursor)
	}
}
