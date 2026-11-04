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
	var batches, singles atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			batches.Add(1)
		}
		if r.URL.Path == "/file" {
			singles.Add(1)
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
	due := queue.due(serverID.ID, time.Now(), 64)
	d := &Daemon{folder: destination}
	destination.SetDownloadConcurrency(2)
	got, failed := d.applyPeerDue(context.Background(), client, addr, serverID.ID, clientID.Short(), epoch, queue.activeCursor(serverID.ID), due, queue, nil)
	if got != 4 || failed != 0 {
		t.Fatalf("applied=%d failed=%d", got, failed)
	}
	if batches.Load() != 1 || singles.Load() != 1 {
		t.Fatalf("transport paths: /files=%d /file=%d", batches.Load(), singles.Load())
	}
	for _, fi := range remote {
		data, err := os.ReadFile(filepath.Join(destination.Root, fi.Name))
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(data)) != fi.Size {
			t.Fatalf("%s size=%d want %d", fi.Name, len(data), fi.Size)
		}
	}
	if _, appliedCursor, ok := queue.cursor(serverID.ID); !ok || appliedCursor != cursor {
		t.Fatalf("queue cursor=%d/%v", appliedCursor, ok)
	}
}
