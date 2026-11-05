package app

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sharedir/internal/config"
	"sharedir/internal/folder"
	"sharedir/internal/transport"
)

type perfResult struct {
	elapsed               time.Duration
	index, intent, groups uint64
	flush                 folder.PersistenceCounters
}

func perfDaemon(t testing.TB, base, tag string, measure *folder.PersistenceMeasurement) *Daemon {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	home := filepath.Join(base, tag)
	root := filepath.Join(home, "root")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Name: tag, Folder: root, DownloadConcurrency: 6}
	if err := cfg.Save(home); err != nil {
		t.Fatal(err)
	}
	d, err := newWithFolderFactory(home, logger, func(root, state, device string, l *log.Logger) (*folder.Folder, error) {
		f, e := folder.Open(root, state, device, l)
		if e == nil && measure != nil {
			f.SetPersistenceProbe(measure.Probe())
		}
		return f, e
	})
	if err != nil {
		t.Fatal(err)
	}
	d.folder, err = d.openFolder(root, filepath.Join(home, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.folder.SetDownloadConcurrency(6)
	t.Cleanup(func() {
		if e := d.folder.Close(); e != nil {
			t.Error(e)
		}
	})
	return d
}

func perfServer(t testing.TB, owner, peer *Daemon, observe func(*http.Request)) *httptest.Server {
	t.Helper()
	allowed := func(id string) bool { return id == peer.ID.ID }
	handler := (&transport.Server{Folder: owner.folder, State: folderPeerState{owner.folder}, Allowed: allowed}).Handler()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if observe != nil {
			observe(r)
		}
		handler.ServeHTTP(w, r)
	}))
	srv.EnableHTTP2 = true
	srv.TLS = transport.ServerTLS(owner.ID, allowed, nil)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func perfClient(t testing.TB, local, remote *Daemon) *transport.Client {
	t.Helper()
	c := transport.NewClient(local.ID, remote.ID.ID, 0, nil)
	c.LocalHello = func() transport.Hello { return folderPeerState{local.folder}.Hello() }
	t.Cleanup(c.Close)
	return c
}

func perfTransfer(t testing.TB, ctx context.Context, dst, remote *Daemon, client *transport.Client, addr string, baseline bool) (*peerQueue, *transport.IndexResponse) {
	t.Helper()
	idx, err := client.IndexSince(ctx, addr, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	q := newPeerQueue()
	if !q.accept(remote.ID.ID, idx.StoreEpoch, idx.Cursor, false, idx.Files) {
		if baseline {
			t.Fatal("queue rejected baseline")
		}
		t.Fatal("queue rejected index")
	}
	for {
		due := q.due(remote.ID.ID, time.Now(), 64)
		if len(due) == 0 {
			break
		}
		_, failed := dst.applyPeerDue(ctx, client, addr, remote.ID.ID, remote.ID.Short(), idx.StoreEpoch, idx.Cursor, due, q, nil)
		if failed != 0 {
			if baseline {
				t.Fatalf("apply failures=%d", failed)
			}
			t.Fatalf("transfer failures: %d", failed)
		}
	}
	return q, idx
}

func perfVerify(t testing.TB, dest *Daemon, expected []folder.FileInfo, byName map[string]folder.FileInfo) {
	t.Helper()
	for _, want := range expected {
		fi := byName[want.Name]
		if fi.Hash != want.Hash || fi.Size != want.Size || fi.Exec != want.Exec || !reflect.DeepEqual(fi.Version, want.Version) {
			t.Fatalf("metadata mismatch %s", want.Name)
		}
		body, e := os.ReadFile(filepath.Join(dest.folder.Root, fi.Name))
		if e != nil {
			t.Fatal(e)
		}
		hash := sha256.Sum256(body)
		if hex.EncodeToString(hash[:]) != want.Hash {
			t.Fatalf("disk hash mismatch %s", want.Name)
		}
		st, e := os.Stat(filepath.Join(dest.folder.Root, fi.Name))
		if e != nil {
			t.Fatal(e)
		}
		if (st.Mode()&0111 != 0) != want.Exec {
			t.Fatalf("disk Exec mismatch %s", want.Name)
		}
	}
}

func perfPopulate(t testing.TB, source *Daemon, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		// 4*4*4 leaf directories, depth four including filename.
		dir := filepath.Join(source.folder.Root, fmt.Sprint(i%4), fmt.Sprint((i/4)%4), fmt.Sprint((i/16)%4))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 11<<10)
		for off := 0; off < len(body); off += 32 {
			var seed [16]byte
			binary.LittleEndian.PutUint64(seed[:8], uint64(i))
			binary.LittleEndian.PutUint64(seed[8:], uint64(off))
			hash := sha256.Sum256(seed[:])
			copy(body[off:], hash[:])
		}
		mode := os.FileMode(0644)
		if i%3 == 0 {
			mode = 0755
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), body, mode); err != nil {
			t.Fatal(err)
		}
	}
}

