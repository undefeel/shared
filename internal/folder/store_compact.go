package folder

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// needsCompaction is checked at idle, under scanMu/saveMu, after intent cleanup.
// removed/total describe the just-committed GC, not volatile scan statistics.
func (s *indexStore) needsCompaction(removed, total uint64) (bool, error) {
	if s.failed != nil {
		return false, s.failed
	}
	info, err := s.log.Stat()
	if err != nil {
		return false, err
	}
	h, err := decodeLogHeader(io.NewSectionReader(s.log, 0, logHeaderSize))
	if err != nil {
		return false, err
	}
	if h.StoreID != s.state.StoreID || h.BaseGeneration > s.state.Generation {
		return false, codecError()
	}
	changed := s.state.Generation > h.BaseGeneration
	return changed && (info.Size() >= 8<<20 || s.state.Generation-h.BaseGeneration >= 4096 ||
		(removed > 0 && removed <= total && (removed > total/4 || (removed == total/4 && total%4 == 0)))), nil
}

// compact requires scanMu then saveMu, between groups, with no mu held. It
// borrows the immutable durable state, retaining no second full state copy.
func (s *indexStore) compact() error { return s.compactWithSyncDir(syncDir) }

func (s *indexStore) compactWithSyncDir(syncDirectory func(*os.File) error) (err error) {
	if s.failed != nil {
		return s.failed
	}
	dir := filepath.Dir(s.log.Name())
	// Never replace a log whose transaction still needs recovery/cleanup.
	if _, e := os.Lstat(filepath.Join(dir, "intent.json")); e == nil {
		return fmt.Errorf("compaction requires intent cleanup")
	} else if !os.IsNotExist(e) {
		return e
	}
	data, err := encodeSnapshot(s.state)
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Any uncertain replacement or durability failure fences subsequent appends.
	defer func() {
		if err != nil {
			s.failed = fmt.Errorf("store compaction requires recovery: %w", err)
		}
	}()
	replace := func(name string, data []byte) error {
		tmp, e := os.CreateTemp(dir, name+".tmp-*")
		if e != nil {
			return e
		}
		defer os.Remove(tmp.Name())
		n, e := tmp.Write(data)
		if e == nil && n != len(data) {
			e = io.ErrShortWrite
		}
		// One file in each compaction phase: plain fsync plus one device full
		// flush on macOS; directory entries receive only a plain fsync.
		if e == nil {
			e = s.flush(tmp)
		}
		closeErr := tmp.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return e
		}
		if e = os.Rename(tmp.Name(), filepath.Join(dir, name)); e != nil {
			return e
		}
		return syncDirectory(d)
	}
	if err = replace("index.snapshot", data); err != nil {
		return err
	}
	// The old log stays intact until the snapshot rename is durable.
	data = nil
	if err = replace("index.log", encodeLogHeader(storeLogHeader{
		StoreID: s.state.StoreID, BaseGeneration: s.state.Generation,
	})); err != nil {
		return err
	}
	next, err := os.OpenFile(filepath.Join(dir, "index.log"), os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	old := s.log
	s.log, s.write = next, next.Write
	if err = old.Close(); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && (strings.HasPrefix(entry.Name(), "index.snapshot.tmp-") || strings.HasPrefix(entry.Name(), "index.log.tmp-")) {
			if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
