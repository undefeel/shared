package folder

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var ErrStaleWatchGeneration = errors.New("watch: stale generation")

// WatchBatch is a coalesced set of relative paths that may need rescanning.
type WatchBatch struct {
	Paths        []string
	RenamePaths  []string // rename targets, independently fingerprinted
	RenameFull   bool     // an unmatched/ambiguous rename needs full reconciliation
	FullRequired bool
	RootChanged  bool
	Generation   uint64
}

// ScanWatchBatch scans only the dirty paths unless watcher history was lost.
func (f *Folder) ScanWatchBatch(ctx context.Context, b WatchBatch) error {
	if b.Generation != f.ConfirmedGeneration() {
		return ErrStaleWatchGeneration
	}
	if b.FullRequired || b.RootChanged {
		return f.ScanGeneration(ctx, b.Generation)
	}
	if len(b.Paths) == 0 {
		return nil
	}
	return f.scanPathsGeneration(ctx, b.Paths, b.Generation)
}

// normalizeWatchPath converts an absolute event path to a safe relative path.
func (f *Folder) normalizeWatchPath(name string) (string, bool) {
	rel, err := filepathRel(f.Root, name)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", false
	}
	rel = path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if rel == ignoreFile {
		return "", true
	}
	if f.Ignored(rel) {
		return "", false
	}
	return rel, true
}

// relativeWatchEvent accepts event paths only within the watched root. Root
// metadata events are distinguished from root replacement flags by callers.
func relativeWatchEvent(root, event string) (rel string, inTree, rootEvent bool) {
	rel, err := filepath.Rel(root, event)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false, false
	}
	if rel == "." {
		return "", true, true
	}
	return strings.ReplaceAll(rel, string(filepath.Separator), "/"), true, false
}

// filepathRel is isolated here to keep watcher paths slash-normalized on every OS.
func filepathRel(root, name string) (string, error) { return filepath.Rel(root, name) }

// Watch preserves the original callback API for older callers.
func (f *Folder) Watch(ctx context.Context, onChange func()) (func(), <-chan struct{}, error) {
	return f.WatchPaths(ctx, func(WatchBatch) { onChange() })
}

func (f *Folder) addWatchTree(root, start string, add func(string) error) error {
	return filepath.WalkDir(start, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d == nil {
			return fmt.Errorf("watch: missing entry %s", p)
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." && f.Ignored(strings.ReplaceAll(rel, string(filepath.Separator), "/")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		st, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		return add(p)
	})
}

func watchLossBatch(gen uint64, root bool) WatchBatch {
	return WatchBatch{FullRequired: true, RootChanged: root, Generation: gen}
}
func watchRenameBatch(rel string, gen uint64) WatchBatch {
	paths := []string{rel}
	if parent := path.Dir(rel); parent != "." {
		paths = append(paths, parent)
	}
	return WatchBatch{Paths: paths, RenamePaths: []string{rel}, RenameFull: true, FullRequired: true, Generation: gen}
}
