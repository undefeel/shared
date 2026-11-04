package folder

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type rootBinding struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// RootError is a persistent latch. Only explicit confirmation may clear it.
type RootError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Token   string `json:"token"`
}

func (e *RootError) Error() string { return e.Code + ": " + e.Message }

func (f *Folder) Blocked() *RootError {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocked == nil {
		return nil
	}
	c := *f.blocked
	return &c
}

func bindingOf(r *rootFS, path string) (rootBinding, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(r.root.Fd()), &st); err != nil {
		return rootBinding{}, err
	}
	return rootBinding{path, uint64(st.Dev), uint64(st.Ino)}, nil
}

// blockRoot requires scanMu. A failed save keeps the in-memory latch closed.
func (f *Folder) blockRoot(code, message string) error {
	if e := f.Blocked(); e != nil {
		return e
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	e := &RootError{code, message, hex.EncodeToString(token[:])}
	f.mu.Lock()
	f.blocked = e
	f.mu.Unlock()
	f.Kick()
	if err := f.save(); err != nil {
		return err
	}
	return e
}

// guardRoot requires scanMu; callers recheck after download/before publication.
// Comparing both the configured pathname and the held fd detects replacement.
func (f *Folder) guardRoot() error {
	if e := f.Blocked(); e != nil {
		return e
	}
	f.mu.Lock()
	binding := f.binding
	f.mu.Unlock()
	path, err := filepath.Abs(f.Root)
	if err != nil {
		return f.blockRoot("root_unavailable", err.Error())
	}
	if path != binding.Path {
		return f.blockRoot("root_changed", "configured path changed")
	}
	current, err := openRootFS(path)
	if err != nil {
		return f.blockRoot("root_unavailable", err.Error())
	}
	defer current.Close()
	now, err := bindingOf(current, path)
	if err != nil {
		return f.blockRoot("root_unavailable", err.Error())
	}
	held, err := bindingOf(f.root, path)
	if err != nil {
		return f.blockRoot("root_unavailable", err.Error())
	}
	if now != binding || held != binding {
		return f.blockRoot("root_changed", "root identity changed")
	}
	return nil
}

func (f *Folder) initRoot(data []byte) error {
	var p persisted
	if len(data) != 0 {
		if err := json.Unmarshal(data, &p); err != nil {
			return err
		}
	}
	legacy := len(data) != 0 && p.Schema == 0
	// Link atomically archives the validated legacy bytes without replacing an archive.
	if legacy {
		archive := f.statePath + ".legacy"
		if err := os.Link(f.statePath, archive); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		dir, err := os.Open(filepath.Dir(f.statePath))
		if err != nil {
			return err
		}
		err = syncDir(dir)
		dir.Close()
		if err != nil {
			return err
		}
	}
	if len(f.files) == 0 && f.binding.Path == "" && f.blocked == nil {
		if err := os.MkdirAll(f.Root, 0o755); err != nil {
			return err
		}
	}
	r, err := openRootFS(f.Root)
	if err != nil {
		returnLatch := f.blockRoot("root_unavailable", err.Error())
		var latch *RootError
		if errors.As(returnLatch, &latch) {
			return nil
		}
		return returnLatch
	}
	f.root = r
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	if f.binding.Path == "" {
		if len(f.files) != 0 {
			err = f.blockRoot("root_changed", "legacy index requires explicit rebind or reset")
		} else {
			f.binding, err = bindingOf(r, f.Root)
			if err == nil {
				err = f.save()
			}
		}
	} else {
		err = f.guardRoot()
	}
	var latch *RootError
	if err != nil && !errors.As(err, &latch) {
		return err
	}
	ok = true
	return nil
}

// Close requires callers to drain watcher and peer operations first.
func (f *Folder) Close() error {
	if f.root != nil {
		return f.root.Close()
	}
	return nil
}

// ConfirmedGeneration tags downloads; callers compare it again under scanMu.
func (f *Folder) ConfirmedGeneration() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.confirmedGeneration
}
