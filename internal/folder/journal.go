package folder

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
)

type captureIdentity struct{ Device, Inode uint64 }

type journalIntent struct {
	TxID, Target, Tmp, Capture string
	Generation                 uint64
	Binding                    rootBinding
	Before, After              *FileInfo
	Identity                   captureIdentity
}

func (f *Folder) intentPath() string { return filepath.Join(filepath.Dir(f.statePath), "intent.json") }
func (j journalIntent) validate() error {
	if j.TxID == "" || j.Generation == 0 || !diskName(j.Target) {
		return fmt.Errorf("invalid intent")
	}
	for _, fi := range []*FileInfo{j.Before, j.After} {
		if fi != nil && (!validDisk(fi.Name, fi.Disk) || fi.diskPath() != j.Target) {
			return fmt.Errorf("invalid intent record")
		}
	}
	if j.Before == nil && j.After == nil {
		return fmt.Errorf("empty intent")
	}
	for _, n := range []string{j.Tmp, j.Capture} {
		if n != "" && (!diskName(n) || path.Dir(n) != path.Dir(j.Target) || n == j.Target) {
			return fmt.Errorf("invalid intent path")
		}
	}
	if j.Tmp != "" && (!strings.HasPrefix(path.Base(j.Tmp), tmpPrefix) || j.Tmp == j.Capture) {
		return fmt.Errorf("invalid tmp")
	}
	if j.Capture != "" && (j.Identity.Inode == 0 || len(path.Base(j.Capture)) <= len(tmpPrefix+"capture-") || path.Base(j.Capture)[:len(tmpPrefix+"capture-")] != tmpPrefix+"capture-") {
		return fmt.Errorf("invalid capture")
	}
	return nil
}

func (f *Folder) writeIntent(j journalIntent) error {
	if err := j.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.statePath), ".sharedir-intent-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	flush := f.persistHooks.syncFile
	if flush == nil {
		flush = syncFile
	}
	if err = flush(tmp); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), f.intentPath()); err != nil {
		return err
	}
	return f.syncIntentDir()
}
func (f *Folder) syncIntentDir() error {
	d, err := os.Open(filepath.Dir(f.statePath))
	if err != nil {
		return err
	}
	defer d.Close()
	flush := f.persistHooks.syncDir
	if flush == nil {
		flush = syncDir
	}
	return flush(d)
}

// recoverIntent requires scanMu, before Scan/network service; Open integration belongs to the commit coordinator.
func (f *Folder) recoverIntent() error {
	data, err := os.ReadFile(f.intentPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var j journalIntent
	if err = json.Unmarshal(data, &j); err != nil {
		return err
	}
	if err = j.validate(); err != nil {
		return err
	}
	if err = f.guardRoot(); err != nil {
		return err
	}
	if j.Binding != f.binding {
		return f.blockRoot("recovery_ambiguous", "intent binding differs")
	}
	data, err = os.ReadFile(f.statePath)
	if err != nil {
		return err
	}
	var p persisted
	if err = json.Unmarshal(data, &p); err != nil {
		return err
	}
	var current *FileInfo
	for _, fi := range p.Files {
		if fi.Name == recordName(j) {
			current = fi
		}
	}
	committed := p.Generation == j.Generation && reflect.DeepEqual(current, j.After)
	if !committed && (p.Generation >= j.Generation || !reflect.DeepEqual(current, j.Before)) {
		return f.blockRoot("recovery_ambiguous", "index differs from intent")
	}
	if committed && j.After != nil && !j.After.Deleted {
		file, e := f.root.openRead(j.Target)
		if e != nil {
			return e
		}
		hash, _, _, he := stableHash(file)
		file.Close()
		if he != nil {
			return he
		}
		if hash != j.After.Hash {
			return f.blockRoot("recovery_ambiguous", "committed target hash differs")
		}
	}
	if j.Capture != "" {
		c, e := f.root.openCapture(j.Target, j.Capture, j.Identity)
		if e != nil {
			return e
		}
		defer c.Close()
		file, e := f.root.openRead(j.Capture)
		if e == nil {
			var st unix.Stat_t
			e = unix.Fstat(int(file.Fd()), &st)
			file.Close()
			if e != nil {
				return e
			}
			if committed && j.Identity == (captureIdentity{uint64(st.Dev), uint64(st.Ino)}) {
				e = c.discard()
			} else if committed {
				_, e = c.promote("recovery", "")
			} else {
				_, e = c.restore("recovery", "")
			}
			if e != nil {
				return e
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		} else if !committed {
			file, e = f.root.openRead(j.Target)
			if e != nil {
				if !errors.Is(e, os.ErrNotExist) {
					return e
				}
				return f.blockRoot("recovery_ambiguous", "capture and target unavailable")
			}
			hash, _, _, he := stableHash(file)
			file.Close()
			if he != nil {
				return he
			}
			if (j.Before == nil || hash != j.Before.Hash) && (j.After == nil || hash != j.After.Hash) {
				return f.blockRoot("recovery_ambiguous", "target hash differs")
			}
		}
		if err = syncDir(c.parent); err != nil {
			return err
		}
	}
	if err = os.Remove(f.intentPath()); err != nil {
		return err
	}
	return f.syncIntentDir()
}
func recordName(j journalIntent) string {
	if j.After != nil {
		return j.After.Name
	}
	return j.Before.Name
}
