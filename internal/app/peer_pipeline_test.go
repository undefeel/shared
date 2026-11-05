package app

import (
	"context"
	"crypto/tls"
	"fmt"
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
	"sharedir/internal/identity"
	"sharedir/internal/transport"
)

func testOpenFolder(t *testing.T, root, state, device string) *folder.Folder {
	t.Helper()
	for _, dir := range []string{root, filepath.Dir(state)} {
		mustMkdirAll(t, dir, 0700)
	}
	f, err := folder.Open(root, state, device, log.New(io.Discard, "", 0))
	requireNoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func testIdentity(t *testing.T, path string) *identity.Identity {
	t.Helper()
	mustMkdirAll(t, path, 0700)
	id, err := identity.LoadOrCreate(path)
	requireNoError(t, err)
	return id
}

func testTLSServer(t *testing.T, handler http.Handler, id *identity.Identity) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{id.Cert}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func TestV2PeerPipelineUsesBatchAndSingleFileStreams(t *testing.T) {
	base := t.TempDir()
	open := func(tag string) *folder.Folder {
		return testOpenFolder(t, filepath.Join(base, tag, "root"), filepath.Join(base, tag, "state", "index.json"), tag)
	}
	source, destination := open("source"), open("destination")
	serverID := testIdentity(t, filepath.Join(base, "server-id"))
	clientID := testIdentity(t, filepath.Join(base, "client-id"))
	for i := 0; i < 3; i++ {
		writeTestFile(t, filepath.Join(source.Root, fmt.Sprintf("small-%d", i)), []byte(strings.Repeat("s", 1024+i)), 0600)
	}
	largeBody := strings.Repeat("large-body-", 40000)
	writeTestFile(t, filepath.Join(source.Root, "large"), []byte(largeBody), 0600)
	mustScan(t, source)
	api := (&transport.Server{Folder: source, State: folderPeerState{source}}).Handler()
	var batches, singles, namedRefreshes, fullIndexes atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			batches.Add(1)
		}
		if r.URL.Path == "/file" {
			singles.Add(1)
		}
		if r.URL.Path == "/index" {
			if r.URL.Query().Get("name") != "" {
				namedRefreshes.Add(1)
			} else {
				fullIndexes.Add(1)
			}
		}
		api.ServeHTTP(w, r)
	})
	server := testTLSServer(t, handler, serverID)
	addr := strings.TrimPrefix(server.URL, "https://")
	client := transport.NewClient(clientID, serverID.ID, 0, nil)
	defer client.Close()
	client.LocalHello = func() transport.Hello {
		return transport.Hello{Protocol: 2, StoreEpoch: "client", PeerSetEpoch: "test", Features: []string{"files", "gzip"}}
	}
	remote, _, _ := source.DurableState()
	_, epoch, cursor := source.DurableState()
	queue := newPeerQueue()
	if !queue.accept(serverID.ID, epoch, cursor, false, remote) {
		t.Fatal("index rejected")
	}
	// Delete one batch-sized and one single-file record after the index
	// snapshot. Both unavailable paths must refresh by exact name and apply the
	// returned tombstones before the accepted cursor can settle.
	requireNoError(t, os.Remove(filepath.Join(source.Root, "small-0")))
	requireNoError(t, os.Remove(filepath.Join(source.Root, "large")))
	mustScan(t, source)
	due := queue.due(serverID.ID, time.Now(), 64)
	d := &Daemon{folder: destination}
	destination.SetDownloadConcurrency(2)
	got, failed := d.applyPeerDue(context.Background(), client, addr, serverID.ID, clientID.Short(), epoch, queue.activeCursor(serverID.ID), due, queue, nil)
	if got != 2 || failed != 2 {
		t.Fatalf("applied=%d failed=%d", got, failed)
	}
	if _, _, ok := queue.cursor(serverID.ID); ok {
		t.Fatal("cursor advanced before changed large body retry")
	}
	if batches.Load() != 1 || singles.Load() != 1 || namedRefreshes.Load() != 2 || fullIndexes.Load() != 0 {
		t.Fatalf("transport paths: /files=%d /file=%d", batches.Load(), singles.Load())
	}
	due = queue.due(serverID.ID, time.Now(), 64)
	requireEqualf(t, len(due), 2, "retry queue=%#v", due)
	got, failed = d.applyPeerDue(context.Background(), client, addr, serverID.ID, clientID.Short(), epoch, queue.activeCursor(serverID.ID), due, queue, nil)
	if got != 2 || failed != 0 {
		t.Fatalf("retry applied=%d failed=%d", got, failed)
	}
	if batches.Load() != 1 || singles.Load() != 1 || namedRefreshes.Load() != 2 || fullIndexes.Load() != 0 {
		t.Fatalf("retries batches=%d singles=%d named=%d full=%d", batches.Load(), singles.Load(), namedRefreshes.Load(), fullIndexes.Load())
	}
	for _, fi := range remote {
		if fi.Name == "small-0" || fi.Name == "large" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(destination.Root, fi.Name))
		requireNoError(t, err)
		wantSize := fi.Size
		if fi.Name == "large" {
			wantSize = int64(len(largeBody))
		}
		requireEqualf(t, int64(len(data)), wantSize, "%s size=%d want %d", fi.Name, len(data), wantSize)
	}
	if _, appliedCursor, ok := queue.cursor(serverID.ID); !ok || appliedCursor != cursor {
		t.Fatalf("queue cursor=%d/%v", appliedCursor, ok)
	}
	for _, name := range []string{"small-0", "large"} {
		requireMissingFile(t, filepath.Join(destination.Root, name), "deleted record remains")
	}
}

