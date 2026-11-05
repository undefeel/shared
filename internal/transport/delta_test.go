package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sharedir/internal/folder"
)

func deltaHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func signatures(b []byte) []string {
	var h []string
	for len(b) > 0 {
		n := folder.DeltaBlockSize
		if n > len(b) {
			n = len(b)
		}
		h = append(h, deltaHash(b[:n]))
		b = b[n:]
	}
	return h
}
func testDeltaCall(t *testing.T, f *folder.Folder, req folder.DeltaRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/delta", bytes.NewReader(b))
	r.Header.Set(ProtocolHeader, "2")
	w := httptest.NewRecorder()
	(&Server{Folder: f}).delta(&batchWriter{ResponseRecorder: w}, r)
	return w
}
func readDeltaTestHeader(t *testing.T, r *bytes.Reader) deltaHeader {
	t.Helper()
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatal(err)
	}
	var h deltaHeader
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	return h
}
func TestDeltaServer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		base, target []byte
		status       string
		blocks       int
	}{
		{"change", bytes.Repeat([]byte{1}, 4*folder.DeltaBlockSize), append(bytes.Repeat([]byte{1}, 3*folder.DeltaBlockSize), bytes.Repeat([]byte{2}, folder.DeltaBlockSize)...), "ok", 1},
		{"shrink", bytes.Repeat([]byte{1}, 3*folder.DeltaBlockSize), bytes.Repeat([]byte{1}, 2*folder.DeltaBlockSize), "ok", 0},
		{"grow", bytes.Repeat([]byte{1}, 2*folder.DeltaBlockSize), bytes.Repeat([]byte{1}, 3*folder.DeltaBlockSize), "ok", 1},
		{"seventy", bytes.Repeat([]byte{1}, 10*folder.DeltaBlockSize), append(bytes.Repeat([]byte{1}, 3*folder.DeltaBlockSize), bytes.Repeat([]byte{2}, 7*folder.DeltaBlockSize)...), "full_required", 0},
		{"above", bytes.Repeat([]byte{1}, 4*folder.DeltaBlockSize), bytes.Repeat([]byte{2}, 4*folder.DeltaBlockSize), "full_required", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := fileFixture(t)
			if err := os.WriteFile(filepath.Join(f.Root, "a"), tc.target, 0600); err != nil {
				t.Fatal(err)
			}
			if err := f.Scan(); err != nil {
				t.Fatal(err)
			}
			fi, _, _ := f.Lookup("a")
			req := folder.DeltaRequest{Protocol: 2, Name: "a", TargetHash: fi.Hash, TargetSize: int64(len(tc.target)), BaseHash: deltaHash(tc.base), BaseSize: int64(len(tc.base)), BlockSize: folder.DeltaBlockSize, Blocks: signatures(tc.base)}
			w := testDeltaCall(t, f, req)
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			r := bytes.NewReader(w.Body.Bytes())
			magic := make([]byte, 8)
			io.ReadFull(r, magic)
			if string(magic) != deltaMagic {
				t.Fatal("magic")
			}
			h := readDeltaTestHeader(t, r)
			if h.Status != tc.status {
				t.Fatalf("status %s", h.Status)
			}
			if h.Status == "ok" {
				for i := 0; i < tc.blocks; i++ {
					b := readDeltaTestHeader(t, r)
					body := make([]byte, b.WireSize)
					io.ReadFull(r, body)
					var plain bytes.Buffer
					n, hash, err := DecodeBody(&plain, bytes.NewReader(body), b.Encoding, b.Length)
					if err != nil || n != b.Length || hex.EncodeToString(hash[:]) != b.Hash {
						t.Fatalf("block: %v", err)
					}
				}
				end := readDeltaTestHeader(t, r)
				if !end.End || end.Frames != tc.blocks || r.Len() != 0 {
					t.Fatal("bad end", end)
				}
			}
			req.TargetSize++
			w = testDeltaCall(t, f, req)
			r = bytes.NewReader(w.Body.Bytes())
			io.ReadFull(r, magic)
			if h = readDeltaTestHeader(t, r); h.Status != "version_changed" {
				t.Fatal(h)
			}
			req.BlockSize = 1
			if w = testDeltaCall(t, f, req); w.Code != 400 {
				t.Fatal(w.Code)
			}
			req.BlockSize = folder.DeltaBlockSize
			req.Blocks = []string{strings.Repeat("z", 64)}
			if w = testDeltaCall(t, f, req); w.Code != 400 {
				t.Fatal(w.Code)
			}
		})
	}
}
