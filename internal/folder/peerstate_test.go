package folder

import (
	"errors"
	"io"
	"path/filepath"
	"testing"
)

func TestPeerStateExactDurableAndIncarnation(t *testing.T) {
	dir := t.TempDir()
	root, state := filepath.Join(dir, "root"), filepath.Join(dir, "index")
	f, err := Open(root, state, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	fi := FileInfo{Name: "gone", Deleted: true, Version: Vector{"me": 2, "other": 4}}
	f.mu.Lock()
	f.files[fi.Name] = &fi
	f.mu.Unlock()
	if err := f.save(); err != nil {
		t.Fatal(err)
	}
	if err := f.SetPeers([]string{"peer"}); err != nil {
		t.Fatal(err)
	}
	epoch, membership, _ := f.PeerACKState()
	if err := f.Acknowledge("peer", epoch, "remote", membership, []FileInfo{fi}, ""); err == nil {
		t.Fatal("unobserved baseline accepted")
	}
	token, err := f.BeginPeerBaseline("peer", "remote", epoch, f.store.state.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Acknowledge("peer", epoch, "remote", membership, []FileInfo{fi}, "bad"); err == nil {
		t.Fatal("bad challenge accepted")
	}
	if err := f.Acknowledge("stranger", epoch, "remote", membership, nil, token); err == nil {
		t.Fatal("unknown peer accepted")
	}
	if err := f.Acknowledge("peer", epoch, "other-epoch", membership, nil, token); err == nil {
		t.Fatal("wrong remote epoch accepted")
	}
	wrongName := fi
	wrongName.Name = "neighbor"
	if err := f.Acknowledge("peer", epoch, "remote", membership, []FileInfo{wrongName}, token); err == nil {
		t.Fatal("neighbor name accepted")
	}
	near := fi
	near.Version = Vector{"me": 2, "other": 3}
	if err := f.Acknowledge("peer", epoch, "remote", membership, []FileInfo{near}, token); err == nil {
		t.Fatal("neighbor vector accepted")
	}
	if err := f.Acknowledge("peer", epoch, "remote", membership, []FileInfo{fi}, token); err != nil {
		t.Fatal(err)
	}
	generation := f.store.state.Generation
	if err := f.Acknowledge("peer", epoch, "remote", membership, []FileInfo{fi}, token); err != nil || f.store.state.Generation != generation {
		t.Fatalf("duplicate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = Open(root, state, "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.store.state.Control.ACK) != 1 || !f.store.state.Control.Members[0].Baseline {
		t.Fatal("ack not replayed")
	}
	if err := f.SetPeers(nil); err != nil {
		t.Fatal(err)
	}
	if err := f.SetPeers([]string{"peer"}); err != nil {
		t.Fatal(err)
	}
	_, next, _ := f.PeerACKState()
	if next == membership || len(f.store.state.Control.ACK) != 0 {
		t.Fatal("incarnation reused")
	}
	if err := f.Acknowledge("peer", epoch, "remote", membership, nil, token); err == nil {
		t.Fatal("stale epoch accepted")
	}
}

func TestPeerStateAppendFailure(t *testing.T) {
	f := rootScanFixture(t)
	original := f.store.write
	f.store.write = func([]byte) (int, error) { return 0, io.ErrClosedPipe }
	before := f.store.state.Control.PeerSetEpoch
	if err := f.SetPeers([]string{"peer"}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("got %v", err)
	}
	if f.store.state.Control.PeerSetEpoch != before || len(f.store.state.Control.Members) != 0 {
		t.Fatal("failed append published")
	}
	f.store.write = original
}
