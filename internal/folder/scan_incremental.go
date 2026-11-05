package folder

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ScanPaths scans the affected paths atomically. Rename callers must include both sides.
func (f *Folder) ScanPaths(ctx context.Context, scopes []string) error {
	return f.scanPathsWith(ctx, scopes, f.rootScanIO())
}

func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	full := false
	result := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s == "" || s == "." {
			full = true
			continue
		}
		if strings.ContainsAny(s, "\\\x00") || strings.HasPrefix(s, "/") {
			return nil, errors.New("scan: invalid scope: " + s)
		}
		for _, part := range strings.Split(s, "/") {
			if part == ".." {
				return nil, errors.New("scan: invalid scope: " + s)
			}
		}
		s = norm.NFC.String(path.Clean(s))
		if !validName(s) {
			return nil, errors.New("scan: invalid scope: " + s)
		}
		if s == ignoreFile {
			full = true
		}
		result = append(result, s)
	}
	if full {
		return nil, nil
	}
	sort.Strings(result)
	out := result[:0]
	for _, s := range result {
		if len(out) == 0 || !scopeCovered(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

func scopeCovered(scopes []string, name string) bool {
	if scopes == nil {
		return true
	}
	for _, s := range scopes {
		if name == s || strings.HasPrefix(name, s+"/") {
			return true
		}
	}
	return false
}
func scopeRelevant(scopes []string, name string) bool {
	if scopeCovered(scopes, name) {
		return true
	}
	for _, s := range scopes {
		if strings.HasPrefix(s, name+"/") {
			return true
		}
	}
	return false
}

func (f *Folder) scanPathsWith(ctx context.Context, paths []string, ioops scanIO) (result error) {
	scopes, err := normalizeScopes(paths)
	if err != nil {
		return err
	}
	if scopes == nil {
		return f.scanStaged(ctx, 0, false, ioops)
	}
	scanPath := ""
	defer func() { f.recordScanResult(result, scanPath) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	f.scanMu.Lock()
	scanPath = f.Root
	b, err := f.snapshotScan()
	f.scanMu.Unlock()
	if err != nil {
		return err
	}
	defer b.root.releaseLease()
	if err = f.preflightScopes(ctx, ioops, scopes); err != nil {
		return err
	}
	indexed := make(map[string]bool, len(b.files))
	for name, fi := range b.files {
		if !fi.Deleted {
			indexed[name] = true
		}
	}
	candidate, patterns, cache, observed, err := f.buildScopedScan(ctx, ioops, b.root, indexed, false, &scanPath, scopes, b)
	if err != nil {
		var re *rootProbeError
		if errors.As(err, &re) {
			f.scanMu.Lock()
			defer f.scanMu.Unlock()
			if f.root != b.root || f.ConfirmedGeneration() != b.confirmed {
				return ErrScanPending
			}
			return f.blockRoot("root_unavailable", err.Error())
		}
		return err
	}
	if !equalStrings(patterns, b.ignore) {
		return f.scanStaged(ctx, 0, false, ioops)
	}
	// Confirm an apparently empty root with a complete scan before latching.
	if len(candidate) == 0 && len(indexed) != 0 {
		return f.scanStaged(ctx, 0, false, ioops)
	}
	if ioops.beforeCommit != nil {
		ioops.beforeCommit()
	}
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	if err = ctx.Err(); err != nil {
		return err
	}
	if f.root != b.root {
		return ErrScanPending
	}
	if err = f.guardRoot(); err != nil {
		return err
	}
	if !f.baselineMatches(b) {
		return ErrScanPending
	}
	if err = validateScan(b.root, observed, b.files, candidate, patterns); err != nil {
		return err
	}
	return f.commitScan(candidate, patterns, cache, false)
}
