package app

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sharedir/internal/config"
	"sharedir/internal/folder"
	"sharedir/internal/transport"
)

func TestAckConvergesAfterLossAndOfflineMember(t *testing.T) {
	base := t.TempDir()
	open := func(name string) *Daemon {
		home := filepath.Join(base, name)
		root := filepath.Join(home, "root")
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := (&config.Config{Name: name, Folder: root}).Save(home); err != nil {
			t.Fatal(err)
		}
		d, err := New(home, log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		d.folder, err = folder.Open(root, filepath.Join(home, "index.json"), d.ID.ID, d.Log)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.folder.Close() })
		return d
	}
	source, receiver := open("source"), open("receiver")
	receiver.folder.SetPersistenceProbe(folder.PersistenceProbe{ClockNow: func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) }})
	if err := os.WriteFile(filepath.Join(source.folder.Root, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.folder.Root, "gone"), []byte("gone"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := source.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	source.folder.SetPersistenceProbe(folder.PersistenceProbe{ClockNow: func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) }})
	if err := os.Remove(filepath.Join(source.folder.Root, "gone")); err != nil {
		t.Fatal(err)
	}
	if err := source.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := source.folder.SetPeers([]string{receiver.ID.ID}); err != nil {
		t.Fatal(err)
	}
	if err := receiver.folder.SetPeers([]string{source.ID.ID}); err != nil {
		t.Fatal(err)
	}
	var lost, holdFetch, fetchBlocked atomic.Bool
	holdFetch.Store(true)
	allowed := func(id string) bool { return id == receiver.ID.ID }
	handler := (&transport.Server{Folder: source.folder, State: folderPeerState{source.folder}, Allowed: allowed, PollTimeout: 200 * time.Millisecond}).Handler()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path == "/files" || r.URL.Path == "/file") && holdFetch.Load() {
			fetchBlocked.Store(true)
			http.Error(w, "retry", 503)
			return
		}
		if r.URL.Path == "/ack" && !lost.Swap(true) {
			http.Error(w, "lost", 503)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	srv.TLS = transport.ServerTLS(source.ID, allowed, func(string) {})
	srv.StartTLS()
	defer srv.Close()
	receiver.cfg.Peers = []config.Peer{{ID: source.ID.ID, Name: "source", Addrs: []string{strings.TrimPrefix(srv.URL, "https://")}}}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &peerRunner{lan: make(chan struct{}, 1), st: PeerStatus{ID: source.ID.ID, Name: "source"}}
	done := make(chan struct{})
	go func() { defer close(done); receiver.runPeer(ctx, runner) }()
	defer func() { cancel(); <-done }()
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("timed out waiting for ACK convergence")
	}
	wait(fetchBlocked.Load)
	if n, err := source.folder.CollectTombstones(time.Now()); err != nil || n != 0 || lost.Load() {
		t.Fatalf("pending apply authorized GC: %d %v", n, err)
	}
	if err := source.folder.SetPeers([]string{receiver.ID.ID, "offline"}); err != nil {
		t.Fatal(err)
	}
	holdFetch.Store(false)
	wait(func() bool { fi, _, ok := receiver.folder.Lookup("gone"); return ok && fi.Deleted && lost.Load() })
	cancel()
	<-done
	if err := receiver.folder.Close(); err != nil {
		t.Fatal(err)
	}
	// Removal and re-pair are a new incarnation even for the same identity.
	if err := source.folder.SetPeers([]string{"offline"}); err != nil {
		t.Fatal(err)
	}
	if err := source.folder.SetPeers([]string{receiver.ID.ID, "offline"}); err != nil {
		t.Fatal(err)
	}
	if n, err := source.folder.CollectTombstones(time.Now()); err != nil || n != 0 {
		t.Fatalf("re-pair reused old ACK: %d %v", n, err)
	}
	var err error
	receiver.folder, err = folder.Open(receiver.cfg.Folder, filepath.Join(receiver.Home, "index.json"), receiver.ID.ID, receiver.Log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	done = make(chan struct{})
	go func() { defer close(done); receiver.runPeer(ctx, runner) }()
	if n, err := source.folder.CollectTombstones(time.Now()); err != nil || n != 0 {
		t.Fatalf("offline member did not block GC: %d %v", n, err)
	}
	if err := source.folder.SetPeers([]string{receiver.ID.ID}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { _, _, ok := source.folder.Lookup("gone"); return !ok })
	// The second daemon can collect too, even if its peer retired the same
	// deletion before receiving this reverse baseline.
	reverseAllowed := func(id string) bool { return id == source.ID.ID }
	reverseHandler := (&transport.Server{Folder: receiver.folder, State: folderPeerState{receiver.folder}, Allowed: reverseAllowed, PollTimeout: 200 * time.Millisecond}).Handler()
	reverse := httptest.NewUnstartedServer(reverseHandler)
	reverse.TLS = transport.ServerTLS(receiver.ID, reverseAllowed, func(string) {})
	reverse.StartTLS()
	defer reverse.Close()
	source.cfg.Peers = []config.Peer{{ID: receiver.ID.ID, Name: "receiver", Addrs: []string{strings.TrimPrefix(reverse.URL, "https://")}}}
	reverseCtx, reverseCancel := context.WithCancel(context.Background())
	reverseRunner := &peerRunner{lan: make(chan struct{}, 1), st: PeerStatus{ID: receiver.ID.ID, Name: "receiver"}}
	reverseDone := make(chan struct{})
	go func() { defer close(reverseDone); source.runPeer(reverseCtx, reverseRunner) }()
	defer func() { reverseCancel(); <-reverseDone }()
	wait(func() bool { _, _, ok := receiver.folder.Lookup("gone"); return !ok })
}

