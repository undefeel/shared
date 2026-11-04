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

func TestV2PeerPipelineUsesBatchAndSingleFileStreams(t *testing.T) {
	base := t.TempDir()
	openFolder := func(tag string) *folder.Folder {
		t.Helper()
		root := filepath.Join(base, tag, "root")
		state := filepath.Join(base, tag, "state", "index.json")
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(state), 0700); err != nil {
			t.Fatal(err)
		}
		f, err := folder.Open(root, state, tag, log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	source, destination := openFolder("source"), openFolder("destination")
	serverHome, clientHome := filepath.Join(base, "server-id"), filepath.Join(base, "client-id")
	if err := os.MkdirAll(serverHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(clientHome, 0700); err != nil {
		t.Fatal(err)
	}
	serverID, err := identity.LoadOrCreate(serverHome)
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := identity.LoadOrCreate(clientHome)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(source.Root, fmt.Sprintf("small-%d", i)), []byte(strings.Repeat("s", 1024+i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	largeBody := strings.Repeat("large-body-", 40000)
	if err := os.WriteFile(filepath.Join(source.Root, "large"), []byte(largeBody), 0600); err != nil {
		t.Fatal(err)
	}
	if err := source.Scan(); err != nil {
		t.Fatal(err)
	}
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
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverID.Cert}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
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
	// Advance the large record after its index snapshot; the old hash must
	// trigger an exact-name refresh, then retry with the returned version.
	largeBody = strings.Repeat("updated-large-body-", 30000)
	if err := os.WriteFile(filepath.Join(source.Root, "large"), []byte(largeBody), 0600); err != nil {
		t.Fatal(err)
	}
	if err := source.Scan(); err != nil {
		t.Fatal(err)
	}
	due := queue.due(serverID.ID, time.Now(), 64)
	d := &Daemon{folder: destination}
	destination.SetDownloadConcurrency(2)
	got, failed := d.applyPeerDue(context.Background(), client, addr, serverID.ID, clientID.Short(), epoch, queue.activeCursor(serverID.ID), due, queue, nil)
	if got != 3 || failed != 1 {
		t.Fatalf("applied=%d failed=%d", got, failed)
	}
	if _, _, ok := queue.cursor(serverID.ID); ok {
		t.Fatal("cursor advanced before changed large body retry")
	}
	if batches.Load() != 1 || singles.Load() != 1 || namedRefreshes.Load() != 1 || fullIndexes.Load() != 0 {
		t.Fatalf("transport paths: /files=%d /file=%d", batches.Load(), singles.Load())
	}
	due = queue.due(serverID.ID, time.Now(), 64)
	if len(due) != 1 || due[0].Name != "large" {
		t.Fatalf("retry queue=%#v", due)
	}
	got, failed = d.applyPeerDue(context.Background(), client, addr, serverID.ID, clientID.Short(), epoch, queue.activeCursor(serverID.ID), due, queue, nil)
	if got != 1 || failed != 0 {
		t.Fatalf("retry applied=%d failed=%d", got, failed)
	}
	if batches.Load() != 1 || singles.Load() != 2 || namedRefreshes.Load() != 1 || fullIndexes.Load() != 0 {
		t.Fatalf("retries batches=%d singles=%d named=%d full=%d", batches.Load(), singles.Load(), namedRefreshes.Load(), fullIndexes.Load())
	}
	for _, fi := range remote {
		data, err := os.ReadFile(filepath.Join(destination.Root, fi.Name))
		if err != nil {
			t.Fatal(err)
		}
		wantSize := fi.Size
		if fi.Name == "large" {
			wantSize = int64(len(largeBody))
		}
		if int64(len(data)) != wantSize {
			t.Fatalf("%s size=%d want %d", fi.Name, len(data), wantSize)
		}
	}
	if _, appliedCursor, ok := queue.cursor(serverID.ID); !ok || appliedCursor != cursor {
		t.Fatalf("queue cursor=%d/%v", appliedCursor, ok)
	}
}

func TestRunPeerRetriesOnlyFailedBodyWithoutBaselineRefetch(t *testing.T) {
	base := t.TempDir()
	open := func(tag string) *folder.Folder {
		root, state := filepath.Join(base, tag, "root"), filepath.Join(base, tag, "state", "index.json")
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(state), 0700); err != nil {
			t.Fatal(err)
		}
		f, err := folder.Open(root, state, tag, log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	source, destination := open("source"), open("destination")
	serverHome, clientHome := filepath.Join(base, "server-id"), filepath.Join(base, "client-id")
	_ = os.MkdirAll(serverHome, 0700)
	_ = os.MkdirAll(clientHome, 0700)
	serverID, err := identity.LoadOrCreate(serverHome)
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := identity.LoadOrCreate(clientHome)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("z", 300<<10)
	for _, name := range []string{"bad", "good-a", "good-b"} {
		if err := os.WriteFile(filepath.Join(source.Root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Scan(); err != nil {
		t.Fatal(err)
	}
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
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverID.Cert}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "https://")
	second := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index" {
			indexes.Add(1)
			if r.URL.Query().Get("epoch") == "" {
				baseline.Add(1)
			}
		}
		if r.URL.Path == "/file" && r.URL.Query().Get("name") == "bad" {
			badRetry.Add(1)
		}
		api.ServeHTTP(w, r)
	}))
	second.TLS = &tls.Config{Certificates: []tls.Certificate{serverID.Cert}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	second.StartTLS()
	defer second.Close()
	secondAddr := strings.TrimPrefix(second.URL, "https://")
	cfg := &config.Config{Folder: destination.Root, Port: 0, Peers: []config.Peer{{ID: serverID.ID, Name: "remote", Addrs: []string{addr, secondAddr}}}}
	d := &Daemon{Home: base, ID: clientID, cfg: cfg, folder: destination, Log: log.New(io.Discard, "", 0), act: loadActivity(filepath.Join(base, "activity.json"))}
	destination.SetDownloadConcurrency(1)
	ctx, cancel := context.WithCancel(context.Background())
	pr := &peerRunner{st: PeerStatus{ID: serverID.ID, Name: "remote"}}
	done := make(chan struct{})
	go func() { defer close(done); d.runPeer(ctx, pr) }()
	deadline := time.Now().Add(15 * time.Second)
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
		got, err := os.ReadFile(filepath.Join(destination.Root, name))
		if err != nil || string(got) != body {
			t.Fatalf("%s body not durably applied: err=%v bytes=%d", name, err, len(got))
		}
	}
}
