package folder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type groupProgress struct{ done func() }

func (*groupProgress) Plan(int, int64)               {}
func (p *groupProgress) Done(FileInfo, Event, int64) { p.done() }

func TestApplyGroup64IncomingOneFrame(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	var remote []FileInfo
	for i := 0; i < 64; i++ {
		r := incoming("body", Vector{"B": 1}, 200e9)
		r.Name = fmt.Sprintf("file-%02d", i)
		remote = append(remote, r)
	}
	var barrier sync.WaitGroup
	// The pool bounds network concurrency; all 64 may still enter one durable
	// coordinator group as completed downloads make room for later workers.
	barrier.Add(6)
	var fetchMu sync.Mutex
	fetches := 0
	frames, logWrites := 0, 0
	n.f.SetPersistenceProbe(PersistenceProbe{Write: func(kind, _ string, _ int, _ error) {
		if kind == "log" {
			logWrites++
		}
	}, Phase: func(_, phase string) {
		if phase == "G4" {
			frames++
		}
	}})
	done := 0
	progress := &groupProgress{done: func() {
		files, _ := n.f.Snapshot()
		if len(files) != 64 || (frames != 1 || logWrites != 1) {
			t.Errorf("early/partial Done: %d files, %d phases, %d log writes", len(files), frames, logWrites)
		}
		done++
	}}
	a, b := n.f.ApplyRemote(context.Background(), "B", remote, func(_ context.Context, _ FileInfo, w io.Writer) error {
		fetchMu.Lock()
		fetches++
		firstSix := fetches <= 6
		fetchMu.Unlock()
		if firstSix {
			barrier.Done()
			barrier.Wait()
		}
		_, e := io.WriteString(w, "body")
		return e
	}, progress)
	if a != 64 || b != 0 || frames != 1 || logWrites != 1 || done != 64 {
		t.Fatalf("applied=%d failed=%d frames=%d writes=%d Done=%d", a, b, frames, logWrites, done)
	}
	reopened, err := Open(n.f.Root, n.f.statePath, "A", n.f.log)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	files, _ := reopened.Snapshot()
	if len(files) != 64 {
		t.Fatal(len(files))
	}
	for _, fi := range files {
		body, err := os.ReadFile(filepath.Join(reopened.Root, fi.Name))
		if err != nil || string(body) != "body" {
			t.Fatalf("%s: %q %v", fi.Name, body, err)
		}
	}

}

func TestApplyGroupFailedCommitHasNoDone(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	n.f.SetPersistenceProbe(PersistenceProbe{Fault: func(_, phase string, _, _ uint64) error {
		if phase == "G4" {
			return errors.New("flush failure")
		}
		return nil
	}})
	r := incoming("body", Vector{"B": 1}, 200e9)
	done := 0
	a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, bodyFetch("body"), &groupProgress{done: func() { done++ }})
	files, _ := n.f.Snapshot()
	if a != 0 || b != 1 || done != 0 || len(files) != 0 || n.f.Blocked() == nil {
		t.Fatalf("applied=%d failed=%d Done=%d files=%v blocked=%v", a, b, done, files, n.f.Blocked())
	}
}

