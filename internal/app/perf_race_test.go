package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sharedir/internal/folder"
)

func TestConcurrentConfirmDrainsDownload(t *testing.T) {
	d := folderDaemon(t)
	if err := d.ConfirmFolder(d.folder.Blocked().Token, "reset"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	downloaded := make(chan int, 1)
	d.workers.Add(1)
	d.peers["old"] = &peerRunner{cancel: cancel}
	go func() {
		defer d.workers.Done()
		n, _ := d.folder.ApplyRemote(ctx, "peer", []folder.FileInfo{{Name: "incoming", Size: 3, Hash: fmt.Sprintf("%x", sha256.Sum256([]byte("new"))), Version: folder.Vector{"peer": 1}}}, func(ctx context.Context, _ folder.FileInfo, _ io.Writer) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}, nil)
		downloaded <- n
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	path := filepath.Join(d.folder.Root, "local")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := d.folder.Scan(); err == nil {
		t.Fatal("missing latch")
	}
	token := d.folder.Blocked().Token
	gate := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-gate; results <- d.ConfirmFolder(token, "reset") }()
	}
	close(gate)
	successes := 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err == nil {
				successes++
			}
		case <-time.After(10 * time.Second):
			t.Fatal("confirmation did not finish")
		}
	}
	if successes != 1 {
		t.Fatalf("successful confirmations: %d", successes)
	}
	select {
	case n := <-downloaded:
		if n != 0 {
			t.Fatalf("stale download applied %d files", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download not drained")
	}
	if _, err := os.Stat(filepath.Join(d.folder.Root, "incoming")); !os.IsNotExist(err) {
		t.Fatalf("stale download exists: %v", err)
	}
	if err := os.WriteFile(path, []byte("new generation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := d.ConfirmFolder(token, "reset"); err == nil {
		t.Fatal("stale token accepted")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "new generation" {
		t.Fatalf("new generation changed: %q %v", contents, err)
	}
	files, _ := d.folder.Snapshot()
	if len(files) != 1 || files[0].Name != "local" || files[0].Deleted {
		t.Fatalf("new generation index changed: %+v", files)
	}
}
