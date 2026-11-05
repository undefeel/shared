package folder

import (
	"context"
	"errors"
	"os"
)

func (f *Folder) reuseCandidates(fi FileInfo) []FileInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.contentNames == nil {
		f.rebuildContentNames()
	}
	var out []FileInfo
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if cur := f.files[name]; cur != nil && !cur.Deleted && (name == fi.Name || (cur.Hash == fi.Hash && cur.Size == fi.Size)) {
			c := *cur
			c.Version = cur.Version.Copy()
			out = append(out, c)
		}
	}
	add(fi.Name)
	for name := range f.contentNames[fi.Hash] {
		add(name)
	}
	return out
}

func (f *Folder) hasReusableContent(fi FileInfo) bool {
	for _, candidate := range f.reuseCandidates(fi) {
		if !validDisk(candidate.Name, candidate.diskPath()) {
			continue
		}
		file, err := f.root.openRead(candidate.diskPath())
		if err != nil {
			continue
		}
		h, info, _, e := stableHash(file)
		file.Close()
		if e == nil && h == fi.Hash && info.Size() == fi.Size {
			return true
		}
	}
	return false
}

func (f *Folder) downloadOrReuse(ctx context.Context, fi FileInfo, fetch Fetcher) (string, error) {
	return f.downloadOrReuseWith(ctx, fi, fetch, f.root.cloneFile)
}

func (f *Folder) downloadOrReuseWith(ctx context.Context, fi FileInfo, fetch Fetcher, clone func(*os.File, string, int64) (*os.File, error)) (string, error) {
	for _, candidate := range f.reuseCandidates(fi) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !validDisk(candidate.Name, candidate.diskPath()) {
			continue
		}
		src, err := f.root.openRead(candidate.diskPath())
		if err != nil {
			continue
		}
		before, st, _, err := stableHash(src)
		if err != nil || before != fi.Hash || st.Size() != fi.Size {
			src.Close()
			continue
		}
		name := tmpPrefix + captureToken()
		dst, err := clone(src, name, fi.Size)
		if err != nil {
			now, e := src.Stat()
			src.Close()
			if errors.Is(err, errCloneChanged) || (e == nil && scanStatFromInfo(st) != scanStatFromInfo(now)) {
				continue
			}
			return "", err
		}
		_, err = src.Seek(0, 0)
		if err == nil {
			after, now, _, e := stableHash(src)
			if e != nil || after != before || !sameFileInfo(st, now) {
				err = errCloneChanged
			}
		}
		if err == nil {
			_, err = dst.Seek(0, 0)
			if err == nil {
				got, info, _, e := stableHash(dst)
				if e != nil {
					err = e
				} else if got != fi.Hash || info.Size() != fi.Size {
					err = errCloneChanged
				}
			}
		}

		src.Close()
		ce := dst.Close()
		if err == nil {
			err = ce
		}
		if err == nil {
			return name, nil
		}
		_ = f.root.unlink(name, false)
		if !errors.Is(err, errCloneChanged) {
			return "", err
		}
	}
	if name, ok, err := f.downloadDelta(ctx, fi); err != nil {
		return "", err
	} else if ok {
		return name, nil
	}
	return f.download(ctx, fi, fetch)
}

func sameFileInfo(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime() == b.ModTime() && os.SameFile(a, b)
}