func TestRunPeerRetriesOnlyFailedBodyWithoutBaselineRefetch(t *testing.T) {
	base := t.TempDir()
	open := func(tag string) *folder.Folder {
		return testOpenFolder(t, filepath.Join(base, tag, "root"), filepath.Join(base, tag, "state", "index.json"), tag)
	}
	source, destination := open("source"), open("destination")
	serverID := testIdentity(t, filepath.Join(base, "server-id"))
	clientID := testIdentity(t, filepath.Join(base, "client-id"))
	body := strings.Repeat("z", 300<<10)
	for _, name := range []string{"bad", "good-a", "good-b"} {
		writeTestFile(t, filepath.Join(source.Root, name), []byte(body+name), 0600)
	}
	mustScan(t, source)
	var bad, badRetry, goodA, goodB, baseline, indexes atomic.Int32
	api := (&transport.Server{Folder: source, State: folderPeerState{source}}).Handler()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			indexes.Add(1)
			if r.URL.Query().Get("epoch") == "" {
				baseline.Add(1)
			}
		}
		if r.URL.Path == "/file" {
			switch r.URL.Query().Get("name") {
			case "bad":
				if bad.Add(1) == 1 {
					http.Error(w, "temporary", 500)
					return
				}
			case "good-a":
				goodA.Add(1)
			case "good-b":
				goodB.Add(1)
			}
		}
		api.ServeHTTP(w, r)
	})
	server := testTLSServer(t, handler, serverID)
	addr := strings.TrimPrefix(server.URL, "https://")
	retryGate := make(chan struct{})
	retryWaiting := make(chan struct{}, 1)
	defer func() {
		select {
		case <-retryGate:
		default:
			close(retryGate)
		}
	}()
	second := testTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			indexes.Add(1)
			if r.URL.Query().Get("epoch") == "" {
				baseline.Add(1)
			}
		}
		if r.URL.Path == "/file" && r.URL.Query().Get("name") == "bad" {
			badRetry.Add(1)
			select {
			case retryWaiting <- struct{}{}:
			default:
			}
			select {
			case <-retryGate:
			case <-r.Context().Done():
				return
			}
		}
		api.ServeHTTP(w, r)
	}), serverID)
	secondAddr := strings.TrimPrefix(second.URL, "https://")
	cfg := &config.Config{Folder: destination.Root, Port: 0, Peers: []config.Peer{{ID: serverID.ID, Name: "remote", Addrs: []string{addr, secondAddr}}}}
	d := &Daemon{Home: base, ID: clientID, cfg: cfg, folder: destination, Log: log.New(io.Discard, "", 0), act: loadActivity(filepath.Join(base, "activity.json"))}
	destination.SetDownloadConcurrency(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr := &peerRunner{st: PeerStatus{ID: serverID.ID, Name: "remote"}}
	done := make(chan struct{})
	go func() { defer close(done); d.runPeer(ctx, pr) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(15 * time.Second)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-retryWaiting:
		if fi, _, ok := destination.Lookup("bad"); ok && fi.Deleted {
			t.Fatal("transient body failure created a tombstone")
		}
		close(retryGate)
	case <-timer.C:
		cancel()
		<-done
		t.Fatal("retry did not start")
	}
	for time.Now().Before(deadline) {
		if bad.Load() == 1 && badRetry.Load() == 1 && goodA.Load() == 1 && goodB.Load() == 1 && baseline.Load() == 1 && indexes.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPeer did not stop")
	}
	if bad.Load() != 1 || badRetry.Load() != 1 || goodA.Load() != 1 || goodB.Load() != 1 || baseline.Load() != 1 || indexes.Load() < 2 {
		t.Fatalf("requests bad=%d retry-address=%d good-a=%d good-b=%d baseline=%d total-index=%d", bad.Load(), badRetry.Load(), goodA.Load(), goodB.Load(), baseline.Load(), indexes.Load())
	}
	for _, name := range []string{"bad", "good-a", "good-b"} {
		requireFileContents(t, filepath.Join(destination.Root, name), []byte(body+name), "body not durably applied")
	}
}
