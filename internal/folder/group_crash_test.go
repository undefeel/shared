package folder

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These checkpoints exercise process termination, not loss of hardware caches.
// G1-G5 callbacks mark phase entry; G5 follows durable append and publication.
func crashReady() { fmt.Println("READY"); select {} }

func TestGroupCrashChild(t *testing.T) {
	phase := os.Getenv("SHAREDIR_GROUP_CRASH_PHASE")
	if phase == "" {
		return
	}
	root, state := os.Getenv("SHAREDIR_GROUP_CRASH_ROOT"), os.Getenv("SHAREDIR_GROUP_CRASH_STATE")
	f, err := Open(root, state, "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Blocked() != nil {
		t.Fatal(f.Blocked())
	}
	if phase == "verify-before" || phase == "verify-after" || phase == "verify-installed" {
		crashVerify(t, f, phase == "verify-after", phase == "verify-installed")
		return
	}
	if strings.HasPrefix(phase, "compact-") {
		n := 0
		err = f.store.compactWithSyncDir(func(d *os.File) error {
			n++
			if (phase == "compact-snapshot" && n == 1) || (phase == "compact-log" && n == 2) {
				crashReady()
			}
			return syncDir(d)
		})
		t.Fatalf("compaction checkpoint not reached: %v", err)
	}
	parent, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	g := groupIntent{Schema: storeSchema, StoreID: f.store.state.StoreID, TxID: [16]byte{91}, BaseGeneration: f.store.state.Generation, Generation: f.store.state.Generation + 1, Clock: f.clock, Binding: f.binding, ConfirmedGeneration: f.confirmedGeneration}
	var trees []*commitTree
	for _, name := range []string{"replace", "delete"} {
		before, _, ok := f.Lookup(name)
		if !ok {
			t.Fatal("missing", name)
		}
		after := before
		after.Version = before.Version.Copy()
		after.Version["B"]++
		cap, e := f.root.prepareCapture(name)
		if e != nil {
			t.Fatal(e)
		}
		defer cap.Close()
		old, e := f.root.openRead(name)
		if e != nil {
			t.Fatal(e)
		}
		dev, ino, e := fileDevice(old)
		old.Close()
		if e != nil {
			t.Fatal(e)
		}
		entry := groupIntentEntry{Target: name, Before: &before, After: &after, Capture: cap.Recovery, Identity: captureIdentity{dev, ino}, Conflict: name + ".sync-conflict-crash"}
		tree := &commitTree{capture: cap, dirs: []*os.File{parent}, mutate: func() error { return nil }}
		if name == "replace" {
			after = incoming("new", after.Version, 200e9)
			after.Name = name
			entry.After = &after
			entry.Tmp = tmpPrefix + "crash-incoming"
			file, e := f.root.create(entry.Tmp)
			if e != nil {
				t.Fatal(e)
			}
			defer file.Close()
			if _, e = file.WriteString("new"); e != nil {
				t.Fatal(e)
			}
			dev, ino, e = fileDevice(file)
			if e != nil {
				t.Fatal(e)
			}
			entry.IncomingIdentity = captureIdentity{dev, ino}
			tree.incoming = file
			tree.mutate = func() error { return renameNoReplace(parent, entry.Tmp, parent, name) }
		} else {
			after.Deleted = true
			after.Hash = ""
			after.Size = 0
			entry.After = &after
		}
		g.Entries = append(g.Entries, entry)
		trees = append(trees, tree)
	}
	if phase == "G0" {
		crashReady()
	}
	f.SetPersistenceProbe(PersistenceProbe{Phase: func(_, p string) {
		if p == phase {
			crashReady()
		}
	}})
	err = runGroupCommit(f, g, trees, []*os.File{parent}, func() {
		for _, e := range g.Entries {
			f.files[e.After.Name] = e.After
		}
	})
	t.Fatalf("group checkpoint not reached: %v", err)
}

