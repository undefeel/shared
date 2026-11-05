package folder

import (
	"context"
	"errors"
	"os"
	"reflect"

	"golang.org/x/sys/unix"
)

type scanBaseline struct {
	root                                  *rootFS
	generation, durable, confirmed, clock uint64
	files                                 map[string]*FileInfo
	ignore                                []string
	cache                                 map[string]scanHashCache
}

// snapshotScan is called under scanMu and takes an immutable baseline and lease.
func (f *Folder) snapshotScan() (*scanBaseline, error) {
	if err := f.guardRoot(); err != nil {
		return nil, err
	}
	r := f.root
	if !r.acquireLease() {
		return nil, ErrScanPending
	}
	f.saveMu.Lock()
	f.mu.Lock()
	b := &scanBaseline{root: r, generation: f.generation, durable: f.durableGeneration, confirmed: f.confirmedGeneration, clock: f.clock, files: make(map[string]*FileInfo, len(f.files)), ignore: append([]string(nil), f.ignore...), cache: make(map[string]scanHashCache, len(f.hashCache))}
	for k, v := range f.files {
		c := *v
		c.Version = v.Version.Copy()
		b.files[k] = &c
	}
	for k, v := range f.hashCache {
		b.cache[k] = v
	}
	f.mu.Unlock()
	f.saveMu.Unlock()
	return b, nil
}

func (f *Folder) baselineMatches(b *scanBaseline) bool {
	if f.root != b.root {
		return false
	}
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation == b.generation && f.durableGeneration == b.durable && f.confirmedGeneration == b.confirmed && f.clock == b.clock && reflect.DeepEqual(f.files, b.files) && equalStrings(f.ignore, b.ignore)
}

func scanStatAt(r *rootFS, name string) (scanStat, error) {
	if name == "." {
		info, err := r.root.Stat()
		if err != nil {
			return scanStat{}, err
		}
		return scanStatFromInfo(info), nil
	}
	p, leaf, err := r.parent(name, false)
	if err != nil {
		return scanStat{}, err
	}
	defer p.Close()
	var st unix.Stat_t
	if err = unix.Fstatat(int(p.Fd()), leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return scanStat{}, err
	}
	return scanStatFromSys(&st, st.Size, uint32(st.Mode)), nil
}

func validateScan(r *rootFS, observed map[string]scanStat, baseline map[string]*FileInfo, candidate map[string]*FileInfo, patterns []string) error {
	for name, old := range observed {
		now, err := scanStatAt(r, name)
		if err != nil || now != old {
			return ErrScanPending
		}
	}
	for name, fi := range baseline {
		if fi.Deleted || candidate[name] != nil || ignoredWith(patterns, name) {
			continue
		}
		disk := fi.Disk
		if disk == "" {
			disk = name
		}
		if _, seen := observed[disk]; seen {
			continue
		}
		_, err := scanStatAt(r, disk)
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return ErrScanPending
		}
	}
	return nil
}

func (f *Folder) scanStaged(ctx context.Context, generation uint64, checkGeneration bool, ioops scanIO) (result error) {
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
	if checkGeneration && generation != b.confirmed {
		return errors.New("stale watcher generation")
	}
	if err = f.preflightStability(ctx, ioops); err != nil {
		return err
	}
	indexed := make(map[string]bool, len(b.files))
	for name, fi := range b.files {
		if !fi.Deleted {
			indexed[name] = true
		}
	}
	candidate, patterns, cache, observed, err := f.buildScan(ctx, ioops, b.root, indexed, false, &scanPath)
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
	if err = f.commitScan(candidate, patterns, cache, false); err != nil {
		return err
	}
	present := make(map[string]struct{}, len(candidate))
	for name := range candidate {
		present[name] = struct{}{}
	}
	f.integrationStability().reconcile(present)
	return nil
}
