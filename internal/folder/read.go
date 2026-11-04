package folder

import (
	"errors"
	"io"
	"os"
)

var ErrVersionUnavailable = errors.New("file version unavailable")

// CheckRead rejects requests from a previous confirmed root, even after unblock.
func (f *Folder) CheckRead(generation uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocked != nil {
		c := *f.blocked
		return &c
	}
	if f.confirmedGeneration != generation {
		return ErrVersionUnavailable
	}
	return nil
}

// OpenVersion validates bytes through a no-follow descriptor. The caller owns
// the returned file and must check its request generation while transmitting.
func (f *Folder) OpenVersion(name, hash string) (FileInfo, *os.File, error) {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	if err := f.guardRoot(); err != nil {
		return FileInfo{}, nil, err
	}
	if !validName(name) {
		return FileInfo{}, nil, ErrVersionUnavailable
	}
	f.mu.Lock()
	entry := f.files[name]
	var fi FileInfo
	if entry != nil {
		fi = *entry
		fi.Version = entry.Version.Copy()
	}
	f.mu.Unlock()
	if entry == nil || fi.Deleted || fi.Hash != hash || !validDisk(fi.Name, fi.Disk) {
		return FileInfo{}, nil, ErrVersionUnavailable
	}
	disk := fi.Disk
	if disk == "" {
		disk = fi.Name
	}
	file, err := f.root.openRead(disk)
	if err != nil {
		return FileInfo{}, nil, err
	}
	actual, st, _, err := stableHash(file)
	if err == nil && (actual != hash || st.Size() != fi.Size || (st.Mode().Perm()&0100 != 0) != fi.Exec) {
		err = ErrVersionUnavailable
	}
	if err == nil {
		err = f.guardRoot()
	}
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		file.Close()
		return FileInfo{}, nil, err
	}
	fi.Disk = ""
	return fi, file, nil
}
