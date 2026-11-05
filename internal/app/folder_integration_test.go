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

func testBlockedDaemonDoesNotServeIndexIntegration(t *testing.T, d *Daemon) {
	h := (&transport.Server{Folder: d.folder}).Handler()
	for _, path := range []string{"/index", "/file?name=item"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 503 || strings.Contains(r.Body.String(), `"files"`) {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
		}
	}
}
func TestConfirmRejectsOldDownload(t *testing.T) {
	d := folderDaemon(t)
	requireNoError(t, d.ConfirmFolder(d.folder.Blocked().Token, "reset"))
	for _, tc := range []struct {
		name        string
		honorCancel bool
	}{{"cancelled", true}, {"lateWrite", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(d.ctx)
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			result := make(chan int, 1)
			content := "incoming"
			hash := sha256.Sum256([]byte(content))
			d.workers.Add(1)
			d.peers["old"] = &peerRunner{cancel: cancel}
			go func() {
				defer d.workers.Done()
				n, _ := d.folder.ApplyRemote(ctx, "peer", []folder.FileInfo{{Name: "incoming", Size: int64(len(content)), Hash: fmt.Sprintf("%x", hash), Version: folder.Vector{"peer": 1}}}, func(ctx context.Context, _ folder.FileInfo, w io.Writer) error {
					close(started)
					<-release
					if tc.honorCancel {
						return ctx.Err()
					}
					_, err := io.WriteString(w, content)
					return err
				}, nil)
				result <- n
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("download not started")
			}
			path := filepath.Join(d.folder.Root, "local")
			writeTestFile(t, path, []byte("keep"), 0600)
			mustScan(t, d.folder)
			requireNoError(t, os.Remove(path))
			if err := d.folder.Scan(); err == nil {
				t.Fatal("missing latch")
			}
			go func() { <-ctx.Done(); close(release) }()
			requireNoError(t, d.ConfirmFolder(d.folder.Blocked().Token, "reset"))
			if n := <-result; n != 0 {
				t.Fatal("stale download committed", n)
			}
			requireMissingFile(t, filepath.Join(d.folder.Root, "incoming"), "stale bytes installed")
			if files, _ := d.folder.Snapshot(); len(files) != 0 {
				t.Fatal("stale index", files)
			}
			requireEqual(t, d.Status().Files, 0, "old runner committed")
		})
	}
}
