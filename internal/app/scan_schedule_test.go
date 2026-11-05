package app

import (
	"fmt"
	"testing"
	"time"

	"sharedir/internal/folder"
)

func TestScanQueueCoalescesAndDrains(t *testing.T) {
	var q scanQueue
	q.merge(folder.WatchBatch{Paths: []string{"a", "b"}, Generation: 3})
	q.merge(folder.WatchBatch{Paths: []string{"b", "c"}, Generation: 3})
	b := q.drain()
	if len(b.Paths) != 3 || b.Generation != 3 {
		t.Fatalf("batch = %+v", b)
	}
	if b := q.drain(); len(b.Paths) != 0 {
		t.Fatalf("queue not drained: %+v", b)
	}
}

func TestScanQueueOverflowAndFull(t *testing.T) {
	var q scanQueue
	paths := make([]string, maxWatchPaths+1)
	for i := range paths {
		paths[i] = fmt.Sprintf("path-%d", i)
	}
	q.merge(folder.WatchBatch{Paths: paths, Generation: 7})
	b := q.drain()
	if !b.FullRequired || len(b.Paths) != 0 || b.Generation != 7 {
		t.Fatalf("overflow = %+v", b)
	}
	q.merge(folder.WatchBatch{Paths: []string{"x"}, Generation: 8})
	q.merge(folder.WatchBatch{FullRequired: true, Generation: 8})
	if !q.drain().FullRequired {
		t.Fatal("full request lost")
	}
}

func TestScanQueueUnknownRenameRequiresFull(t *testing.T) {
	var q scanQueue
	q.merge(folder.WatchBatch{Paths: []string{"dir", "dir/new"}, RenamePaths: []string{"dir/new"}, RenameFull: true, FullRequired: true, Generation: 7})
	b := q.drain()
	if !b.FullRequired || !b.RenameFull || len(b.RenamePaths) != 1 || b.RenamePaths[0] != "dir/new" {
		t.Fatalf("ambiguous rename = %+v", b)
	}
}

func TestScanQueueHardFullCannotBeFilteredByRename(t *testing.T) {
	overflow := make([]string, maxWatchPaths+1)
	for i := range overflow {
		overflow[i] = fmt.Sprintf("overflow-%d", i)
	}
	for _, prior := range []folder.WatchBatch{
		{Paths: overflow, Generation: 7},
		{FullRequired: true, Generation: 7},
	} {
		var q scanQueue
		q.merge(prior)
		q.merge(folder.WatchBatch{Paths: []string{"parent", "new"}, RenamePaths: []string{"new"}, RenameFull: true, FullRequired: true, Generation: 7})
		b := q.drain()
		if !b.FullRequired || b.RenameFull || b.RootChanged {
			t.Fatalf("hard full downgraded by rename: %+v", b)
		}
	}
}

func TestScanQueueBoundsRenamePaths(t *testing.T) {
	paths := make([]string, maxWatchPaths+1)
	for i := range paths {
		paths[i] = fmt.Sprintf("rename-%d", i)
	}
	var q scanQueue
	q.merge(folder.WatchBatch{RenamePaths: paths, RenameFull: true, FullRequired: true, Generation: 9})
	b := q.drain()
	if !b.FullRequired || b.RenameFull || len(b.RenamePaths) != 0 {
		t.Fatalf("rename overflow = %+v", b)
	}
}

func TestScanDeadlineIgnoresTrafficAndSwitchesHealth(t *testing.T) {
	now := time.Unix(100, 0)
	var d scanDeadline
	d.set(now, true)
	first := d.at
	d.set(now.Add(time.Minute), true)
	if d.at != first {
		t.Fatal("traffic reset periodic deadline")
	}
	if d.due(first.Add(-time.Nanosecond)) || !d.due(first) {
		t.Fatal("healthy deadline mismatch")
	}
	d.set(now.Add(2*time.Minute), false)
	if !d.due(now.Add(3 * time.Minute)) {
		t.Fatal("unhealthy watcher did not use fallback interval")
	}
}

func TestScanQueueRetainsEventsArrivingDuringScan(t *testing.T) {
	var q scanQueue
	q.merge(folder.WatchBatch{Paths: []string{"before"}, Generation: 11})
	first := q.drain() // executor owns this immutable batch
	q.merge(folder.WatchBatch{Paths: []string{"during"}, Generation: 11})
	if len(first.Paths) != 1 || first.Paths[0] != "before" {
		t.Fatalf("drained batch mutated: %+v", first)
	}
	next := q.drain()
	if len(next.Paths) != 1 || next.Paths[0] != "during" {
		t.Fatalf("event lost during scan: %+v", next)
	}
}

func TestScanDeadlineMissedIntervalsCatchUp(t *testing.T) {
	now := time.Unix(500, 0)
	var d scanDeadline
	d.set(now, true)
	d.advance(now.Add(40 * time.Minute))
	if d.due(now.Add(40*time.Minute)) || !d.at.After(now.Add(40*time.Minute)) {
		t.Fatalf("deadline failed to catch up: %v", d.at)
	}
}
