package folder

import "testing"

func TestBuiltinsBeforeScan(t *testing.T) {
	f := &Folder{}
	for _, name := range []string{"node_modules/a", "x/.venv/a", "x/__pycache__/a", ".next/cache", "a/dist/main.js", ".sharedir-tmp-x", "a/a.swp", "a.swo", "a~", ".#a", "#a#", "~$a", "a.crdownload", "a.part", "a.download"} {
		if !f.Ignored(name) {
			t.Errorf("%q must be excluded", name)
		}
	}
	for _, name := range []string{".git/config", "distfile", "my_node_modules/a", "a/venv/b", "a.part.txt"} {
		if f.Ignored(name) {
			t.Errorf("%q must be included", name)
		}
	}
	f.ignore = []string{"custom/*"}
	if !f.Ignored("custom/a") || !f.Ignored("dist/a") || f.Ignored("other/a") {
		t.Fatal("user rules must add restrictions to builtins")
	}
}
