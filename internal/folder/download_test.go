package folder

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func downloadRecord(body string, size int64) FileInfo {
	r := incoming(body, Vector{"B": 1}, time.Now().Add(time.Hour).UnixNano())
	r.Size = size
	r.Name = "downloaded"
	return r
}

func assertDownloadFailedCleanly(t *testing.T, n *node, r FileInfo, fetch Fetcher, progress ...Progress) {
	t.Helper()
	before, seq := n.f.Snapshot()
	var p Progress
	if len(progress) != 0 {
		p = progress[0]
	}
	_, failed := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, fetch, p)
	if failed != 1 {
		t.Fatalf("failed = %d, want 1", failed)
	}
	after, afterSeq := n.f.Snapshot()
	if !reflect.DeepEqual(before, after) || seq != afterSeq {
		t.Fatalf("index changed: before=%+v/%d after=%+v/%d", before, seq, after, afterSeq)
	}
	if _, err := os.Lstat(filepath.Join(n.f.Root, r.Name)); !os.IsNotExist(err) {
		t.Fatalf("target should be absent, stat error = %v", err)
	}
	entries, err := os.ReadDir(n.f.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len(tmpPrefix) && entry.Name()[:len(tmpPrefix)] == tmpPrefix {
			t.Fatalf("temporary file was not cleaned: %q", entry.Name())
		}
	}
}

func TestDownloadRejectsNegativeSize(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := downloadRecord("", -1)
	assertDownloadFailedCleanly(t, n, r, func(context.Context, FileInfo, io.Writer) error {
		t.Fatal("invalid size reached Fetcher")
		return nil
	})
}

func TestDownloadAcceptsZeroSize(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := downloadRecord("", 0)
	_, failed := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, bodyFetch(""), nil)
	if failed != 0 {
		t.Fatalf("failed = %d, want 0", failed)
	}
	if b, err := os.ReadFile(filepath.Join(n.f.Root, r.Name)); err != nil || len(b) != 0 {
		t.Fatalf("downloaded body=%q, err=%v", b, err)
	}
}

func TestDownloadRejectsShortBody(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := downloadRecord("four", 5)
	assertDownloadFailedCleanly(t, n, r, bodyFetch("four"))
}

func TestDownloadRejectsSizePlusOne(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := downloadRecord("four", 3)
	assertDownloadFailedCleanly(t, n, r, func(_ context.Context, _ FileInfo, w io.Writer) error {
		_, _ = io.WriteString(w, "four") // Ignore the counting writer's overflow error.
		return nil
	})
}

type planRecorder struct {
	files int
	bytes int64
}

func (p *planRecorder) Plan(files int, bytes int64) { p.files, p.bytes = files, bytes }
func (*planRecorder) Done(FileInfo, Event, int64)   {}

func TestDownloadMaxInt64DoesNotOverflow(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := downloadRecord("", int64(^uint64(0)>>1))
	p := &planRecorder{}
	assertDownloadFailedCleanly(t, n, r, bodyFetch(""), p)
	// A declared maximum size is representable in planning and fails as a short
	// body without an overflowing Size+1 calculation.
	if p.files != 1 || p.bytes != r.Size {
		t.Fatalf("plan = (%d files, %d bytes), want (1, %d)", p.files, p.bytes, r.Size)
	}
}

func TestDownloadSyncFailureCleansTemp(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := downloadRecord("body", 4)
	n.f.SetPersistenceProbe(PersistenceProbe{Fault: func(_, phase string, _, _ uint64) error {
		if phase == "G1" {
			return errors.New("sync failed")
		}
		return nil
	}})
	assertDownloadFailedCleanly(t, n, r, bodyFetch("body"))
}

func TestDownloadRejectsHashMismatch(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := downloadRecord("else", 4)
	assertDownloadFailedCleanly(t, n, r, bodyFetch("body"))
}

func TestDownloadFetchFailureCleansTemp(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := downloadRecord("body", 4)
	assertDownloadFailedCleanly(t, n, r, func(context.Context, FileInfo, io.Writer) error {
		return errors.New("fetch failed")
	})
}

func TestDownloadCloseFailureCleansTemp(t *testing.T) {
	n := applySeed(t)
	defer n.f.Close()
	r := downloadRecord("body", 4)
	oldClose := closeDownloadedFile
	closeDownloadedFile = func(file *os.File) error {
		_ = file.Close()
		return errors.New("close failed")
	}
	t.Cleanup(func() { closeDownloadedFile = oldClose })
	assertDownloadFailedCleanly(t, n, r, bodyFetch("body"))
}
