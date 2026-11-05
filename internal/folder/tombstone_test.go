package folder

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestTombstoneRetentionAndReplay(t *testing.T) {
	dir := t.TempDir()
	root, state := filepath.Join(dir, "root"), filepath.Join(dir, "index")
	f, err := Open(root, state, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	fi := FileInfo{Name: "gone", Deleted: true, Version: Vector{"me": 3, "other": 7}}
	f.mu.Lock()
	f.files[fi.Name] = &fi
	f.mu.Unlock()
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	observed := time.Unix(0, f.store.state.Files[0].DeletedAt)
	if err := f.SetPeers([]string{"peer"}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.CollectTombstones(observed.Add(90 * 24 * time.Hour)); err != nil || n != 0 {
		t.Fatalf("offline: %d %v", n, err)
	}
	epoch, member, _ := f.PeerACKState()
	token, err := f.BeginPeerBaseline("peer", "remote", epoch, f.store.state.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Acknowledge("peer", epoch, "remote", member, []FileInfo{fi}, token); err != nil {
		t.Fatal(err)
	}
	if n, err := f.CollectTombstones(observed.Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("rollback: %d %v", n, err)
	}
	if n, err := f.CollectTombstones(observed.Add(tombstoneRetention - time.Nanosecond)); err != nil || n != 0 {
		t.Fatalf("early: %d %v", n, err)
	}
	if n, err := f.CollectTombstones(observed.Add(tombstoneRetention)); err != nil || n != 1 {
		t.Fatalf("collect: %d %v", n, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = Open(root, state, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, failed := f.ApplyRemote(context.Background(), "peer", []FileInfo{fi}, nil, nil); failed != 0 {
		t.Fatal("stale tombstone failed")
	}
	if _, _, ok := f.Lookup(fi.Name); ok {
		t.Fatal("retired tombstone resurrected")
	}
	f.scanMu.Lock()
	f.saveMu.Lock()
	old := fi
	old.Deleted = false
	old.Version = Vector{"me": 2, "other": 7}
	newer := fi
	newer.Version = Vector{"me": 4, "other": 7}
	concurrent := fi
	concurrent.Version = Vector{"me": 4, "other": 6}
	if !f.retiredCoversLocked(old) || f.retiredCoversLocked(newer) || f.retiredCoversLocked(concurrent) {
		t.Fatal("incorrect retirement coverage")
	}
	f.saveMu.Unlock()
	f.scanMu.Unlock()
}

func TestReplacementDeletionRestartsRetention(t *testing.T) {
	f := rootScanFixture(t)
	first := FileInfo{Name: "gone", Deleted: true, Version: Vector{"me": 1}}
	f.mu.Lock()
	f.files[first.Name] = &first
	f.mu.Unlock()
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	var observed int64
	for _, v := range f.store.state.Files {
		if v.Name == first.Name {
			observed = v.DeletedAt
		}
	}
	future := time.Unix(0, observed).Add(5 * time.Hour)
	f.persistHooks.probe.ClockNow = func() time.Time { return future }
	replacement := first
	replacement.Version = Vector{"me": 2}
	f.mu.Lock()
	f.files[first.Name] = &replacement
	f.mu.Unlock()
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	for _, v := range f.store.state.Files {
		if v.Name == first.Name && v.DeletedAt != future.UnixNano() {
			t.Fatal("replacement inherited observation time")
		}
	}
	if n, err := f.CollectTombstones(time.Unix(0, observed).Add(tombstoneRetention)); err != nil || n != 0 {
		t.Fatalf("collected replacement early: %d %v", n, err)
	}
}

func TestCollectorAppendFailureDoesNotPublish(t *testing.T) {
	f := rootScanFixture(t)
	fi := FileInfo{Name: "gone", Deleted: true, Version: Vector{"me": 2}}
	f.mu.Lock()
	f.files[fi.Name] = &fi
	f.mu.Unlock()
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	if err := f.SetPeers([]string{"peer"}); err != nil {
		t.Fatal(err)
	}
	epoch, member, _ := f.PeerACKState()
	token, err := f.BeginPeerBaseline("peer", "remote", epoch, f.store.state.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Acknowledge("peer", epoch, "remote", member, []FileInfo{fi}, token); err != nil {
		t.Fatal(err)
	}
	f.store.write = func([]byte) (int, error) { return 0, io.ErrClosedPipe }
	if n, err := f.CollectTombstones(time.Now().Add(31 * 24 * time.Hour)); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("%d %v", n, err)
	}
	if _, _, ok := f.Lookup(fi.Name); !ok || len(f.store.state.Control.Retired) != 0 {
		t.Fatal("failed GC was published")
	}
}
