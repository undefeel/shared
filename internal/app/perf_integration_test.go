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

func perfTrial(t testing.TB, n int) perfResult {
	t.Helper()
	base := t.TempDir()
	logger := log.New(io.Discard, "", 0)
	var measure folder.PersistenceMeasurement
	open := func(tag string, receiver bool) *Daemon {
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
			if e == nil && receiver {
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
	source, dest := open("source", false), open("receiver", true)
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
	if err := source.folder.Scan(); err != nil {
		t.Fatal(err)
	}
	expected, _, _ := source.folder.DurableState()
	var h2, authenticated atomic.Bool
	handler := (&transport.Server{Folder: source.folder, State: folderPeerState{source.folder}, Allowed: func(id string) bool { return id == dest.ID.ID }}).Handler()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 {
			h2.Store(true)
		}
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			authenticated.Store(true)
		}
		handler.ServeHTTP(w, r)
	}))
	srv.EnableHTTP2 = true
	srv.TLS = transport.ServerTLS(source.ID, func(id string) bool { return id == dest.ID.ID }, nil)
	srv.StartTLS()
	defer srv.Close()
	client := transport.NewClient(dest.ID, source.ID.ID, 0, nil)
	defer client.Close()
	client.LocalHello = func() transport.Hello { return folderPeerState{dest.folder}.Hello() }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	before := dest.folder.PersistenceStats()
	start := time.Now()
	addr := strings.TrimPrefix(srv.URL, "https://")
	idx, err := client.IndexSince(ctx, addr, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	queue := newPeerQueue()
	if !queue.accept(source.ID.ID, idx.StoreEpoch, idx.Cursor, false, idx.Files) {
		t.Fatal("queue rejected baseline")
	}
	for {
		due := queue.due(source.ID.ID, time.Now(), 64)
		if len(due) == 0 {
			break
		}
		_, failed := dest.applyPeerDue(ctx, client, addr, source.ID.ID, source.ID.Short(), idx.StoreEpoch, idx.Cursor, due, queue, nil)
		if failed != 0 {
			t.Fatalf("apply failures=%d", failed)
		}
	}
	got, _, _ := dest.folder.DurableState()
	byName := make(map[string]folder.FileInfo, len(got))
	for _, fi := range got {
		byName[fi.Name] = fi
	}
	if len(got) != n {
		t.Fatalf("files=%d want=%d", len(got), n)
	}
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
	for i := 0; i < b.N; i++ {
		r := perfTrial(b, 2092)
		b.ReportMetric(2092/r.elapsed.Seconds(), "files/s")
		b.ReportMetric(float64(r.index), "index-B")
		b.ReportMetric(float64(r.flush.Fsync), "fsync")
		b.ReportMetric(float64(r.flush.Fullfsync), "FULLFSYNC")
		b.ReportMetric(float64(r.groups), "groups")
	}
}