func perfTrial(t testing.TB, n int) perfResult {
	t.Helper()
	base := t.TempDir()
	var measure folder.PersistenceMeasurement
	source, dest := perfDaemon(t, base, "source", nil), perfDaemon(t, base, "receiver", &measure)
	perfPopulate(t, source, n)
	if err := source.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	expected, _, _ := source.folder.DurableState()
	var h2, authenticated atomic.Bool
	srv := perfServer(t, source, dest, func(r *http.Request) {
		if r.ProtoMajor == 2 {
			h2.Store(true)
		}
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			authenticated.Store(true)
		}
	})
	client := perfClient(t, dest, source)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	before := dest.folder.PersistenceStats()
	start := time.Now()
	addr := strings.TrimPrefix(srv.URL, "https://")
	queue, idx := perfTransfer(t, ctx, dest, source, client, addr, true)
	got, _, _ := dest.folder.DurableState()
	byName := make(map[string]folder.FileInfo, len(got))
	for _, fi := range got {
		byName[fi.Name] = fi
	}
	if len(got) != n {
		t.Fatalf("files=%d want=%d", len(got), n)
	}
	perfVerify(t, dest, expected, byName)
	elapsed := time.Since(start)
	if !h2.Load() || !authenticated.Load() {
		t.Fatal("authenticated HTTP/2 not observed")
	}
	if _, cursor, ok := queue.cursor(source.ID.ID); !ok || cursor != idx.Cursor {
		t.Fatal("durable cursor unsettled")
	}
	bytes, groups := measure.Snapshot()
	after := dest.folder.PersistenceStats()
	result := perfResult{elapsed: elapsed, groups: groups, flush: folder.PersistenceCounters{Bytes: after.Bytes - before.Bytes, Fsync: after.Fsync - before.Fsync, Fullfsync: after.Fullfsync - before.Fullfsync, Barrierfsync: after.Barrierfsync - before.Barrierfsync}}
	for kind, b := range bytes {
		if kind == "intent" {
			result.intent += b
		} else {
			result.index += b
		}
	}
	fmt.Printf("Perf n=%d time=%s files/s=%.1f index=%d intent=%d fsync=%d FULLFSYNC=%d BARRIERFSYNC=%d G=%d mean=%.2f\n", n, elapsed, float64(n)/elapsed.Seconds(), result.index, result.intent, result.flush.Fsync, result.flush.Fullfsync, result.flush.Barrierfsync, groups, float64(n)/float64(groups))
	if groups == 0 || float64(n)/float64(groups) < 32 {
		t.Errorf("pipeline mean group size below 32: files=%d groups=%d", n, groups)
	}
	if result.index+result.intent != result.flush.Bytes {
		t.Errorf("probe byte accounting mismatch")
	}
	if result.intent > 4<<20 {
		t.Errorf("intent bytes %d >4MiB", result.intent)
	}
	if result.index > 4<<20 {
		t.Errorf("index bytes %d > 4 MiB", result.index)
	}
	if result.flush.Fullfsync > 3*groups+12 {
		t.Errorf("FULLFSYNC=%d >3G+12 G=%d", result.flush.Fullfsync, groups)
	}
	a := dest.folder.PersistenceStats()
	if err := dest.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	b := dest.folder.PersistenceStats()
	if a != b {
		t.Errorf("no-op Scan wrote/flushed: before=%+v after=%+v", a, b)
	}
	return result
}

func TestPerf(t *testing.T) {
	if os.Getenv("SHAREDIR_PERF") != "1" {
		t.Skip("set SHAREDIR_PERF=1")
	}
	runs := 1
	if os.Getenv("SHAREDIR_PERF") == "1" {
		runs = 5
	}
	times := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		r := perfTrial(t, 2092)
		times = append(times, r.elapsed)
	}
	if runs == 5 {
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		fmt.Printf("Perf median=%s\n", times[2])
		if times[2] > 30*time.Second || times[4] > 45*time.Second {
			t.Errorf("timing threshold: %v", times)
		}
	}
	_ = perfTrial(t, 1000)
	two := perfTrial(t, 2000)
	four := perfTrial(t, 4000)
	if float64(four.index) > 2.3*float64(two.index)+256*1024 {
		t.Errorf("nonlinear writes: 2000=%d 4000=%d", two.index, four.index)
	}
}

