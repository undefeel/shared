package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sharedir/internal/folder"
	"sharedir/internal/transport"
	"strings"
	"testing"
	"time"
)

func TestBlockedDaemonDoesNotServeIndexIntegration(t *testing.T) {
	d := folderDaemon(t)
	h := (&transport.Server{Folder: d.folder}).Handler()
	for _, path := range []string{"/index", "/file?name=item"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 503 || strings.Contains(r.Body.String(), `"files"`) {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
		}
	}
}
func TestConfirmDuringDownloadIntegration(t *testing.T) {
	d := folderDaemon(t)
	if err := d.ConfirmFolder(d.folder.Blocked().Token, "reset"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	result := make(chan int, 1)
	d.workers.Add(1)
	d.peers["old"] = &peerRunner{cancel: cancel}
	go func() {
		defer d.workers.Done()
		n, _ := d.folder.ApplyRemote(ctx, "peer", []folder.FileInfo{{Name: "incoming", Size: 3, Hash: fmt.Sprintf("%x", sha256.Sum256([]byte("new"))), Version: folder.Vector{"peer": 1}}}, func(context.Context, folder.FileInfo, io.Writer) error {
			close(started)
			<-release
			return ctx.Err()
		}, nil)
		result <- n
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download not started")
	}
	if err := os.WriteFile(filepath.Join(d.folder.Root, "local"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(d.folder.Root, "local")); err != nil {
		t.Fatal(err)
	}
	if err := d.folder.Scan(); err == nil {
		t.Fatal("missing latch")
	}
	go func() { <-ctx.Done(); close(release) }()
	if err := d.ConfirmFolder(d.folder.Blocked().Token, "reset"); err != nil {
		t.Fatal(err)
	}
	if n := <-result; n != 0 {
		t.Fatal("stale download committed", n)
	}
	if _, err := os.Stat(filepath.Join(d.folder.Root, "incoming")); !os.IsNotExist(err) {
		t.Fatal("stale bytes installed", err)
	}
	if files, _ := d.folder.Snapshot(); len(files) != 0 {
		t.Fatal("stale index", files)
	}
}
