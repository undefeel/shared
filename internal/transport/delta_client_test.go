package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"testing"

	"sharedir/internal/folder"
)

func TestFetchDeltaValidation(t *testing.T) {
	body := bytes.Repeat([]byte{7}, folder.DeltaBlockSize)
	target := append(bytes.Clone(body), []byte("end")...)
	req := folder.DeltaRequest{TargetHash: deltaHash(target), TargetSize: int64(len(target))}
	for _, mode := range []string{"identity", "gzip", "lost_end", "bad_gzip", "bad_hash", "overflow", "duplicate", "bad_end_hash", "bad_end_count", "full_required"} {
		t.Run(mode, func(t *testing.T) {
			c, addr, closeServer := clientForFiles(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-sharedir-delta;version=2")
				io.WriteString(w, deltaMagic)
				status := "ok"
				if mode == "full_required" {
					status = "full_required"
				}
				writeDeltaHeader(w, deltaHeader{Status: status, TargetHash: req.TargetHash, TargetSize: req.TargetSize})
				if status != "ok" {
					return
				}
				wire := body
				encoding := "identity"
				if mode == "gzip" || mode == "bad_gzip" {
					var b bytes.Buffer
					z := gzip.NewWriter(&b)
					z.Write(body)
					z.Close()
					wire = b.Bytes()
					encoding = "gzip"
					if mode == "bad_gzip" {
						wire[len(wire)-1] ^= 1
					}
				}
				h := deltaHeader{Length: int64(len(body)), WireSize: int64(len(wire)), Hash: deltaHash(body), Encoding: encoding}
				if mode == "bad_hash" {
					h.Hash = deltaHash([]byte("bad"))
				}
				if mode == "overflow" {
					h.Offset = 1 << 62
				}
				writeDeltaHeader(w, h)
				w.Write(wire)
				if mode == "duplicate" {
					writeDeltaHeader(w, h)
					w.Write(wire)
				}
				if mode == "lost_end" {
					return
				}
				end := deltaHeader{End: true, TargetHash: req.TargetHash, TargetSize: req.TargetSize, Frames: 1}
				if mode == "bad_end_hash" {
					end.TargetHash = deltaHash(nil)
				}
				if mode == "bad_end_count" {
					end.Frames = 2
				}
				writeDeltaHeader(w, end)
			})
			defer closeServer()
			calls := 0
			err := c.FetchDelta(context.Background(), addr, req, func(b folder.DeltaBlock) error {
				calls++
				if b.Offset != 0 || !bytes.Equal(b.Data, body) {
					t.Error("unexpected block")
				}
				return nil
			})
			success := mode == "identity" || mode == "gzip"
			requireEqualf(t, success, (err == nil), "err=%v calls=%d", err, calls)
			if success && calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