func TestApplyGroupMergeOnlyEditDeleteDurable(t *testing.T) {
	n := applySeed(t)
	n.write(t, "gone", "gone", time.Unix(100, 0))
	n.write(t, "keep", "keep", time.Unix(100, 0))
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(n.f.Root, "gone")); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	old, _, _ := n.f.Lookup("gone")
	if !old.Deleted {
		t.Fatal("missing Scan tombstone")
	}
	edit := incoming("new", old.Version.Copy(), 200e9)
	edit.Name = "gone"
	edit.Version["B"]++
	x, _, _ := n.f.Lookup("x")
	del := FileInfo{Name: "x", Deleted: true, Version: x.Version.Copy()}
	del.Version["B"]++
	merge := incoming("keep", Vector{"B": 1}, 100e9)
	merge.Name = "keep"
	writes := 0
	n.f.SetPersistenceProbe(PersistenceProbe{Write: func(kind, _ string, _ int, _ error) {
		if kind == "log" {
			writes++
		}
	}})
	a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{merge, edit, del}, bodyFetch("new"), nil)
	if a != 3 || b != 0 || writes != 1 {
		t.Fatalf("apply %d/%d writes %d", a, b, writes)
	}
	reopened, err := Open(n.f.Root, n.f.statePath, "A", n.f.log)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	kept, _, _ := reopened.Lookup("keep")
	deleted, _, _ := reopened.Lookup("x")
	restored, _, _ := reopened.Lookup("gone")
	if kept.Version["B"] != 1 || !deleted.Deleted || restored.Deleted || restored.Hash != edit.Hash {
		t.Fatalf("replay: keep=%+v deleted=%+v restored=%+v", kept, deleted, restored)
	}
	body, err := os.ReadFile(filepath.Join(reopened.Root, "gone"))
	if err != nil || string(body) != "new" {
		t.Fatalf("resurrected body: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(reopened.Root, "x")); !os.IsNotExist(err) {
		t.Fatalf("deleted body remains: %v", err)
	}
}

func TestApplyGroupCancelAfterG1BeforeIntent(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.f.SetPersistenceProbe(PersistenceProbe{Fault: func(_, phase string, _, _ uint64) error {
		if phase == "G1" {
			cancel()
		}
		return nil
	}})
	r := incoming("body", Vector{"B": 1}, 200e9)
	a, b := n.f.ApplyRemote(ctx, "B", []FileInfo{r}, bodyFetch("body"), nil)
	if a != 0 || b != 1 {
		t.Fatalf("apply %d/%d", a, b)
	}
	if _, err := os.Stat(n.f.intentPath()); !os.IsNotExist(err) {
		t.Fatalf("cancel wrote intent: %v", err)
	}
	files, _ := n.f.Snapshot()
	if len(files) != 0 || n.f.Blocked() != nil {
		t.Fatalf("cancel published or blocked: %v %v", files, n.f.Blocked())
	}
}

func TestApplyGroupRootReplacementBeforeIntent(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	moved := false
	n.f.SetPersistenceProbe(PersistenceProbe{Fault: func(_, phase string, _, _ uint64) error {
		if phase == "G1" && !moved {
			moved = true
			return os.Rename(n.f.Root, n.f.Root+"-old")
		}
		return nil
	}})
	done := make(chan [2]int, 1)
	go func() {
		a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{incoming("body", Vector{"B": 1}, 200e9)}, bodyFetch("body"), nil)
		done <- [2]int{a, b}
	}()
	select {
	case result := <-done:
		if result != [2]int{0, 1} || n.f.Blocked() == nil {
			t.Fatalf("result %v blocked %v", result, n.f.Blocked())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("root latch deadlocked under saveMu")
	}
	if _, err := os.Stat(n.f.intentPath()); !os.IsNotExist(err) {
		t.Fatalf("stale root wrote intent: %v", err)
	}
}

func TestApplyGroupRepeatsForecastIntoConflict(t *testing.T) {
	n := applySeed(t)
	r := incoming("new", Vector{"A": 1, "B": 1}, 200e9)
	events := &applyEvents{}
	a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, func(ctx context.Context, fi FileInfo, w io.Writer) error {
		n.write(t, "x", "local", time.Unix(150, 0))
		if err := n.f.Scan(); err != nil {
			return err
		}
		return bodyFetch("new")(ctx, fi, w)
	}, events)
	if a != 1 || b != 0 || len(events.events) != 1 || events.events[0] != EventConflict {
		t.Fatalf("apply %d/%d events %v", a, b, events.events)
	}
	assertBody(t, n, "new")
	entries, err := os.ReadDir(n.f.Root)
	if err != nil {
		t.Fatal(err)
	}
	conflicts := 0
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".sync-conflict-") {
			conflicts++
			body, err := os.ReadFile(filepath.Join(n.f.Root, entry.Name()))
			if err != nil || string(body) != "local" {
				t.Fatalf("conflict %s: %q %v", entry.Name(), body, err)
			}
		}
	}
	if conflicts != 1 {
		t.Fatal("missing local conflict", conflicts)
	}
}
