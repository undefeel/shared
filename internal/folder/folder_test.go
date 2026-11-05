package folder

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVectorCompare(t *testing.T) {
	cases := []struct {
		a, b Vector
		want Ordering
	}{
		{Vector{"A": 1}, Vector{"A": 1}, Equal},
		{Vector{"A": 2}, Vector{"A": 1}, Greater},
		{Vector{"A": 1}, Vector{"A": 1, "B": 1}, Lesser},
		{Vector{"A": 2}, Vector{"A": 1, "B": 1}, Concurrent},
		{Vector{}, Vector{}, Equal},
	}
	for _, c := range cases {
		if got := c.a.Compare(c.b); got != c.want {
			t.Errorf("%v vs %v = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

type node struct {
	f  *Folder
	id string
}

func fastFolder(f *Folder) *Folder {
	f.integrationStability().interval = time.Millisecond
	return f
}

func newNode(t *testing.T, id string) *node {
	n := newDefaultNode(t, id)
	fastFolder(n.f)
	return n
}

func newDefaultNode(t *testing.T, id string) *node {
	dir := t.TempDir()
	f, err := Open(filepath.Join(dir, "data"), filepath.Join(dir, "index.json"), id, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return &node{f, id}
}

func (n *node) write(t *testing.T, name, content string, mtime time.Time) {
	p := filepath.Join(n.f.Root, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mtime, mtime)
}

func (n *node) read(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(n.f.Root, name))
	return string(b), err == nil
}

// pull — одна итерация: dst сканирует себя и забирает индекс src.
func pull(t *testing.T, dst, src *node) {
	t.Helper()
	if err := src.f.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := dst.f.Scan(); err != nil {
		t.Fatal(err)
	}
	idx, _ := src.f.Snapshot()
	fetch := func(ctx context.Context, fi FileInfo, w io.Writer) error {
		_, p, _ := src.f.Lookup(fi.Name)
		file, err := os.Open(p)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(w, file)
		return err
	}
	if _, failed := dst.f.ApplyRemote(context.Background(), src.id, idx, fetch, nil); failed > 0 {
		t.Fatalf("%s <- %s: %d ошибок", dst.id, src.id, failed)
	}
}

func syncBoth(t *testing.T, a, b *node) {
	t.Helper()
	for range 3 {
		pull(t, a, b)
		pull(t, b, a)
	}
}

func listFiles(n *node) []string {
	var out []string
	filepath.Walk(n.f.Root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(n.f.Root, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func TestSyncLifecycle(t *testing.T) {
	a, b := newNode(t, "AAAAAAA"), newNode(t, "BBBBBBB")
	now := time.Now().Truncate(time.Second)

	a.write(t, "docs/отчёт й.txt", "v1", now)
	b.write(t, "b.txt", "from b", now)
	syncBoth(t, a, b)
	if got, _ := b.read("docs/отчёт й.txt"); got != "v1" {
		t.Fatalf("b не получил файл: %q", got)
	}
	if got, _ := a.read("b.txt"); got != "from b" {
		t.Fatalf("a не получил файл: %q", got)
	}

	// Правка на b доходит до a.
	b.write(t, "docs/отчёт й.txt", "v2", now.Add(time.Minute))
	syncBoth(t, a, b)
	if got, _ := a.read("docs/отчёт й.txt"); got != "v2" {
		t.Fatalf("правка не дошла: %q", got)
	}

	// Удаление на a доходит до b, пустой каталог исчезает.
	os.Remove(filepath.Join(a.f.Root, "docs/отчёт й.txt"))
	syncBoth(t, a, b)
	if _, ok := b.read("docs/отчёт й.txt"); ok {
		t.Fatal("удаление не дошло")
	}
	if _, err := os.Stat(filepath.Join(b.f.Root, "docs")); err == nil {
		t.Fatal("пустой каталог не удалён")
	}

	// Параллельная правка: побеждает более поздняя, проигравшая сохраняется копией.
	a.write(t, "b.txt", "edit A", now.Add(2*time.Minute))
	b.write(t, "b.txt", "edit B", now.Add(3*time.Minute))
	syncBoth(t, a, b)
	for _, n := range []*node{a, b} {
		if got, _ := n.read("b.txt"); got != "edit B" {
			t.Fatalf("%s: b.txt = %q, ждали edit B", n.id, got)
		}
		var conflict string
		for _, p := range listFiles(n) {
			if strings.Contains(p, "sync-conflict") {
				conflict = p
			}
		}
		if got, _ := n.read(conflict); conflict == "" || got != "edit A" {
			t.Fatalf("%s: нет конфликтной копии с edit A (файлы: %v)", n.id, listFiles(n))
		}
	}

	// Индексы сошлись.
	ia, _ := a.f.Snapshot()
	ib, _ := b.f.Snapshot()
	if len(ia) != len(ib) {
		t.Fatalf("индексы разной длины: %d vs %d", len(ia), len(ib))
	}
	for _, fa := range ia {
		fb, _, ok := b.f.Lookup(fa.Name)
		if !ok || fa.Version.Compare(fb.Version) != Equal || fa.Hash != fb.Hash {
			t.Errorf("%s: расхождение %v / %v", fa.Name, fa.Version, fb.Version)
		}
	}
}

func TestRejectsBadPaths(t *testing.T) {
	for _, p := range []string{"../x", "/etc/passwd", "a/../../x", "a\\b", "", "./a"} {
		if validName(p) {
			t.Errorf("%q принят", p)
		}
	}
}
