package folder

import (
	"context"
	"errors"
	"fmt"
	"os"
)

func (f *Folder) reuseCandidates(fi FileInfo) []FileInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FileInfo
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if cur := f.files[name]; cur != nil && !cur.Deleted && cur.Hash == fi.Hash && cur.Size == fi.Size {
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
		dst, err := f.root.create(name)
		if err != nil {
			src.Close()
			return "", err
		}
		err = cloneInto(src, dst, fi.Size)
		if err == nil {
			after, now, _, e := stableHash(src)
			if e != nil || after != before || !sameFileInfo(st, now) {
				err = errors.New("reuse source changed")
			}
		}
		if err == nil {
			got, info, _, e := stableHash(dst)
			if e != nil || got != fi.Hash || info.Size() != fi.Size {
				err = fmt.Errorf("reused content verification failed")
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
		if errors.Is(err, os.ErrInvalid) {
			return "", err
		}
	}
	return f.download(ctx, fi, fetch)
}

func sameFileInfo(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime() == b.ModTime() && os.SameFile(a, b)
}
