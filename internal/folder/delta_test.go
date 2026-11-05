package folder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDeltaStaging(t *testing.T) {
	for _, mode := range []string{"valid", "grow", "shrink", "mutated", "corrupt", "missing", "absent", "small"} {
		t.Run(mode, func(t *testing.T) {
			n := newNode(t, "A")
			defer n.f.Close()
			base := bytes.Repeat([]byte{1}, 8<<20)
			target := bytes.Clone(base)
			copy(target[DeltaBlockSize:2*DeltaBlockSize], bytes.Repeat([]byte{2}, DeltaBlockSize))
			if mode == "grow" {
				target = append(target, bytes.Repeat([]byte{3}, DeltaBlockSize)...)
			}
			if mode == "shrink" {
				base = append(base, bytes.Repeat([]byte{3}, DeltaBlockSize)...)
			}
			if mode == "small" {
				target = []byte("small")
			}
			path := filepath.Join(n.f.Root, "x")
			if e := os.WriteFile(path, base, 0600); e != nil {
				t.Fatal(e)
			}
			if e := n.f.Scan(); e != nil {
				t.Fatal(e)
			}
			old, _ := os.Stat(path)
			h := sha256.Sum256(target)
			fi := FileInfo{Name: "x", Hash: hex.EncodeToString(h[:]), Size: int64(len(target))}
			calls, deltaCalls := 0, 0
			fetch := func(_ context.Context, _ FileInfo, w io.Writer) error { calls++; _, e := w.Write(target); return e }
			ctx := context.Background()
			if mode != "absent" {
				ctx = WithDeltaFetcher(ctx, func(_ context.Context, req DeltaRequest, emit func(DeltaBlock) error) error {
					deltaCalls++
					if req.BaseSize != int64(len(base)) || len(req.Blocks) != int((int64(len(base))+DeltaBlockSize-1)/DeltaBlockSize) {
						return errors.New("bad request")
					}
					if mode == "mutated" {
						if e := os.WriteFile(path, bytes.Repeat([]byte{4}, len(base)), 0600); e != nil {
							return e
						}
					}
					if mode == "missing" {
						return nil
					}
					if mode == "corrupt" {
						return emit(DeltaBlock{Offset: DeltaBlockSize, Data: bytes.Repeat([]byte{9}, DeltaBlockSize)})
					}
					if e := emit(DeltaBlock{Offset: DeltaBlockSize, Data: target[DeltaBlockSize : 2*DeltaBlockSize]}); e != nil {
						return e
					}
					if mode == "grow" {
						return emit(DeltaBlock{Offset: 8 << 20, Data: target[8<<20:]})
					}
					return nil
				})
			}
			tmp, e := n.f.downloadOrReuse(ctx, fi, fetch)
			if e != nil {
				t.Fatal(e)
			}
			defer n.f.root.unlink(tmp, false)
			b, e := os.ReadFile(filepath.Join(n.f.Root, tmp))
			if e != nil || !bytes.Equal(b, target) {
				t.Fatal("wrong staging bytes", e)
			}
			st, e := os.Stat(filepath.Join(n.f.Root, tmp))
			if e != nil || os.SameFile(st, old) {
				t.Fatal("staging shares live inode")
			}
			fallback := mode == "mutated" || mode == "corrupt" || mode == "missing" || mode == "absent" || mode == "small"
			if fallback != (calls == 1) {
				t.Fatalf("fallback calls=%d", calls)
			}
			wantDelta := mode != "absent" && mode != "small"
			if wantDelta != (deltaCalls == 1) {
				t.Fatalf("delta calls=%d", deltaCalls)
			}
		})
	}
}