// BenchmarkPerf runs real flushes; use -benchtime=1x for one complete fixture.
func BenchmarkPerf(b *testing.B) {
	if os.Getenv("SHAREDIR_PERF") != "1" {
		b.Skip("set SHAREDIR_PERF=1")
	}
	for i := 0; i < b.N; i++ {
		r := perfTrial(b, 2092)
		b.ReportMetric(2092/r.elapsed.Seconds(), "files/s")
		b.ReportMetric(float64(r.index), "index-B")
		b.ReportMetric(float64(r.flush.Fsync), "fsync")
		b.ReportMetric(float64(r.flush.Fullfsync), "FULLFSYNC")
		b.ReportMetric(float64(r.groups), "groups")
	}
}

func perfReuseTrial(t testing.TB, n int) perfResult {
	t.Helper()
	base := t.TempDir()
	var measure folder.PersistenceMeasurement
	source, dest := perfDaemon(t, base, "source", nil), perfDaemon(t, base, "receiver", &measure)
	perfPopulate(t, source, n)
	for i := 0; i < n; i++ {
		dir := filepath.Join(source.folder.Root, fmt.Sprint(i%4), fmt.Sprint((i/4)%4), fmt.Sprint((i/16)%4))
		body, e := os.ReadFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dest.folder.Root, fmt.Sprintf("donor%05d", i)), body, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := dest.folder.Scan(); e != nil {
		t.Fatal(e)
	}
	if err := source.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	expected, _, _ := source.folder.DurableState()
	var h2, authenticated atomic.Bool
	var bodies atomic.Int64
	srv := perfServer(t, source, dest, func(r *http.Request) {
		if r.URL.Path == "/file" || r.URL.Path == "/files" {
			bodies.Add(1)
		}
		if r.ProtoMajor == 2 {
			h2.Store(true)
		}
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			authenticated.Store(true)
		}
	})
	client := perfClient(t, dest, source)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	setupBytes, setupGroups := measure.Snapshot()
	before := dest.folder.PersistenceStats()
	start := time.Now()
	addr := strings.TrimPrefix(srv.URL, "https://")
	queue, idx := perfTransfer(t, ctx, dest, source, client, addr, true)
	got, _, _ := dest.folder.DurableState()
	byName := make(map[string]folder.FileInfo, len(got))
	for _, fi := range got {
		byName[fi.Name] = fi
	}
	if bodies.Load() != 0 {
		t.Fatalf("body requests=%d", bodies.Load())
	}
	if len(got) != 2*n {
		t.Fatalf("files=%d want=%d", len(got), n)
	}
	perfVerify(t, dest, expected, byName)
	elapsed := time.Since(start)
	if !h2.Load() || !authenticated.Load() {
		t.Fatal("authenticated HTTP/2 not observed")
	}
	if _, cursor, ok := queue.cursor(source.ID.ID); !ok || cursor != idx.Cursor {
		t.Fatal("durable cursor unsettled")
	}
	bytes, groups := measure.Snapshot()
	groups -= setupGroups
	for kind, n := range setupBytes {
		bytes[kind] -= n
	}
	after := dest.folder.PersistenceStats()
	result := perfResult{elapsed: elapsed, groups: groups, flush: folder.PersistenceCounters{Bytes: after.Bytes - before.Bytes, Fsync: after.Fsync - before.Fsync, Fullfsync: after.Fullfsync - before.Fullfsync, Barrierfsync: after.Barrierfsync - before.Barrierfsync}}
	for kind, b := range bytes {
		if kind == "intent" {
			result.intent += b
		} else {
			result.index += b
		}
	}
	fmt.Printf("Perf n=%d time=%s files/s=%.1f index=%d intent=%d fsync=%d FULLFSYNC=%d BARRIERFSYNC=%d G=%d mean=%.2f\n", n, elapsed, float64(n)/elapsed.Seconds(), result.index, result.intent, result.flush.Fsync, result.flush.Fullfsync, result.flush.Barrierfsync, groups, float64(n)/float64(groups))
	if result.index+result.intent != result.flush.Bytes {
		t.Fatal("probe byte accounting mismatch")
	}
	if groups == 0 || float64(n)/float64(groups) < 32 {
		t.Errorf("pipeline mean group size below 32: files=%d groups=%d", n, groups)
	}
	if result.intent > 4<<20 {
		t.Errorf("intent bytes %d >4MiB", result.intent)
	}
	if result.index > 4<<20 {
		t.Errorf("index bytes %d > 4 MiB", result.index)
	}
	if result.flush.Fullfsync > 3*groups+12 {
		t.Errorf("FULLFSYNC=%d >3G+12 G=%d", result.flush.Fullfsync, groups)
	}
	return result
}

