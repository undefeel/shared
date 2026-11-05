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

func ackDaemon(t *testing.T, base, name string) *Daemon {
	t.Helper()
	home := filepath.Join(base, name)
	root := filepath.Join(home, "root")
	mustMkdirAll(t, root, 0700)
	requireNoError(t, (&config.Config{Name: name, Folder: root}).Save(home))
	d, err := New(home, log.New(io.Discard, "", 0))
	requireNoError(t, err)
	d.folder, err = folder.Open(root, filepath.Join(home, "index.json"), d.ID.ID, d.Log)
	requireNoError(t, err)
	t.Cleanup(func() { _ = d.folder.Close() })
	return d
}

func waitForAck(t *testing.T, check func() bool) {
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

func TestAckConvergesAfterLossAndOfflineMember(t *testing.T) {
	base := t.TempDir()
	source, receiver := ackDaemon(t, base, "source"), ackDaemon(t, base, "receiver")
	receiver.folder.SetPersistenceProbe(folder.PersistenceProbe{ClockNow: func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) }})
	writeTestFile(t, filepath.Join(source.folder.Root, "keep"), []byte("keep"), 0600)
	writeTestFile(t, filepath.Join(source.folder.Root, "gone"), []byte("gone"), 0600)
	mustScan(t, source.folder)
	source.folder.SetPersistenceProbe(folder.PersistenceProbe{ClockNow: func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) }})
	requireNoError(t, os.Remove(filepath.Join(source.folder.Root, "gone")))
	mustScan(t, source.folder)
	requireNoError(t, source.folder.SetPeers([]string{receiver.ID.ID}))
	requireNoError(t, receiver.folder.SetPeers([]string{source.ID.ID}))
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

	waitForAck(t, fetchBlocked.Load)
	if n, err := source.folder.CollectTombstones(time.Now()); err != nil || n != 0 || lost.Load() {
		t.Fatalf("pending apply authorized GC: %d %v", n, err)
	}
	requireNoError(t, source.folder.SetPeers([]string{receiver.ID.ID, "offline"}))
	holdFetch.Store(false)
	waitForAck(t, func() bool { fi, _, ok := receiver.folder.Lookup("gone"); return ok && fi.Deleted && lost.Load() })
	cancel()
	<-done
	requireNoError(t, receiver.folder.Close())
	// Removal and re-pair are a new incarnation even for the same identity.
	requireNoError(t, source.folder.SetPeers([]string{"offline"}))
	requireNoError(t, source.folder.SetPeers([]string{receiver.ID.ID, "offline"}))
	if n, err := source.folder.CollectTombstones(time.Now()); err != nil || n != 0 {
		t.Fatalf("re-pair reused old ACK: %d %v", n, err)
	}
	var err error
	receiver.folder, err = folder.Open(receiver.cfg.Folder, filepath.Join(receiver.Home, "index.json"), receiver.ID.ID, receiver.Log)
	requireNoError(t, err)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	done = make(chan struct{})
	go func() { defer close(done); receiver.runPeer(ctx, runner) }()
	if n, err := source.folder.CollectTombstones(time.Now()); err != nil || n != 0 {
		t.Fatalf("offline member did not block GC: %d %v", n, err)
	}
	requireNoError(t, source.folder.SetPeers([]string{receiver.ID.ID}))
	waitForAck(t, func() bool { _, _, ok := source.folder.Lookup("gone"); return !ok })
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
	waitForAck(t, func() bool { _, _, ok := receiver.folder.Lookup("gone"); return !ok })
}

