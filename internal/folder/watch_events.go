package folder

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"strings"
)

var ErrStaleWatchGeneration = errors.New("watch: stale generation")

// WatchBatch is a coalesced set of relative paths that may need rescanning.
type WatchBatch struct {
	Paths        []string
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

// filepathRel is isolated here to keep watcher paths slash-normalized on every OS.
func filepathRel(root, name string) (string, error) { return filepath.Rel(root, name) }

// Watch preserves the original callback API for older callers.
func (f *Folder) Watch(ctx context.Context, onChange func()) (func(), <-chan struct{}, error) {
	return f.WatchPaths(ctx, func(WatchBatch) { onChange() })
}
