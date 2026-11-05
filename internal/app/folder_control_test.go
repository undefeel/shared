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
	requireNoError(t, cfg.Save(home))
	d, err := New(home, log.New(io.Discard, "", 0))
	requireNoError(t, err)
	d.ctx = context.Background()
	d.folder, err = folder.Open(cfg.Folder, filepath.Join(home, "index.json"), d.ID.Short(), d.Log)
	requireNoError(t, err)
	t.Cleanup(func() { d.lifecycle.Lock(); defer d.lifecycle.Unlock(); d.stopFolder(); d.folder.Close() })
	writeTestFile(t, filepath.Join(cfg.Folder, "item"), []byte("old"), 0600)
	mustScan(t, d.folder)
	requireNoError(t, os.Remove(filepath.Join(cfg.Folder, "item")))
	if err = d.folder.Scan(); err == nil {
		t.Fatal("expected latch")
	}
	return d
}

func TestBlockedFolderStatusAndLatch(t *testing.T) {
	d := folderDaemon(t)
	t.Run("BlockedDaemonDoesNotServeIndexIntegration", func(t *testing.T) { testBlockedDaemonDoesNotServeIndexIntegration(t, d) })
	t.Run("BlockedDaemonStatus", func(t *testing.T) {
		r := httptest.NewRecorder()
		d.controlHandler().ServeHTTP(r, httptest.NewRequest("GET", "/status", nil))
		if r.Code != 200 || !bytes.Contains(r.Body.Bytes(), []byte(`"folder_error"`)) {
			t.Fatal(r.Body.String())
		}
	})
	t.Run("PauseResumePreservesLatch", func(t *testing.T) {
		token := d.folder.Blocked().Token
		for _, paused := range []bool{true, false} {
			requireNoError(t, d.SetPaused(paused))
		}
		requireEqual(t, d.folder.Blocked().Token, token, "latch changed")
	})
	t.Run("RestartPreservesLatch", func(t *testing.T) {
		token := d.folder.Blocked().Token
		d.folder.Close()
		f, err := folder.Open(d.Config().Folder, filepath.Join(d.Home, "index.json"), d.ID.Short(), d.Log)
		requireNoError(t, err)
		d.folder = f
		requireEqual(t, f.Blocked().Token, token, "restart cleared latch")
	})
	t.Run("ControlStaleToken409", func(t *testing.T) {
		r := httptest.NewRecorder()
		d.controlHandler().ServeHTTP(r, httptest.NewRequest("POST", "/folder/confirm", bytes.NewBufferString(`{"token":"stale","action":"reset"}`)))
		requireEqual(t, r.Code, 409, r.Code, r.Body.String())
	})
}
func TestReloadNewRoot(t *testing.T) {
	d := folderDaemon(t)
	cfg := d.Config()
	cfg.Folder = t.TempDir()
	requireNoError(t, cfg.Save(d.Home))
	requireNoError(t, d.Reload())
	if d.Status().Folder != cfg.Folder || d.folder.Root != cfg.Folder || d.folder.Blocked() == nil {
		t.Fatal(d.Status())
	}
}

func TestCtlResetSemantics(t *testing.T) {
	d := folderDaemon(t)
	srv := httptest.NewServer(d.controlHandler())
	defer srv.Close()
	ctl := &Ctl{http: srv.Client()}
	// Ctl uses a fixed host; route its requests to this handler.
	ctl.http.Transport = handlerTransport{d.controlHandler()}
	requireNoError(t, ctl.ConfirmFolder(d.folder.Blocked().Token, "reset"))
	if d.Status().FolderError != nil || d.Status().Files != 0 {
		t.Fatal(d.Status())
	}
}
func TestControlRebindSemantics(t *testing.T) {
	d := folderDaemon(t)
	requireNoError(t, d.ConfirmFolder(d.folder.Blocked().Token, "rebind"))
	files, _ := d.folder.Snapshot()
	if len(files) != 1 || !files[0].Deleted {
		t.Fatal(files)
	}
}
func TestStopFolderCancelsAndDrainsBlockedDownload(t *testing.T) {
	d := folderDaemon(t)
	requireNoError(t, d.ConfirmFolder(d.folder.Blocked().Token, "reset"))
	keep := filepath.Join(d.folder.Root, "existing")
	writeTestFile(t, keep, []byte("prior data"), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	started, observedCancel, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	pr := &peerRunner{cancel: cancel, st: PeerStatus{ID: "peer"}}
	d.peers["peer"] = pr
	hash := sha256.Sum256([]byte("incoming"))
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		defer close(done)
		_, _ = d.folder.ApplyRemote(ctx, "peer", []folder.FileInfo{{Name: "late", Size: 8, Hash: hex.EncodeToString(hash[:]), Version: folder.Vector{"peer": 1}}}, func(ctx context.Context, _ folder.FileInfo, _ io.Writer) error {
			close(started)
			<-ctx.Done()
			close(observedCancel)
			return ctx.Err()
		}, nil)
	}()
	<-started
	d.lifecycle.Lock()
	d.stopFolder()
	d.lifecycle.Unlock()
	select {
	case <-observedCancel:
	default:
		t.Fatal("runner cancellation was not drained")
	}
	select {
	case <-done:
	default:
		t.Fatal("runner returned before stopFolder completed")
	}
	requireMissingFile(t, filepath.Join(d.Config().Folder, "late"), "late mutation remains")
	requireFileContents(t, keep, []byte("prior data"), "existing data after canceled download")
	if _, failed := d.folder.ApplyRemote(context.Background(), "peer", []folder.FileInfo{{Name: "resume", Size: 8, Hash: hex.EncodeToString(hash[:]), Version: folder.Vector{"peer": 1}}}, func(_ context.Context, _ folder.FileInfo, w io.Writer) error {
		_, err := w.Write([]byte("incoming"))
		return err
	}, nil); failed != 0 {
		t.Fatalf("resume failed: %d", failed)
	}
}