func TestAckRetryScenarios(t *testing.T) {
	cases := []struct {
		name             string
		status           int
		ignored          string
		source, receiver *Daemon
	}{
		{name: "RetriesDroppedRequestWithoutRemoteChanges", status: http.StatusServiceUnavailable},
		{name: "RefreshesStaleBaseline", status: http.StatusConflict},
		{name: "BaselineAllowsIgnoredLiveFile", status: http.StatusServiceUnavailable, ignored: "keep"},
		{name: "BaselineRequiresIgnoredDeletionCoverage", status: http.StatusServiceUnavailable, ignored: "gone"},
	}
	for i := range cases {
		tc := &cases[i]
		base := t.TempDir()
		tc.source, tc.receiver = ackDaemon(t, base, "source"), ackDaemon(t, base, "receiver")
		if tc.ignored != "" {
			writeTestFile(t, filepath.Join(tc.receiver.folder.Root, ".sharedirignore"), []byte(tc.ignored+"\n"), 0600)
		}
		for _, name := range []string{"keep", "gone"} {
			writeTestFile(t, filepath.Join(tc.source.folder.Root, name), []byte(name), 0600)
		}
	}
	// The folders are independent; scan them concurrently to share their stability wait.
	scan := func(folders []*folder.Folder) {
		errs := make(chan error, len(folders))
		for _, f := range folders {
			go func(f *folder.Folder) { errs <- f.Scan() }(f)
		}
		var firstErr error
		for range folders {
			if err := <-errs; err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if firstErr != nil {
			t.Fatal(firstErr)
		}
	}
	var first []*folder.Folder
	for _, tc := range cases {
		first = append(first, tc.source.folder)
		if tc.ignored != "" {
			first = append(first, tc.receiver.folder)
		}
	}
	scan(first)
	var second []*folder.Folder
	for _, tc := range cases {
		tc.receiver.folder.SetPersistenceProbe(folder.PersistenceProbe{ClockNow: func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) }})
		tc.source.folder.SetPersistenceProbe(folder.PersistenceProbe{ClockNow: func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) }})
		requireNoError(t, os.Remove(filepath.Join(tc.source.folder.Root, "gone")))
		second = append(second, tc.source.folder)
	}
	scan(second)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { testAckRetry(t, tc.source, tc.receiver, tc.status, tc.ignored) })
	}
}

func testAckRetry(t *testing.T, source, receiver *Daemon, droppedStatus int, ignored string) {
	requireNoError(t, source.folder.SetPeers([]string{receiver.ID.ID}))
	requireNoError(t, receiver.folder.SetPeers([]string{source.ID.ID}))
	var lost, holdFetch, fetchBlocked atomic.Bool
	var refreshed atomic.Bool
	var polls atomic.Int32
	holdFetch.Store(true)
	allowed := func(id string) bool { return id == receiver.ID.ID }
	handler := (&transport.Server{Folder: source.folder, State: folderPeerState{source.folder}, Allowed: allowed, PollTimeout: 200 * time.Millisecond}).Handler()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path == "/files" || r.URL.Path == "/file") && holdFetch.Load() {
			fetchBlocked.Store(true)
			http.Error(w, "retry", 503)
			return
		}
		if r.URL.Path == "/index" {
			polls.Add(1)
		}
		if r.URL.Path == "/index" && lost.Load() && r.URL.Query().Get("epoch") == "" {
			refreshed.Store(true)
		}
		if r.URL.Path == "/ack" && !lost.Swap(true) {
			if droppedStatus == http.StatusConflict {
				_, epoch, cursor := source.folder.DurableState()
				remoteEpoch, _ := receiver.folder.PeerACKEpoch()
				if _, err := source.folder.BeginPeerBaseline(receiver.ID.ID, remoteEpoch, epoch, cursor); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				handler.ServeHTTP(w, r)
				return
			}
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

	if ignored != "keep" {
		waitForAck(t, fetchBlocked.Load)
	}
	holdFetch.Store(false)
	if ignored == "gone" {
		waitForAck(t, func() bool { return polls.Load() >= 2 })
		if lost.Load() {
			t.Fatal("uncovered ignored deletion authorized an ACK")
		}
		if n, err := source.folder.CollectTombstones(time.Now()); err != nil || n != 0 {
			t.Fatalf("uncovered ignored deletion collected: %d %v", n, err)
		}
		if _, _, ok := source.folder.Lookup("gone"); !ok {
			t.Fatal("uncovered ignored deletion disappeared")
		}
		return
	}
	waitForAck(t, lost.Load)
	waitForAck(t, func() bool { _, _, ok := source.folder.Lookup("gone"); return !ok })
	if droppedStatus == http.StatusConflict && !refreshed.Load() {
		t.Fatal("stale ACK did not request a fresh full baseline")
	}
}