func TestAckRetriesDroppedRequestWithoutRemoteChanges(t *testing.T) {
	testAckRetry(t, http.StatusServiceUnavailable)
}

func TestAckRefreshesStaleBaseline(t *testing.T) {
	testAckRetry(t, http.StatusConflict)
}

func testAckRetry(t *testing.T, droppedStatus int) {
	base := t.TempDir()
	open := func(name string) *Daemon {
		home := filepath.Join(base, name)
		root := filepath.Join(home, "root")
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := (&config.Config{Name: name, Folder: root}).Save(home); err != nil {
			t.Fatal(err)
		}
		d, err := New(home, log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		d.folder, err = folder.Open(root, filepath.Join(home, "index.json"), d.ID.ID, d.Log)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.folder.Close() })
		return d
	}
	source, receiver := open("source"), open("receiver")
	receiver.folder.SetPersistenceProbe(folder.PersistenceProbe{ClockNow: func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) }})
	if err := os.WriteFile(filepath.Join(source.folder.Root, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.folder.Root, "gone"), []byte("gone"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := source.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	source.folder.SetPersistenceProbe(folder.PersistenceProbe{ClockNow: func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) }})
	if err := os.Remove(filepath.Join(source.folder.Root, "gone")); err != nil {
		t.Fatal(err)
	}
	if err := source.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := source.folder.SetPeers([]string{receiver.ID.ID}); err != nil {
		t.Fatal(err)
	}
	if err := receiver.folder.SetPeers([]string{source.ID.ID}); err != nil {
		t.Fatal(err)
	}
	var lost, holdFetch, fetchBlocked atomic.Bool
	holdFetch.Store(true)
	allowed := func(id string) bool { return id == receiver.ID.ID }
	handler := (&transport.Server{Folder: source.folder, State: folderPeerState{source.folder}, Allowed: allowed, PollTimeout: 200 * time.Millisecond}).Handler()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path == "/files" || r.URL.Path == "/file") && holdFetch.Load() {
			fetchBlocked.Store(true)
			http.Error(w, "retry", 503)
			return
		}
		if r.URL.Path == "/ack" && !lost.Swap(true) {
			http.Error(w, "lost", droppedStatus)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	srv.TLS = transport.ServerTLS(source.ID, allowed, func(string) {})
	srv.StartTLS()
	defer srv.Close()
	receiver.cfg.Peers = []config.Peer{{ID: source.ID.ID, Name: "source", Addrs: []string{strings.TrimPrefix(srv.URL, "https://")}}}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &peerRunner{lan: make(chan struct{}, 1), st: PeerStatus{ID: source.ID.ID, Name: "source"}}
	done := make(chan struct{})
	go func() { defer close(done); receiver.runPeer(ctx, runner) }()
	defer func() { cancel(); <-done }()
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("timed out waiting for ACK convergence")
	}
	wait(fetchBlocked.Load)
	holdFetch.Store(false)
	wait(lost.Load)
	wait(func() bool { _, _, ok := source.folder.Lookup("gone"); return !ok })
}
