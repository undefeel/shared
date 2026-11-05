package folder

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// SIGKILL exercises process recovery; it does not model hardware write caches.
func TestRecoveryProcess(t *testing.T) {
	mode := os.Getenv("SHAREDIR_CRASH_MODE")
	if mode == "" {
		return
	}
	home := os.Getenv("SHAREDIR_CRASH_HOME")
	f, err := Open(filepath.Join(home, "root"), filepath.Join(home, "index.json"), "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	fastFolder(f)
	defer f.Close()
	if mode == "restart" {
		fmt.Println("ready")
		for {
			time.Sleep(time.Hour)
		}
	}
	f.SetPersistenceProbe(PersistenceProbe{Phase: func(_, phase string) {
		if (mode == "1" && phase == "G3") || (mode == "3" && phase == "G4") || (mode == "4" && phase == "G5") {
			fmt.Println("ready")
			select {}
		}
	}, Fault: func(_, phase string, _, _ uint64) error {
		if mode == "2" && phase == "G3a" {
			fmt.Println("ready")
			select {}
		}
		return nil
	}})
	fi := incoming("new", Vector{"A": 1, "B": 1}, 200e9)
	if n, failed := f.ApplyRemote(context.Background(), "B", []FileInfo{fi}, bodyFetch("new"), nil); n != 1 || failed != 0 {
		t.Fatalf("apply %d/%d", n, failed)
	}
	t.Fatal("crash barrier not reached")
}
func killRecoveryProcess(t *testing.T, home, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryProcess$")
	cmd.Env = append(os.Environ(), "SHAREDIR_CRASH_HOME="+home, "SHAREDIR_CRASH_MODE="+mode)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(out)
	if !scanner.Scan() || scanner.Text() != "ready" {
		lines := scanner.Text()
		for scanner.Scan() {
			lines += "\n" + scanner.Text()
		}
		t.Fatalf("child did not reach crash barrier: %s (%v)", lines, scanner.Err())
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
}
func crashIntegration(t *testing.T, phase string) {
	t.Helper()
	home := t.TempDir()
	f, err := Open(filepath.Join(home, "root"), filepath.Join(home, "index.json"), "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	fastFolder(f)
	if err = os.WriteFile(filepath.Join(f.Root, "x"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.Scan(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	killRecoveryProcess(t, home, phase)
	// Kill a fresh process after startup recovery, then recover once more.
	killRecoveryProcess(t, home, "restart")
	f, err = Open(filepath.Join(home, "root"), filepath.Join(home, "index.json"), "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	fastFolder(f)
	defer f.Close()
	if f.Blocked() != nil {
		t.Fatal(f.Blocked())
	}
	entries, err := os.ReadDir(f.Root)
	if err != nil {
		t.Fatal(err)
	}
	bodies := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix+"capture-") {
			t.Fatal("capture retained", e.Name())
		}
		b, err := os.ReadFile(filepath.Join(f.Root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		bodies += string(b)
	}
	if phase == "4" {
		journalBytes(t, f, "x", "new")
	} else if !strings.Contains(bodies, "old") {
		t.Fatal("uncommitted bytes lost", bodies)
	}
	if _, err = os.Stat(f.intentPath()); !os.IsNotExist(err) {
		t.Fatal("intent retained", err)
	}
	if err = f.Scan(); err != nil {
		t.Fatal(err)
	}
}
func TestCrashRecoveryIntegration(t *testing.T) {
	for _, checkpoint := range []string{"1", "2", "3", "4"} {
		t.Run(checkpoint, func(t *testing.T) { crashIntegration(t, checkpoint) })
	}
}

func TestWatcherScanApplySaveIntegration(t *testing.T) {
	f := watchFixture(t)
	events := make(chan struct{}, 1)
	stop, done, err := f.Watch(context.Background(), func() {
		select {
		case events <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(); <-done }()
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 6; i++ {
				switch worker {
				case 0:
					_ = f.Scan() // A changing download tmp may invalidate a staged scan; retry below.
				case 1:
					if err := f.save(); err != nil {
						errs <- err
					}
				default:
					fi := incoming("body", Vector{fmt.Sprint(worker): 1}, 200e9)
					fi.Name = fmt.Sprintf("peer%d-%d", worker, i)
					if n, failed := f.ApplyRemote(context.Background(), fmt.Sprint(worker), []FileInfo{fi}, bodyFetch("body"), nil); n != 1 || failed != 0 {
						errs <- fmt.Errorf("apply %d/%d", n, failed)
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	waitWatch(t, events)
	if err = f.Scan(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.Root, f.statePath, f.me, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	files, _ := reopened.Snapshot()
	if len(files) != 12 {
		t.Fatal("lost concurrent commits", len(files))
	}
	for _, fi := range files {
		if fi.Deleted {
			t.Fatal("spurious tombstone", fi)
		}
	}
}
func TestTwoPeersConcurrentEditDeleteIntegration(t *testing.T) {
	a, b := newNode(t, "A"), newNode(t, "B")
	defer a.f.Close()
	defer b.f.Close()
	a.write(t, "x", "base", time.Unix(100, 0))
	a.write(t, "keep", "keep", time.Unix(100, 0))
	syncBoth(t, a, b)
	a.write(t, "x", "edit", time.Unix(200, 0))
	if err := os.Remove(filepath.Join(b.f.Root, "x")); err != nil {
		t.Fatal(err)
	}
	if err := a.f.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := b.f.Scan(); err != nil {
		t.Fatal(err)
	}
	syncBoth(t, a, b)
	for _, n := range []*node{a, b} {
		assertBody(t, n, "edit")
	}
	fa, _, _ := a.f.Lookup("x")
	fb, _, _ := b.f.Lookup("x")
	if fa.Deleted || fb.Deleted || fa.Version.Compare(fb.Version) != Equal {
		t.Fatal("edit/delete did not converge")
	}
}
