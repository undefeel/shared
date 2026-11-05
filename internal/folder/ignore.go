package folder

import (
	"path"
	"strings"
)

const (
	tmpPrefix  = ".sharedir-tmp-"
	ignoreFile = ".sharedirignore"
)

// Built-in exclusions apply independently of the user ignore file.
var builtinIgnore = []string{
	".DS_Store", "._*", ".Spotlight-V100", ".Trashes", ".fseventsd",
	".TemporaryItems", ".localized", "Thumbs.db", "desktop.ini", tmpPrefix + "*",
	"node_modules", ".venv", "__pycache__", ".next", "dist",
	"*.swp", "*.swo", "*~", ".#*", "#*#", "~$*", "*.crdownload", "*.part", "*.download",
}

// Ignored checks slash-separated relative paths against built-ins and user rules.
func (f *Folder) Ignored(name string) bool {
	f.mu.Lock()
	patterns := append([]string(nil), f.ignore...)
	f.mu.Unlock()
	return ignoredWith(patterns, name)
}

// ignoredWith applies built-ins and user rules once, independent of Folder state.
func ignoredWith(user []string, name string) bool {
	return matchIgnore(append(append([]string(nil), builtinIgnore...), user...), name)
}

func matchIgnore(patterns []string, name string) bool {
	for p := strings.TrimPrefix(path.Clean(name), "./"); p != "." && p != "" && p != "/"; p = path.Dir(p) {
		base := path.Base(p)
		for _, pat := range patterns {
			// Component rules prevent substring exclusions (for example distfile).
			if !strings.ContainsAny(pat, "/") && base == pat {
				return true
			}
			if ok, _ := path.Match(pat, base); ok || okPath(pat, p) {
				return true
			}
		}
	}
	return false
}

func okPath(pattern, name string) bool {
	ok, _ := path.Match(pattern, name)
	return ok
}
