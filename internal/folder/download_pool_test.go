package folder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadPoolSharedAcrossApplyRemoteCalls(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	n.f.SetDownloadConcurrency(2)
	body := []byte("shared-body")
	hash := sha256.Sum256(body)
	makeRecords := func(peer string) []FileInfo {
		out := make([]FileInfo, 12)
		for i := range out {
			out[i] = FileInfo{Name: fmt.Sprintf("%s-%02d", peer, i), Size: int64(len(body)), Hash: hex.EncodeToString(hash[:]), Version: Vector{peer: 1}}
		}
		return out
	}
	entered := make(chan struct{}, 24)
	gate := make(chan struct{})
	var active, maximum atomic.Int32
	fetch := func(ctx context.Context, _ FileInfo, w io.Writer) error {
		n := active.Add(1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		entered <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
			active.Add(-1)
			return ctx.Err()
		}
		active.Add(-1)
		_, err := w.Write(body)
		return err
	}
	var wg sync.WaitGroup
	for _, peer := range []string{"B", "C"} {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			_, failed := n.f.ApplyRemote(context.Background(), peer, makeRecords(peer), fetch, nil)
			if failed != 0 {
				t.Errorf("%s failed %d records", peer, failed)
			}
		}(peer)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("pool did not admit two fetches")
		}
	}
	select {
	case <-entered:
		t.Fatal("pool admitted more than configured concurrency")
	case <-time.After(50 * time.Millisecond):
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum active fetches = %d, want exactly 2", got)
	}
	close(gate)
	wg.Wait()
}

func TestDownloadPoolStagingReservationAndOversizedExclusive(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	var releases []func()
	for i := 0; i < maxStagedOperations; i++ {
		release, err := n.f.reserveStaging(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	ctxCount, cancelCount := context.WithTimeout(context.Background(), 30*time.Millisecond)
	if _, err := n.f.reserveStaging(ctxCount, 0); err == nil {
		cancelCount()
		t.Fatal("admitted more than the operation limit")
	}
	cancelCount()
	for _, release := range releases {
		release()
	}
	release, err := n.f.reserveStaging(context.Background(), maxStagedBytes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := n.f.reserveStaging(ctx, 1); err == nil {
		t.Fatal("admitted staging beyond byte budget")
	}
	release()
	large, err := n.f.reserveStaging(context.Background(), maxStagedBytes+1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := n.f.reserveStaging(ctx, 1); err == nil {
		t.Fatal("admitted ordinary operation beside oversized reservation")
	}
	large()
	drain, err := n.f.reserveStaging(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	drain()
}

func TestDownloadCancellationReleasesPoolAndTemporaryFile(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	started, closed := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, failed := n.f.ApplyRemote(ctx, "B", []FileInfo{incoming("partial", Vector{"B": 1}, 200e9)}, func(ctx context.Context, _ FileInfo, w io.Writer) error {
			close(started)
			_, _ = io.WriteString(w, "partial")
			<-ctx.Done()
			close(closed)
			return ctx.Err()
		}, nil)
		if failed != 1 {
			t.Errorf("failed=%d, want 1", failed)
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("active fetch did not observe cancellation")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not drain after cancellation")
	}
	p := n.f.pool()
	p.mu.Lock()
	streams, operations, bytes := p.streams, p.operations, p.bytes
	p.mu.Unlock()
	if streams != 0 || operations != 0 || bytes != 0 {
		t.Fatalf("pool leaked permits: stream=%d operations=%d bytes=%d", streams, operations, bytes)
	}
	entries, err := os.ReadDir(n.f.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary file leaked: %v", entries)
	}
}
