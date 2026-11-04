package folder

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestApplyStageOutsideScanLock(t *testing.T) {
	n := applySeed(t)
	r := incoming("new", Vector{"B": 2}, 200e9)
	n.f.scanMu.Lock()
	done := make(chan error, 1)
	go func() {
		s, err := n.f.stageApply(context.Background(), step{r: r, act: take}, bodyFetch("new"))
		if s != nil {
			s.close(n.f)
		}
		done <- err
	}()
	select {
	case err := <-done:
		n.f.scanMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		n.f.scanMu.Unlock()
		<-done
		t.Fatal("network/local hashing waited for scanMu")
	}
	assertBody(t, n, "old")
}

func TestApplyStageRepeatsDecisionAfterLocalEdit(t *testing.T) {
	n := applySeed(t)
	r := incoming("new", Vector{"B": 1}, 200e9)
	a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, func(_ context.Context, _ FileInfo, w io.Writer) error {
		if !n.f.scanMu.TryLock() {
			t.Fatal("fetch holds scanMu")
		}
		n.f.scanMu.Unlock()
		n.write(t, "x", "local", time.Unix(300, 0))
		_, err := io.WriteString(w, "new")
		return err
	}, nil)
	if a != 0 || b != 0 {
		t.Fatalf("applied=%d failed=%d", a, b)
	}
	assertBody(t, n, "local")
}

func TestApplyStageMetadataOnlyDoesNotFetch(t *testing.T) {
	n := applySeed(t)
	files, _ := n.f.Snapshot()
	r := files[0]
	r.Version = r.Version.Copy()
	r.Version["B"]++
	r.Exec = true
	a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, func(context.Context, FileInfo, io.Writer) error {
		t.Fatal("metadata-only fetched content")
		return nil
	}, nil)
	if a != 1 || b != 0 {
		t.Fatalf("applied=%d failed=%d", a, b)
	}
	assertBody(t, n, "old")
	files, _ = n.f.Snapshot()
	if !files[0].Exec {
		t.Fatal("Exec was not applied")
	}
}
