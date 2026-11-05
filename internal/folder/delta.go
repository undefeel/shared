package folder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

const (
	DeltaMinSize   = 8 << 20
	DeltaBlockSize = 1 << 20
	DeltaMaxBlocks = 65536
)

// DeltaRequest identifies the target and the receiver's base block signatures.
type DeltaRequest struct {
	Protocol   int      `json:"protocol"`
	Name       string   `json:"name"`
	TargetHash string   `json:"targetHash"`
	TargetSize int64    `json:"targetSize"`
	BaseHash   string   `json:"baseHash"`
	BaseSize   int64    `json:"baseSize"`
	BlockSize  int64    `json:"blockSize"`
	Blocks     []string `json:"blocks"`
}

// DeltaBlock is a verified target block at an absolute file offset.
type DeltaBlock struct {
	Offset int64
	Data   []byte
}

type DeltaFetcher func(context.Context, DeltaRequest, func(DeltaBlock) error) error

type deltaFetcherKey struct{}

// WithDeltaFetcher makes an optional delta transport available during apply.
func WithDeltaFetcher(ctx context.Context, fetch DeltaFetcher) context.Context {
	return context.WithValue(ctx, deltaFetcherKey{}, fetch)
}

// downloadDelta builds only in private staging and falls back on any untrusted
// transport or changing-base result.
func (f *Folder) downloadDelta(ctx context.Context, fi FileInfo) (name string, ok bool, err error) {
	fetch, _ := ctx.Value(deltaFetcherKey{}).(DeltaFetcher)
	if fetch == nil || fi.Size < DeltaMinSize || fi.Size > int64(DeltaMaxBlocks)*DeltaBlockSize || !validSHA256(fi.Hash) {
		return "", false, nil
	}
	var base FileInfo
	f.mu.Lock()
	if cur := f.files[fi.Name]; cur != nil && !cur.Deleted {
		base = *cur
	}
	f.mu.Unlock()
	if base.Name == "" || base.Size < 0 || base.Size > int64(DeltaMaxBlocks)*DeltaBlockSize || !validDisk(base.Name, base.diskPath()) {
		return "", false, nil
	}
	src, e := f.root.openRead(base.diskPath())
	if e != nil {
		return "", false, nil
	}
	defer func() {
		if src != nil {
			_ = src.Close()
		}
	}()
	hash, st, _, e := stableHash(src)
	if e != nil || hash != base.Hash || st.Size() != base.Size {
		return "", false, nil
	}
	blocks := make([]string, 0, (base.Size+DeltaBlockSize-1)/DeltaBlockSize)
	buf := make([]byte, DeltaBlockSize)
	for off := int64(0); off < base.Size; off += DeltaBlockSize {
		n := base.Size - off
		if n > DeltaBlockSize {
			n = DeltaBlockSize
		}
		if _, e = src.ReadAt(buf[:n], off); e != nil {
			return "", false, nil
		}
		h := sha256.Sum256(buf[:n])
		blocks = append(blocks, hex.EncodeToString(h[:]))
	}
	if _, e = src.Seek(0, 0); e != nil {
		return "", false, nil
	}
	again, now, _, e := stableHash(src)
	if e != nil || again != hash || !sameFileInfo(st, now) {
		return "", false, nil
	}
	name = tmpPrefix + captureToken()
	dst, e := f.root.cloneFile(src, name, base.Size)
	if e != nil {
		if errors.Is(e, errCloneChanged) {
			return "", false, nil
		}
		return "", false, e
	}
	keep := false
	defer func() {
		if !keep {
			if e := f.root.unlink(name, false); e != nil && !os.IsNotExist(e) {
				err = e
			}
			name = ""
		}
	}()
	req := DeltaRequest{Protocol: 2, Name: fi.Name, TargetHash: fi.Hash, TargetSize: fi.Size, BaseHash: hash, BaseSize: base.Size, BlockSize: DeltaBlockSize, Blocks: blocks}
	e = fetch(ctx, req, func(b DeltaBlock) error {
		if b.Offset < 0 || b.Offset > fi.Size || int64(len(b.Data)) > fi.Size-b.Offset {
			return errors.New("invalid delta block")
		}
		n, e := dst.WriteAt(b.Data, b.Offset)
		if e == nil && n != len(b.Data) {
			e = io.ErrShortWrite
		}
		return e
	})
	if e == nil {
		e = dst.Truncate(fi.Size)
	}
	if e == nil {
		_, e = dst.Seek(0, 0)
	}
	if e == nil {
		var got string
		var info os.FileInfo
		got, info, _, e = stableHash(dst)
		if e == nil && (got != fi.Hash || info.Size() != fi.Size) {
			e = errCloneChanged
		}
	}
	if e == nil {
		_, e = src.Seek(0, 0)
	}
	if e == nil {
		var got string
		var info os.FileInfo
		got, info, _, e = stableHash(src)
		if e == nil && (got != hash || !sameFileInfo(st, info)) {
			e = errCloneChanged
		}
	}
	if closeErr := dst.Close(); e == nil {
		e = closeErr
	}
	if closeErr := src.Close(); e == nil {
		e = closeErr
	}
	src = nil
	if e != nil {
		if ctx.Err() != nil {
			return name, false, ctx.Err()
		}
		return name, false, nil
	}
	if ctx.Err() != nil {
		return name, false, ctx.Err()
	}
	keep = true
	return name, true, nil
}
