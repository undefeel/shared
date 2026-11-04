package folder

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type applyEvents struct{ events []Event }

func (*applyEvents) Plan(int, int64)                     {}
func (p *applyEvents) Done(_ FileInfo, e Event, _ int64) { p.events = append(p.events, e) }
func incoming(body string, v Vector, mt int64) FileInfo {
	return FileInfo{Name: "x", Hash: fmt.Sprintf("%x", sha256.Sum256([]byte(body))), Size: int64(len(body)), ModTime: mt, Version: v}
}
func bodyFetch(body string) Fetcher {
	return func(_ context.Context, _ FileInfo, w io.Writer) error { _, e := io.WriteString(w, body); return e }
}
func applySeed(t *testing.T) *node {
	t.Helper()
	n := newNode(t, "A")
	t.Cleanup(func() { n.f.Close() })
	n.write(t, "x", "old", time.Unix(100, 0))
	if e := n.f.Scan(); e != nil {
		t.Fatal(e)
	}
	return n
}
func assertBody(t *testing.T, n *node, want string) {
	t.Helper()
	got, ok := n.read("x")
	if !ok || got != want {
		t.Fatalf("body=%q want=%q", got, want)
	}
}

func TestApplyTwoPeersSameStatDifferentHash(t *testing.T) {
	n := applySeed(t)
	started, release := make(chan struct{}), make(chan struct{})
	old := incoming("one", Vector{"B": 1}, 200e9)
	newer := incoming("two", Vector{"A": 1, "B": 1, "C": 1}, 200e9)
	ev := &applyEvents{}
	done := make(chan [2]int, 1)
	go func() {
		a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{old}, func(c context.Context, f FileInfo, w io.Writer) error {
			close(started)
			<-release
			return bodyFetch("one")(c, f, w)
		}, ev)
		done <- [2]int{a, b}
	}()
	<-started
	if _, b := n.f.ApplyRemote(context.Background(), "C", []FileInfo{newer}, bodyFetch("two"), nil); b != 0 {
		t.Fatal(b)
	}
	close(release)
	result := <-done
	if result[1] != 0 {
		t.Fatal(result)
	}
	assertBody(t, n, "two")
	if len(ev.events) != 0 {
		t.Fatal("stale forecast emitted event", ev.events)
	}
}

func TestApplyEditAgainstTombstone(t *testing.T) {
	n := applySeed(t)
	before, _ := n.f.Snapshot()
	r := FileInfo{Name: "x", Deleted: true, Version: Vector{"B": 1}}
	ev := &applyEvents{}
	if _, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, nil, ev); b != 0 {
		t.Fatal(b)
	}
	assertBody(t, n, "old")
	after, _ := n.f.Snapshot()
	if after[0].Version.Compare(before[0].Version) != Greater || after[0].Version["B"] != 1 {
		t.Fatal(after)
	}
	if len(ev.events) != 0 {
		t.Fatal(ev.events)
	}
}

func TestApplyLocalWriteBeforeCapture(t *testing.T) {
	n := applySeed(t)
	r := incoming("new", Vector{"A": 1, "B": 1}, 200e9)
	ev := &applyEvents{}
	fetch := func(c context.Context, f FileInfo, w io.Writer) error {
		n.write(t, "x", "edit", time.Unix(300, 0))
		return bodyFetch("new")(c, f, w)
	}
	if _, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, fetch, ev); b != 0 {
		t.Fatal(b)
	}
	assertBody(t, n, "edit")
	if len(ev.events) != 0 {
		t.Fatal("event for rejected incoming", ev.events)
	}
}

func TestApplyLocalWriteAfterCapture(t *testing.T) {
	n := applySeed(t)
	r := incoming("new", Vector{"A": 1, "B": 1}, 200e9)
	edited := false
	n.f.persistHooks.syncDir = func(d *os.File) error {
		captures, e := filepath.Glob(filepath.Join(n.f.Root, tmpPrefix+"capture-*"))
		if e != nil {
			return e
		}
		if !edited && len(captures) > 0 {
			edited = true
			if e = os.WriteFile(captures[0], []byte("late"), 0600); e != nil {
				return e
			}
		}
		return syncDir(d)
	}
	ev := &applyEvents{}
	_, failed := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, bodyFetch("new"), ev)
	if !edited || failed != 0 {
		t.Fatalf("edited=%v failed=%d", edited, failed)
	}
	assertBody(t, n, "late")
	if len(ev.events) != 0 {
		t.Fatal(ev.events)
	}
}

func TestApplyCancelAfterDownload(t *testing.T) {
	n := applySeed(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := incoming("new", Vector{"A": 1, "B": 1}, 200e9)
	ev := &applyEvents{}
	fetch := func(c context.Context, f FileInfo, w io.Writer) error {
		e := bodyFetch("new")(c, f, w)
		cancel()
		return e
	}
	if a, b := n.f.ApplyRemote(ctx, "B", []FileInfo{r}, fetch, ev); a != 0 || b == 0 {
		t.Fatalf("%d %d", a, b)
	}
	assertBody(t, n, "old")
	if len(ev.events) != 0 {
		t.Fatal(ev.events)
	}
}

func TestApplyMergeOnlyAgainstSave(t *testing.T) {
	n := applySeed(t)
	r := incoming("old", Vector{"B": 1}, 100e9)
	before, _ := n.f.Snapshot()
	ev := &applyEvents{}
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if e := n.f.save(); e != nil {
				errs <- e
				return
			}
		}
	}()
	if a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, nil, ev); a != 1 || b != 0 {
		t.Fatalf("%d %d", a, b)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	after, _ := n.f.Snapshot()
	if after[0].Version["B"] != 1 || before[0].Version["B"] != 0 {
		t.Fatal(after, before)
	}
	if len(ev.events) != 0 {
		t.Fatal(ev.events)
	}
	reopened, e := Open(n.f.Root, n.f.statePath, "A", n.f.log)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	disk, _ := reopened.Snapshot()
	if disk[0].Version["B"] != 1 {
		t.Fatal(disk)
	}
}
