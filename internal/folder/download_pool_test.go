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

func TestDownloadPoolIndependentFIFOAndOversizedStream(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	stage, err := n.f.reserveStaging(context.Background(), maxStagedBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer stage()
	blocked := make(chan error, 1)
	go func() {
		release, err := n.f.reserveStaging(context.Background(), 1)
		if release != nil {
			release()
		}
		blocked <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		p := n.f.pool()
		p.mu.Lock()
		queued := len(p.stageWaiters)
		p.mu.Unlock()
		if queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("staging waiter did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := n.f.acquireDownload(ctx)
	if err != nil {
		t.Fatalf("stream blocked behind staging reservation: %v", err)
	}
	stream()
	stage()
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("staging waiter did not resume")
	}
	// Oversized work is exclusive among staging reservations while streams stay
	// independently available.
	large, err := n.f.reserveStaging(context.Background(), maxStagedBytes+1)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	s, err := n.f.acquireDownload(ctx2)
	if err != nil {
		t.Fatalf("oversized staging blocked stream: %v", err)
	}
	s()
	large()
}

func TestDownloadPoolBoundsProducersWhileCommitBlocked(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	n.f.scanMu.Lock()
	records := func(peer string, start, count int) []FileInfo {
		out := make([]FileInfo, count)
		hash := sha256.Sum256([]byte("body"))
		for i := range out {
			out[i] = FileInfo{Name: fmt.Sprintf("file-%d", start+i), Hash: hex.EncodeToString(hash[:]), Size: 4, Version: Vector{peer: 1}}
		}
		return out
	}
	var fetched atomic.Int32
	fetch := func(_ context.Context, fi FileInfo, w io.Writer) error {
		fetched.Add(1)
		_, err := io.WriteString(w, "body")
		return err
	}
	results := make(chan [2]int, 3)
	for _, batch := range []struct {
		peer     string
		start, n int
	}{{"B", 0, 64}, {"C", 64, 64}, {"D", 128, 1}} {
		b := batch
		go func() {
			a, f := n.f.ApplyRemote(context.Background(), b.peer, records(b.peer, b.start, b.n), fetch, nil)
			results <- [2]int{a, f}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p := n.f.pool()
		p.mu.Lock()
		ops := p.operations
		p.mu.Unlock()
		if ops == maxStagedOperations {
			break
		}
		if time.Now().After(deadline) {
			n.f.scanMu.Unlock()
			t.Fatalf("staged operations=%d, want %d", ops, maxStagedOperations)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(25 * time.Millisecond)
	if got := fetched.Load(); got != maxStagedOperations {
		n.f.scanMu.Unlock()
		t.Fatalf("fetches past blocked commit=%d, want %d", got, maxStagedOperations)
	}
	n.f.scanMu.Unlock()
	for range 3 {
		select {
		case r := <-results:
			if r[1] != 0 {
				t.Fatalf("apply failures=%d", r[1])
			}
		case <-time.After(10 * time.Second):
			t.Fatal("producer did not drain")
		}
	}
	if got := fetched.Load(); got != maxStagedOperations+1 {
		t.Fatalf("total fetches=%d", got)
	}
}

func TestDownloadPoolBlocksByteBudgetAndOversizedProducers(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	n.f.SetDownloadConcurrency(8)
	gate := make(chan struct{})
	entered := make(chan string, 4)
	fetch := func(_ context.Context, fi FileInfo, _ io.Writer) error {
		entered <- fi.Name
		<-gate
		return fmt.Errorf("release test fetch")
	}
	start := func(peer, name string, size int64) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = n.f.ApplyRemote(context.Background(), peer, []FileInfo{{Name: name, Size: size, Hash: hex.EncodeToString(make([]byte, 32)), Version: Vector{peer: 1}}}, fetch, nil)
		}()
		return done
	}
	// Two live fetches consume the full byte budget; a third producer waits
	// before invoking its fetcher even though operation slots remain.
	n.f.scanMu.Lock()
	doneA := start("B", "half-a", maxStagedBytes/2)
	doneB := start("C", "half-b", maxStagedBytes/2)
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			n.f.scanMu.Unlock()
			t.Fatal("byte-budget producers did not start")
		}
	}
	doneC := start("D", "over-byte", 1)
	select {
	case name := <-entered:
		n.f.scanMu.Unlock()
		t.Fatalf("byte-budget producer %q fetched early", name)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	n.f.scanMu.Unlock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("third producer did not enter after byte budget drained")
	}
	for _, done := range []<-chan struct{}{doneA, doneB, doneC} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("byte-budget producer did not drain")
		}
	}

	// An oversized request waits for all ordinary staging to drain, rather
	// than being admitted alongside a live ordinary producer.
	gate2 := make(chan struct{})
	startedOrdinary := make(chan struct{})
	ordinaryDone := make(chan struct{})
	go func() {
		defer close(ordinaryDone)
		_, _ = n.f.ApplyRemote(context.Background(), "E", []FileInfo{{Name: "ordinary", Size: 1, Hash: hex.EncodeToString(make([]byte, 32)), Version: Vector{"E": 1}}}, func(context.Context, FileInfo, io.Writer) error {
			close(startedOrdinary)
			<-gate2
			return fmt.Errorf("release ordinary")
		}, nil)
	}()
	select {
	case <-startedOrdinary:
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary producer did not start")
	}
	oversizedEntered := make(chan struct{})
	oversizedGate := make(chan struct{})
	oversizedDone := make(chan struct{})
	go func() {
		defer close(oversizedDone)
		_, _ = n.f.ApplyRemote(context.Background(), "F", []FileInfo{{Name: "oversized", Size: maxStagedBytes + 1, Hash: hex.EncodeToString(make([]byte, 32)), Version: Vector{"F": 1}}}, func(context.Context, FileInfo, io.Writer) error {
			close(oversizedEntered)
			<-oversizedGate
			return fmt.Errorf("release oversized")
		}, nil)
	}()
	select {
	case name := <-entered:
		t.Fatalf("oversized producer %q entered beside ordinary staging", name)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate2)
	select {
	case <-oversizedEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("oversized producer did not enter after ordinary drain")
	}
	select {
	case <-ordinaryDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary producer did not drain")
	}
	close(oversizedGate)
	select {
	case <-oversizedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("oversized producer did not drain")
	}
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