func TestPerfContentReuse(t *testing.T) {
	if os.Getenv("SHAREDIR_PERF") != "1" {
		t.Skip("set SHAREDIR_PERF=1")
	}
	_ = perfReuseTrial(t, 200)
}

func TestNoOpScanWrites(t *testing.T) {
	d := perfDaemon(t, t.TempDir(), "source", nil)
	if err := os.WriteFile(filepath.Join(d.folder.Root, "file"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	before := d.folder.PersistenceStats()
	if err := d.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	if after := d.folder.PersistenceStats(); before != after {
		t.Errorf("no-op Scan wrote/flushed: before=%+v after=%+v", before, after)
	}
}

// TestWatchScheduleHarness verifies a real incoming group burst is suppressed by
// the receiver scheduler, then checks that a receiver-side edit is scanned
// incrementally and propagated back to the original sender.
func TestWatchScheduleHarness(t *testing.T) {
	base := t.TempDir()
	source, dest := perfDaemon(t, base, "source", nil), perfDaemon(t, base, "receiver", nil)
	for _, d := range []*Daemon{source, dest} {
		d.act = &activity{}
		d.peers = map[string]*peerRunner{}
	}
	for i := 0; i < 2092; i++ {
		name := filepath.Join(source.folder.Root, fmt.Sprintf("f%05d", i))
		if err := os.WriteFile(name, []byte(fmt.Sprintf("baseline-%d", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	sourceServer := perfServer(t, source, dest, nil)
	destServer := perfServer(t, dest, source, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sourceClient := perfClient(t, dest, source)
	perfTransfer(t, ctx, dest, source, sourceClient, strings.TrimPrefix(sourceServer.URL, "https://"), false)
	batches := make(chan folder.WatchBatch, 32)
	dest.watchScanObserver = func(b folder.WatchBatch) {
		select {
		case batches <- b:
		default:
		}
	}
	watchCtx, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() { defer close(watchDone); dest.watchLoop(watchCtx) }()
	defer func() { stopWatch(); <-watchDone }()
	// Drain delayed watcher baseline batches before measuring the incoming burst.
	quiet := time.NewTimer(time.Second)
	defer quiet.Stop()
	for {
		select {
		case <-batches:
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(time.Second)
		case <-quiet.C:
			goto baselineReady
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
baselineReady:
	for i := 0; i < 32; i++ {
		if err := os.WriteFile(filepath.Join(source.folder.Root, fmt.Sprintf("f%05d", i)), []byte(fmt.Sprintf("incoming burst %d", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	perfTransfer(t, ctx, dest, source, sourceClient, strings.TrimPrefix(sourceServer.URL, "https://"), false)
	ownDeadline := time.NewTimer(2 * time.Second)
	defer ownDeadline.Stop()
	select {
	case b := <-batches:
		t.Fatalf("incoming own-write burst scheduled a scan: %+v", b)
	case <-ownDeadline.C:
	}
	// A local receiver edit must produce an incremental scope.
	if err := os.WriteFile(filepath.Join(dest.folder.Root, "f00042"), []byte("receiver edit"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-batches:
		if b.FullRequired || b.RootChanged || len(b.Paths) == 0 {
			t.Fatalf("receiver edit was not incremental: %+v", b)
		}
	case <-ctx.Done():
		t.Fatal("receiver edit was not scanned")
	}
	// Pull the receiver’s durable change back to the source and verify bytes.
	perfTransfer(t, ctx, source, dest, perfClient(t, source, dest), strings.TrimPrefix(destServer.URL, "https://"), false)
	got, err := os.ReadFile(filepath.Join(source.folder.Root, "f00042"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "receiver edit" {
		t.Fatalf("reverse propagation = %q", got)
	}
}
