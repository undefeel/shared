package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"golang.org/x/sys/unix"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"sharedir/internal/config"
	"sharedir/internal/folder"
)

func folderDaemon(t *testing.T) *Daemon {
	t.Helper()
	home := t.TempDir()
	cfg := &config.Config{Folder: filepath.Join(home, "root")}
	if err := cfg.Save(home); err != nil {
		t.Fatal(err)
	}
	d, err := New(home, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	d.ctx = context.Background()
	d.folder, err = folder.Open(cfg.Folder, filepath.Join(home, "index.json"), d.ID.Short(), d.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.lifecycle.Lock(); defer d.lifecycle.Unlock(); d.stopFolder(); d.folder.Close() })
	if err = os.WriteFile(filepath.Join(cfg.Folder, "item"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = d.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(cfg.Folder, "item")); err != nil {
		t.Fatal(err)
	}
	if err = d.folder.Scan(); err == nil {
		t.Fatal("expected latch")
	}
	return d
}
func TestBlockedDaemonStatus(t *testing.T) {
	d := folderDaemon(t)
	r := httptest.NewRecorder()
	d.controlHandler().ServeHTTP(r, httptest.NewRequest("GET", "/status", nil))
	if r.Code != 200 || !bytes.Contains(r.Body.Bytes(), []byte(`"folder_error"`)) {
		t.Fatal(r.Body.String())
	}
}
func TestPauseResumePreservesLatch(t *testing.T) {
	d := folderDaemon(t)
	token := d.folder.Blocked().Token
	for _, paused := range []bool{true, false} {
		if err := d.SetPaused(paused); err != nil {
			t.Fatal(err)
		}
	}
	if d.folder.Blocked().Token != token {
		t.Fatal("latch changed")
	}
}
func TestRestartPreservesLatch(t *testing.T) {
	d := folderDaemon(t)
	token := d.folder.Blocked().Token
	d.folder.Close()
	f, err := folder.Open(d.Config().Folder, filepath.Join(d.Home, "index.json"), d.ID.Short(), d.Log)
	if err != nil {
		t.Fatal(err)
	}
	d.folder = f
	if f.Blocked().Token != token {
		t.Fatal("restart cleared latch")
	}
}
func TestReloadNewRoot(t *testing.T) {
	d := folderDaemon(t)
	cfg := d.Config()
	cfg.Folder = t.TempDir()
	if err := cfg.Save(d.Home); err != nil {
		t.Fatal(err)
	}
	if err := d.Reload(); err != nil {
		t.Fatal(err)
	}
	if d.Status().Folder != cfg.Folder || d.folder.Root != cfg.Folder || d.folder.Blocked() == nil {
		t.Fatal(d.Status())
	}
}
func TestControlStaleToken409(t *testing.T) {
	d := folderDaemon(t)
	r := httptest.NewRecorder()
	d.controlHandler().ServeHTTP(r, httptest.NewRequest("POST", "/folder/confirm", bytes.NewBufferString(`{"token":"stale","action":"reset"}`)))
	if r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
}
func TestCtlResetSemantics(t *testing.T) {
	d := folderDaemon(t)
	srv := httptest.NewServer(d.controlHandler())
	defer srv.Close()
	ctl := &Ctl{http: srv.Client()}
	// Ctl uses a fixed host; route its requests to this handler.
	ctl.http.Transport = handlerTransport{d.controlHandler()}
	if err := ctl.ConfirmFolder(d.folder.Blocked().Token, "reset"); err != nil {
		t.Fatal(err)
	}
	if d.Status().FolderError != nil || d.Status().Files != 0 {
		t.Fatal(d.Status())
	}
}
func TestControlRebindSemantics(t *testing.T) {
	d := folderDaemon(t)
	if err := d.ConfirmFolder(d.folder.Blocked().Token, "rebind"); err != nil {
		t.Fatal(err)
	}
	files, _ := d.folder.Snapshot()
	if len(files) != 1 || !files[0].Deleted {
		t.Fatal(files)
	}
}
func TestOldRunnerCannotCommitAfterConfirm(t *testing.T) {
	d := folderDaemon(t)
	if err := d.ConfirmFolder(d.folder.Blocked().Token, "reset"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(d.ctx)
	started, release := make(chan struct{}), make(chan struct{})
	applied := make(chan int, 1)
	hash := sha256.Sum256([]byte("incoming"))
	d.workers.Add(1)
	d.peers["old"] = &peerRunner{cancel: cancel}
	go func() {
		defer d.workers.Done()
		n, _ := d.folder.ApplyRemote(ctx, "peer", []folder.FileInfo{{Name: "incoming", Size: 8, Hash: hex.EncodeToString(hash[:]), Version: folder.Vector{"peer": 1}}}, func(ctx context.Context, fi folder.FileInfo, w io.Writer) error {
			close(started)
			<-release
			_, err := io.WriteString(w, "incoming")
			return err
		}, nil)
		applied <- n
	}()
	<-started
	if err := os.WriteFile(filepath.Join(d.Config().Folder, "dummy"), []byte("dummy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(d.Config().Folder, "dummy")); err != nil {
		t.Fatal(err)
	}
	_ = d.folder.Scan()
	token := d.folder.Blocked().Token
	go func() { <-ctx.Done(); close(release) }()
	if err := d.ConfirmFolder(token, "reset"); err != nil {
		t.Fatal(err)
	}
	if n := <-applied; n != 0 {
		t.Fatal("old runner applied", n)
	}
	if d.Status().Files != 0 {
		t.Fatal("old runner committed")
	}
}

type handlerTransport struct{ h http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w.Result(), nil
}

func TestRunClosesRootAfterFailure(t *testing.T) {
	d := folderDaemon(t)
	d.folder.Close()
	cfg := d.Config()
	cfg.Port = -1
	if err := cfg.Save(d.Home); err != nil {
		t.Fatal(err)
	}
	d.cfg = &cfg
	before := rootDescriptors(t, cfg.Folder)
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("expected invalid listen port")
	}
	if after := rootDescriptors(t, cfg.Folder); after != before {
		t.Fatalf("root descriptors %d -> %d", before, after)
	}
}
func rootDescriptors(t *testing.T, path string) int {
	t.Helper()
	var root unix.Stat_t
	if err := unix.Stat(path, &root); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	entries, err := dir.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry)
		if err != nil {
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && st.Dev == root.Dev && st.Ino == root.Ino {
			count++
		}
	}
	return count
}

func TestBlockedRunServesStatusAndClosesRoot(t *testing.T) {
	d := folderDaemon(t)
	d.folder.Close()
	cfg := d.Config()
	cfg.Port = 0
	if err := cfg.Save(d.Home); err != nil {
		t.Fatal(err)
	}
	d.cfg = &cfg
	before := rootDescriptors(t, cfg.Folder)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	ctl := NewCtl(d.Home)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := ctl.Status()
		if err == nil {
			if st.FolderError == nil {
				t.Fatal("missing latch")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if after := rootDescriptors(t, cfg.Folder); after != before {
		t.Fatalf("root descriptors %d -> %d", before, after)
	}
}
