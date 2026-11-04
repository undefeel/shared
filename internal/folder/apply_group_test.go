package folder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	barrier.Add(64)
	frames := 0
	n.f.SetPersistenceProbe(PersistenceProbe{Phase: func(_, phase string) {
		if phase == "G4" {
			frames++
		}
	}})
	done := 0
	progress := &groupProgress{done: func() {
		files, _ := n.f.Snapshot()
		if len(files) != 64 || frames != 1 {
			t.Errorf("early/partial Done: %d files, %d frames", len(files), frames)
		}
		done++
	}}
	a, b := n.f.ApplyRemote(context.Background(), "B", remote, func(_ context.Context, _ FileInfo, w io.Writer) error {
		barrier.Done()
		barrier.Wait()
		_, e := io.WriteString(w, "body")
		return e
	}, progress)
	if a != 64 || b != 0 || frames != 1 || done != 64 {
		t.Fatalf("applied=%d failed=%d frames=%d Done=%d", a, b, frames, done)
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

func TestApplyGroupResurrectsScanTombstone(t *testing.T) {
	n := applySeed(t)
	n.write(t, "keep", "keep", time.Unix(100, 0))
	if err := os.Remove(filepath.Join(n.f.Root, "x")); err != nil {
		t.Fatal(err)
	}
	if err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
	old, _, _ := n.f.Lookup("x")
	if !old.Deleted {
		t.Fatal("missing tombstone")
	}
	r := incoming("new", old.Version.Copy(), 200e9)
	r.Version["B"]++
	a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, bodyFetch("new"), nil)
	if a != 1 || b != 0 {
		t.Fatalf("apply %d/%d", a, b)
	}
	reopened, err := Open(n.f.Root, n.f.statePath, "A", n.f.log)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, _, _ := reopened.Lookup("x")
	if got.Deleted || got.Hash != r.Hash {
		t.Fatal(got)
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
