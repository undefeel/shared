package folder

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReuseApplyAndChangedDonor(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "reuse", true: "changed"}[changed], func(t *testing.T) {
			n := newNode(t, "A")
			defer n.f.Close()
			n.write(t, "donor", "body", time.Unix(100, 0))
			if e := n.f.Scan(); e != nil {
				t.Fatal(e)
			}
			if changed {
				n.write(t, "donor", "edit", time.Unix(101, 0))
			}
			r := incoming("body", Vector{"B": 2}, 200e9)
			r.Exec = true
			calls := 0
			a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, func(_ context.Context, _ FileInfo, w io.Writer) error {
				calls++
				_, e := io.WriteString(w, "body")
				return e
			}, nil)
			if a != 1 || b != 0 {
				t.Fatalf("apply=%d failed=%d", a, b)
			}
			want := 0
			if changed {
				want = 1
			}
			if calls != want {
				t.Fatalf("fetch=%d want=%d", calls, want)
			}
			body, _ := os.ReadFile(filepath.Join(n.f.Root, "x"))
			if string(body) != "body" {
				t.Fatal(string(body))
			}
			st, _ := os.Stat(filepath.Join(n.f.Root, "x"))
			if st.Mode().Perm() != 0755 {
				t.Fatalf("mode=%o", st.Mode().Perm())
			}
			r.Version = Vector{"B": 3}
			r.Exec = false
			if e := os.Chmod(filepath.Join(n.f.Root, "x"), 0700); e != nil {
				t.Fatal(e)
			}
			if e := n.f.Scan(); e != nil {
				t.Fatal(e)
			}
			// A dominating remote vector keeps rw permissions while changing Exec.
			idx, _ := n.f.Snapshot()
			for _, fi := range idx {
				if fi.Name == "x" {
					r.Version = fi.Version.Copy()
					r.Version["B"] += 10
				}
			}
			a, b = n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, func(context.Context, FileInfo, io.Writer) error { t.Fatal("equal target fetched"); return nil }, nil)
			if a != 1 || b != 0 {
				t.Fatalf("exec apply=%d failed=%d", a, b)
			}
			st, _ = os.Stat(filepath.Join(n.f.Root, "x"))
			if st.Mode().Perm() != 0600 {
				t.Fatalf("permissions=%o", st.Mode().Perm())
			}
		})
	}
}

func TestReuseChangedDuringCloneFallsBack(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	n.write(t, "donor", "body", time.Unix(100, 0))
	if e := n.f.Scan(); e != nil {
		t.Fatal(e)
	}
	r := incoming("body", Vector{"B": 1}, 200e9)
	calls := 0
	tmp, e := n.f.downloadOrReuseWith(context.Background(), r, func(_ context.Context, _ FileInfo, w io.Writer) error {
		calls++
		_, e := io.WriteString(w, "body")
		return e
	}, func(src *os.File, name string, size int64) (*os.File, error) {
		dst, e := n.f.root.cloneFile(src, name, size)
		if e == nil {
			n.write(t, "donor", "edit", time.Unix(101, 0))
		}
		return dst, e
	})
	if e != nil {
		t.Fatal(e)
	}
	defer n.f.root.unlink(tmp, false)
	if calls != 1 {
		t.Fatalf("network calls=%d", calls)
	}
	body, e := os.ReadFile(filepath.Join(n.f.Root, tmp))
	if e != nil || string(body) != "body" {
		t.Fatalf("body=%q err=%v", body, e)
	}
}

func TestReuseSymlinkDonorFallsBack(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	n.write(t, "donor", "body", time.Unix(100, 0))
	if e := n.f.Scan(); e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("body"), 0600)
	os.Remove(filepath.Join(n.f.Root, "donor"))
	os.Symlink(outside, filepath.Join(n.f.Root, "donor"))
	calls := 0
	r := incoming("body", Vector{"B": 1}, 200e9)
	tmp, e := n.f.downloadOrReuse(context.Background(), r, func(_ context.Context, _ FileInfo, w io.Writer) error {
		calls++
		_, e := io.WriteString(w, "body")
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	defer n.f.root.unlink(tmp, false)
	if calls != 1 {
		t.Fatal("followed donor symlink")
	}
}

func TestReuseDurableVectorAndRestartIndex(t *testing.T) {
	n := newNode(t, "A")
	defer func() { n.f.Close() }()
	n.write(t, "donor", "body", time.Unix(100, 0))
	if e := n.f.Scan(); e != nil {
		t.Fatal(e)
	}
	root, state, logger := n.f.Root, n.f.statePath, n.f.log
	r := incoming("body", Vector{"B": 1}, 200e9)
	a, b := n.f.ApplyRemote(context.Background(), "B", []FileInfo{r}, func(context.Context, FileInfo, io.Writer) error { t.Fatal("fetched"); return nil }, nil)
	if a != 1 || b != 0 {
		t.Fatalf("apply=%d/%d", a, b)
	}
	if e := n.f.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(root, state, "A", logger)
	if e != nil {
		t.Fatal(e)
	}
	n.f = reopened
	idx, _ := n.f.Snapshot()
	found := false
	for _, fi := range idx {
		if fi.Name == "x" {
			found = true
			if fi.Version.Compare(r.Version) != Equal {
				t.Fatal("vector lost")
			}
		}
	}
	if !found {
		t.Fatal("target lost")
	}
	r.Name = "next"
	if !n.f.hasReusableContent(r) {
		t.Fatal("restart index unavailable")
	}
}

func TestReuseDeleteUpdatesIndex(t *testing.T) {
	n := newNode(t, "A")
	defer n.f.Close()
	n.write(t, "donor", "body", time.Unix(100, 0))
	n.write(t, "keep", "other", time.Unix(100, 0))
	if e := n.f.Scan(); e != nil {
		t.Fatal(e)
	}
	r := incoming("body", Vector{"B": 1}, 200e9)
	if !n.f.hasReusableContent(r) {
		t.Fatal("missing donor")
	}
	if e := os.Remove(filepath.Join(n.f.Root, "donor")); e != nil {
		t.Fatal(e)
	}
	if e := n.f.Scan(); e != nil {
		t.Fatal(e)
	}
	if candidates := n.f.reuseCandidates(r); len(candidates) != 0 {
		t.Fatalf("deleted donor indexed: %+v", candidates)
	}
}
