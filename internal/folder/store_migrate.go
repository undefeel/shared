package folder

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// migrateIndexStore runs under the exclusive home lock, before workers start.
// prepareLegacy must initialize/check the root and run the existing recoverIntent
// (including finishNewIntent) against JSON, never against the new store. It must
// return durability/recovery failures; a persistent root latch is valid state.
func migrateIndexStore(path string, prepareLegacy func() error) (*indexStore, error) {
	return migrateStore(path, prepareLegacy, syncFile, syncDir)
}

func migrateStore(path string, prepare func() error, flush, syncDirectory func(*os.File) error) (*indexStore, error) {
	dir := filepath.Dir(path)
	snapshotPath, logPath := filepath.Join(dir, "index.snapshot"), filepath.Join(dir, "index.log")
	data, err := os.ReadFile(snapshotPath)
	if err == nil {
		snapshot, e := decodeSnapshot(bytes.NewReader(data))
		if e != nil {
			return nil, e
		}
		return finishMigration(logPath, snapshot, flush, syncDirectory)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	// Reject invalid legacy input before recovery can touch the tree.
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if _, err = legacySnapshot(data, time.Now()); err != nil {
		return nil, err
	}
	if prepare == nil {
		return nil, fmt.Errorf("migration requires legacy root/intent recovery")
	}
	if err = prepare(); err != nil {
		return nil, err
	}
	// Recovery may have durably finished a legacy transaction or set a latch.
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	snapshot, err := legacySnapshot(data, time.Now())
	if err != nil {
		return nil, err
	}
	if _, err = rand.Read(snapshot.StoreID[:]); err != nil {
		return nil, err
	}
	snapshot.Control.Migration = true
	encoded, err := encodeSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	archive, err := os.CreateTemp(dir, "index.migration-*.json")
	if err != nil {
		return nil, err
	}
	// Keep archives even on failure, for diagnosis/manual rollback.
	err = writeMigrationFile(archive, data, flush)
	if err != nil {
		return nil, err
	}
	if err = migrationSyncDir(dir, syncDirectory); err != nil {
		return nil, err
	}
	if err = migrationReplace(snapshotPath, encoded, flush, syncDirectory); err != nil {
		return nil, err
	}
	return finishMigration(logPath, snapshot, flush, syncDirectory)
}

func legacySnapshot(data []byte, now time.Time) (storeSnapshot, error) {
	var p persisted
	s := storeSnapshot{Schema: storeSchema}
	if err := json.Unmarshal(data, &p); err != nil {
		return s, err
	}
	if p.Schema != 0 && p.Schema != 2 {
		return s, fmt.Errorf("unsupported legacy index schema")
	}
	s.Generation, s.Clock = p.Generation, p.Clock
	s.Control = storeControl{Binding: p.Binding, Blocked: p.Blocked, ConfirmedGeneration: p.ConfirmedGeneration}
	for _, f := range p.Files {
		if f == nil {
			return s, codecError()
		}
		v := storeFile{Name: f.Name, Disk: f.Disk, Hash: f.Hash, Size: f.Size, ModTime: f.ModTime, Exec: f.Exec, Deleted: f.Deleted}
		for peer, n := range f.Version {
			v.Version = append(v.Version, storePair{peer, n})
		}
		sort.Slice(v.Version, func(i, j int) bool { return v.Version[i].Peer < v.Version[j].Peer })
		if f.Deleted {
			v.DeletedAt = now.UnixNano()
		}
		s.Files = append(s.Files, v)
	}
	sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Name < s.Files[j].Name })
	if len(s.Files) > 0 && s.Control.Binding == (rootBinding{}) && s.Control.Blocked == nil {
		var token [24]byte
		if _, err := rand.Read(token[:]); err != nil {
			return s, err
		}
		s.Control.Blocked = &RootError{Code: "root_changed", Message: "legacy index requires explicit rebind or reset", Token: hex.EncodeToString(token[:])}
	}
	return s, s.validate()
}

func finishMigration(path string, snapshot storeSnapshot, flush, syncDirectory func(*os.File) error) (*indexStore, error) {
	if _, err := os.Lstat(path); os.IsNotExist(err) && snapshot.Control.Migration {
		if err = migrationReplace(path, encodeLogHeader(storeLogHeader{StoreID: snapshot.StoreID, BaseGeneration: snapshot.Generation}), flush, syncDirectory); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	s, err := openIndexStore(path, snapshot)
	if err != nil {
		return nil, err
	}
	if s.state.Control.Migration {
		c := s.state.Control
		c.Migration = false
		frame := storeFrame{Generation: s.state.Generation + 1, PreviousGeneration: s.state.Generation, Kind: "Control", Clock: s.state.Clock, Control: &c}
		if _, err = rand.Read(frame.TxID[:]); err == nil {
			s.flush = flush
			err = s.append(frame)
		}
		if err != nil {
			s.close()
			return nil, err
		}
	}
	return s, nil
}

func migrationSyncDir(path string, flush func(*os.File) error) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return flush(d)
}
func writeMigrationFile(f *os.File, data []byte, flush func(*os.File) error) error {
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = flush(f)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
func migrationReplace(path string, data []byte, flush, syncDirectory func(*os.File) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = writeMigrationFile(f, data, flush); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return migrationSyncDir(filepath.Dir(path), syncDirectory)
}
