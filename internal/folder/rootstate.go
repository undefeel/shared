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

// ErrStaleToken means the displayed root error is no longer current.
var ErrStaleToken = errors.New("stale folder confirmation token")

// SetConfiguredRoot serializes path changes with all tree commits.
func (f *Folder) SetConfiguredRoot(configuredPath string) error {
	p, err := filepath.Abs(configuredPath)
	if err != nil {
		return err
	}
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	if p == f.Root {
		return nil
	}
	f.Root = p
	f.mu.Lock()
	f.blocked = nil
	f.mu.Unlock()
	return f.blockRoot("root_changed", "configured path changed")
}

// Confirm requires callers to cancel and drain old runners and watchers first.
// Work on a private candidate so readers continue seeing the latch until durable success.
func (f *Folder) Confirm(token, action, configuredPath string) error {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	blocked := f.Blocked()
	if blocked == nil || token != blocked.Token {
		return ErrStaleToken
	}
	if action != "reset" && action != "rebind" {
		return errors.New("action must be reset or rebind")
	}
	p, err := filepath.Abs(configuredPath)
	if err != nil {
		return err
	}
	r, err := openRootFS(p)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	binding, err := bindingOf(r, p)
	if err != nil {
		return err
	}
	f.mu.Lock()
	c := &Folder{Root: p, root: r, binding: binding, statePath: f.statePath, me: f.me, log: f.log,
		confirmedGeneration: f.confirmedGeneration + 1, files: map[string]*FileInfo{}, clock: f.clock,
		generation: f.generation, durableGeneration: f.durableGeneration, persistHooks: f.persistHooks,
		changed: make(chan struct{})}
	for k, v := range f.files {
		copy := *v
		copy.Version = v.Version.Copy()
		c.files[k] = &copy
	}
	f.mu.Unlock()
	if action == "reset" {
		if err = os.Link(f.statePath, f.statePath+".reset-"+token); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		dir, e := os.Open(filepath.Dir(f.statePath))
		if e != nil {
			return e
		}
		err = syncDir(dir)
		dir.Close()
		if err != nil {
			return err
		}
		c.files = map[string]*FileInfo{}
		err = c.save()
	} else {
		err = c.scanLocked(c.rootScanIO(), true)
		if err == nil {
			err = c.save()
		}
	}
	if err != nil {
		_ = f.save()
		return err
	}
	old := f.root
	f.mu.Lock()
	f.root, f.Root, f.binding, f.blocked = r, p, binding, nil
	f.files, f.clock, f.confirmedGeneration = c.files, c.clock, c.confirmedGeneration
	f.ignore, f.hashCache, f.lastScan = c.ignore, c.hashCache, c.lastScan
	f.generation, f.durableGeneration = c.generation, c.durableGeneration
	f.notify()
	f.mu.Unlock()
	ok = true
	if old != nil {
		_ = old.Close()
	}
	return nil
}