func TestPauseDrainsReadyCoordinatorGroup(t *testing.T) {
	d := folderDaemon(t)
	requireNoError(t, d.ConfirmFolder(d.folder.Blocked().Token, "reset"))
	ctx, cancel := context.WithCancel(context.Background())
	doneRunner := make(chan struct{})
	d.peers["peer"] = &peerRunner{cancel: cancel, done: doneRunner, st: PeerStatus{ID: "peer"}}
	body := []byte("ready-group-body")
	hash := sha256.Sum256(body)
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		defer close(doneRunner)
		_, _ = d.folder.ApplyRemote(ctx, "peer", []folder.FileInfo{{Name: "paused-ready", Size: int64(len(body)), Hash: hex.EncodeToString(hash[:]), Version: folder.Vector{"peer": 1}}}, func(_ context.Context, _ folder.FileInfo, w io.Writer) error {
			_, err := w.Write(body)
			return err
		}, nil)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for d.folder.PendingApplyCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("ready group was not queued at coordinator")
		}
		time.Sleep(time.Millisecond)
	}
	requireNoError(t, d.SetPaused(true))
	requireMissingFile(t, filepath.Join(d.Config().Folder, "paused-ready"), "ready group mutated folder after pause")
}

func TestUnpairWaitsForRunnerDrainWithoutDaemonLock(t *testing.T) {
	d := folderDaemon(t)
	id := d.ID.ID
	d.mu.Lock()
	d.cfg.Peers = append(d.cfg.Peers, config.Peer{ID: id, Name: "temporary"})
	d.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	pr := &peerRunner{cancel: cancel, done: runnerDone, st: PeerStatus{ID: id}}
	d.peers[id] = pr
	started := make(chan struct{})
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		<-ctx.Done()
		close(started)
		time.Sleep(20 * time.Millisecond)
		close(runnerDone)
	}()
	requireNoError(t, d.Unpair(id))
	select {
	case <-started:
	default:
		t.Fatal("unpair returned before runner cancellation/drain")
	}
	select {
	case <-runnerDone:
	default:
		t.Fatal("unpair returned before runner done channel")
	}
}

func TestUnpairPausedPeerWithoutRunnerDoesNotWait(t *testing.T) {
	d := folderDaemon(t)
	id := d.ID.ID
	d.mu.Lock()
	d.cfg.Paused = true
	d.cfg.Peers = append(d.cfg.Peers, config.Peer{ID: id, Name: "paused"})
	d.mu.Unlock()
	d.syncPeers()
	d.mu.Lock()
	pr := d.peers[id]
	d.mu.Unlock()
	if pr == nil || pr.done != nil {
		t.Fatalf("paused peer has runner completion channel: %#v", pr)
	}
	done := make(chan error, 1)
	go func() { done <- d.Unpair(id) }()
	select {
	case err := <-done:
		requireNoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("unpair waited for a runner that was never started")
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
	requireNoError(t, cfg.Save(d.Home))
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
	requireNoError(t, unix.Stat(path, &root))
	dir, err := os.Open("/dev/fd")
	requireNoError(t, err)
	defer dir.Close()
	entries, err := dir.Readdirnames(-1)
	requireNoError(t, err)
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
	requireNoError(t, cfg.Save(d.Home))
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
	requireNoError(t, <-done)
	// FSEvents can release a root descriptor shortly after its stop returns.
	for after := rootDescriptors(t, cfg.Folder); after != before; after = rootDescriptors(t, cfg.Folder) {
		if time.Now().After(deadline) {
			t.Fatalf("root descriptors %d -> %d", before, after)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