func crashVerify(t *testing.T, f *Folder, committed, installed bool) {
	t.Helper()
	for name, want := range map[string]string{"prior": "prior", "replace": "old-replace"} {
		if name == "replace" && (committed || installed) {
			want = "new"
		}
		b, err := os.ReadFile(filepath.Join(f.Root, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
		fi, _, ok := f.Lookup(name)
		if !ok || fi.Deleted || fi.Hash != incoming(func() string {
			if name == "replace" && installed {
				return "old-replace"
			}
			return want
		}(), fi.Version, 0).Hash {
			t.Fatalf("record %s: %+v", name, fi)
		}
	}
	if installed {
		b, err := os.ReadFile(filepath.Join(f.Root, "replace.sync-conflict-crash"))
		if err != nil || string(b) != "old-replace" {
			t.Fatalf("conflict: %q %v", b, err)
		}
	}
	for _, name := range []string{"tomb", "delete"} {
		fi, _, ok := f.Lookup(name)
		deleted := name == "tomb" || committed
		if !ok || fi.Deleted != deleted {
			t.Fatalf("record %s: %+v", name, fi)
		}
		b, err := os.ReadFile(filepath.Join(f.Root, name))
		if deleted && !os.IsNotExist(err) {
			t.Fatalf("%s exists: %v", name, err)
		}
		if !deleted && (err != nil || string(b) != "old-delete") {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
}

func crashChild(t *testing.T, root, state, phase string, kill bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestGroupCrashChild$")
	cmd.Env = append(os.Environ(), "SHAREDIR_GROUP_CRASH_ROOT="+root, "SHAREDIR_GROUP_CRASH_STATE="+state, "SHAREDIR_GROUP_CRASH_PHASE="+phase, "GORACE=atexit_sleep_ms=0")
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if kill {
		scanner := bufio.NewScanner(out)
		if !scanner.Scan() || scanner.Text() != "READY" {
			_ = cmd.Wait()
			t.Fatalf("%s: checkpoint missing: %s %v", phase, scanner.Text(), scanner.Err())
		}
		if err = cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		err = cmd.Wait()
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s: expected killed process: %v", phase, err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() {
			t.Fatalf("%s: not signaled: %v", phase, err)
		}
	} else {
		output, e := bufio.NewReader(out).ReadString(0)
		err = cmd.Wait()
		if err != nil {
			t.Fatalf("%s: %s %v / %v", phase, output, e, err)
		}
	}
}

func crashFixture(t *testing.T, stateBase string) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	stateDir := t.TempDir()
	if stateBase != "" {
		var err error
		stateDir, err = os.MkdirTemp(stateBase, "sharedir-crash-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(stateDir) })
	}
	state := filepath.Join(stateDir, "index.json")
	f, err := Open(root, state, "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"replace": "old-replace", "delete": "old-delete", "tomb": "tomb"} {
		if err = os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.Scan(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(root, "tomb")); err != nil {
		t.Fatal(err)
	}
	if err = f.Scan(); err != nil {
		t.Fatal(err)
	}
	prior := incoming("prior", Vector{"B": 1}, 200e9)
	prior.Name = "prior"
	if a, b := f.ApplyRemote(context.Background(), "B", []FileInfo{prior}, bodyFetch("prior"), nil); a != 1 || b != 0 {
		t.Fatalf("prior apply %d/%d", a, b)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return root, state
}

func crashMatrix(t *testing.T, stateBase string) {
	t.Helper()
	phases := []string{"G0", "G1", "G2", "G3", "G4", "G5", "compact-snapshot", "compact-log"}
	repeats := 1
	if os.Getenv("SHAREDIR_CRASH") == "1" {
		repeats = 3
	}
	for n := 0; n < repeats; n++ {
		for _, phase := range phases {
			t.Run(fmt.Sprintf("%s-%d", phase, n), func(t *testing.T) {
				root, state := crashFixture(t, stateBase)
				crashChild(t, root, state, phase, true)
				verify := "verify-before"
				if phase == "G5" {
					verify = "verify-after"
				} else if phase == "G4" {
					verify = "verify-installed"
				}
				crashChild(t, root, state, verify, false)
				crashChild(t, root, state, verify, false)
			})
		}
	}
}

func TestGroupCrashMatrix(t *testing.T) { crashMatrix(t, "") }

func TestGroupCrashCrossDevice(t *testing.T) {
	root := t.TempDir()
	candidates := []string{os.Getenv("SHAREDIR_CRASH_STATE_DIR"), "/dev/shm"}
	var rootStat syscall.Stat_t
	if err := syscall.Stat(root, &rootStat); err != nil {
		t.Fatal(err)
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat(dir, &st) != nil || st.Dev == rootStat.Dev {
			continue
		}
		probe, err := os.MkdirTemp(dir, "sharedir-crash-probe-")
		if err != nil {
			continue
		}
		os.Remove(probe)
		crashMatrix(t, dir)
		return
	}
	t.Skip("no writable alternate device; set SHAREDIR_CRASH_STATE_DIR")
}
