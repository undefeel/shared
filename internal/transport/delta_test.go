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
	requireNoError(t, binary.Read(r, binary.BigEndian, &n))
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatal(err)
	}
	var h deltaHeader
	requireNoError(t, json.Unmarshal(b, &h))
	return h
}
func TestDeltaServer(t *testing.T) {
	cases := []struct {
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
	}
	f, _ := fileFixture(t)
	for _, tc := range cases {
		writeTestFile(t, filepath.Join(f.Root, tc.name), tc.target, 0600)
	}
	target := bytes.Repeat([]byte{1}, 4*folder.DeltaBlockSize)
	writeTestFile(t, filepath.Join(f.Root, "malformed"), target, 0600)
	mustScan(t, f)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fi, _, _ := f.Lookup(tc.name)
			req := folder.DeltaRequest{Protocol: 2, Name: tc.name, TargetHash: fi.Hash, TargetSize: int64(len(tc.target)), BaseHash: deltaHash(tc.base), BaseSize: int64(len(tc.base)), BlockSize: folder.DeltaBlockSize, Blocks: signatures(tc.base)}
			w := testDeltaCall(t, f, req)
			requireEqualf(t, w.Code, 200, "status %d: %s", w.Code, w.Body.String())
			r := bytes.NewReader(w.Body.Bytes())
			magic := make([]byte, 8)
			io.ReadFull(r, magic)
			requireEqual(t, string(magic), deltaMagic, "magic")
			h := readDeltaTestHeader(t, r)
			requireEqualf(t, h.Status, tc.status, "status %s", h.Status)
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
		})
	}
	t.Run("RejectsMalformedAndMutation", func(t *testing.T) { testDeltaServerRejectsMalformedAndMutation(t, f, target) })
}

type deltaMutatingWriter struct {
	*batchWriter
	mutate func()
}

func (w *deltaMutatingWriter) Write(p []byte) (int, error) {
	n, e := w.batchWriter.Write(p)
	if string(p) == deltaMagic && w.mutate != nil {
		f := w.mutate
		w.mutate = nil
		f()
	}
	return n, e
}
func testDeltaServerRejectsMalformedAndMutation(t *testing.T, f *folder.Folder, target []byte) {
	fi, _, _ := f.Lookup("malformed")
	base := bytes.Repeat([]byte{1}, len(target))
	base[0] = 2
	req := folder.DeltaRequest{Protocol: 2, Name: "malformed", TargetHash: fi.Hash, TargetSize: fi.Size, BaseHash: deltaHash(base), BaseSize: int64(len(base)), BlockSize: folder.DeltaBlockSize, Blocks: signatures(base)}
	badSize, badHash, badCount := req, req, req
	badSize.BlockSize = 1
	badHash.Blocks = []string{strings.Repeat("z", 64)}
	badCount.Blocks = badCount.Blocks[:3]
	for _, tc := range []struct {
		name string
		req  folder.DeltaRequest
	}{{"block size", badSize}, {"block hash", badHash}, {"count", badCount}} {
		w := testDeltaCall(t, f, tc.req)
		requireEqualf(t, w.Code, 400, "%s: status=%d", tc.name, w.Code)
	}
	bad := req
	bad.TargetHash = deltaHash([]byte("stale"))
	w := testDeltaCall(t, f, bad)
	r := bytes.NewReader(w.Body.Bytes())
	var magic [8]byte
	io.ReadFull(r, magic[:])
	if h := readDeltaTestHeader(t, r); h.Status != "version_changed" {
		t.Fatal(h)
	}
	payload, _ := json.Marshal(req)
	request := httptest.NewRequest("POST", "/delta", bytes.NewReader(payload))
	request.Header.Set(ProtocolHeader, "2")
	rec := httptest.NewRecorder()
	mw := &deltaMutatingWriter{batchWriter: &batchWriter{ResponseRecorder: rec}, mutate: func() {
		if e := os.WriteFile(filepath.Join(f.Root, "malformed"), bytes.Repeat([]byte{3}, len(target)), 0600); e != nil {
			t.Error(e)
		}
	}}
	(&Server{Folder: f}).delta(mw, request)
	r = bytes.NewReader(rec.Body.Bytes())
	io.ReadFull(r, magic[:])
	h := readDeltaTestHeader(t, r)
	requireEqual(t, h.Status, "ok", h)
	requireEqual(t, r.Len(), 0, "mutated target emitted a complete block or end")
	oversized := httptest.NewRequest("POST", "/delta", strings.NewReader(strings.Repeat(" ", 5<<20)+"{}"))
	oversized.Header.Set(ProtocolHeader, "2")
	rec = httptest.NewRecorder()
	(&Server{Folder: f}).delta(&batchWriter{ResponseRecorder: rec}, oversized)
	requireEqual(t, rec.Code, 400, rec.Code)
}
