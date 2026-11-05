package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"sharedir/internal/folder"
	"sharedir/internal/identity"
)

const deltaMagic = "SHDDEL02"
const deltaMaxHeader = 4096

type deltaHeader struct {
	Status     string `json:"status,omitempty"`
	TargetHash string `json:"targetHash,omitempty"`
	TargetSize int64  `json:"targetSize,omitempty"`
	Offset     int64  `json:"offset,omitempty"`
	Length     int64  `json:"length,omitempty"`
	Hash       string `json:"hash,omitempty"`
	Encoding   string `json:"encoding,omitempty"`
	WireSize   int64  `json:"wireSize,omitempty"`
	End        bool   `json:"end,omitempty"`
	Frames     int    `json:"frames,omitempty"`
}

func writeDeltaHeader(w io.Writer, h deltaHeader) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(b) > deltaMaxHeader {
		return io.ErrShortBuffer
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	if err = writeAll(w, size[:]); err != nil {
		return err
	}
	return writeAll(w, b)
}

func validDeltaHash(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func deltaCount(n int64) int { return int((n + folder.DeltaBlockSize - 1) / folder.DeltaBlockSize) }
func deltaLength(size int64, i int) int64 {
	n := size - int64(i)*folder.DeltaBlockSize
	if n > folder.DeltaBlockSize {
		return folder.DeltaBlockSize
	}
	return n
}
func sameDeltaStat(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime() && a.Mode() == b.Mode()
}

func (s *Server) delta(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Header.Get(ProtocolHeader) != "2" {
		WriteProtocolIncompatible(w, r.Header.Get(ProtocolHeader))
		return
	}
	generation := s.Folder.ConfirmedGeneration()
	unavailable := func() bool { return (s.Paused != nil && s.Paused()) || s.Folder.CheckRead(generation) != nil }
	if unavailable() {
		http.Error(w, "folder unavailable", 503)
		return
	}
	var req folder.DeltaRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5<<20))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil {
		http.Error(w, "invalid delta request", 400)
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "trailing delta request", 400)
		return
	}
	if req.Protocol != ProtocolVersion {
		WriteProtocolIncompatible(w, "body")
		return
	}
	max := int64(folder.DeltaMaxBlocks) * folder.DeltaBlockSize
	if req.Name == "" || !validDeltaHash(req.TargetHash) || !validDeltaHash(req.BaseHash) || req.TargetSize < 0 || req.TargetSize > max || req.BaseSize < 0 || req.BaseSize > max || req.BlockSize != folder.DeltaBlockSize || len(req.Blocks) != deltaCount(req.BaseSize) {
		http.Error(w, "invalid delta request", 400)
		return
	}
	for _, h := range req.Blocks {
		if !validDeltaHash(h) {
			http.Error(w, "invalid block hash", 400)
			return
		}
	}
	status := "version_changed"
	fi, f, err := s.Folder.OpenVersion(req.Name, req.TargetHash)
	var targetStat os.FileInfo
	var hashes []string
	var changed []int
	if err == nil {
		defer f.Close()
		before, e := f.Stat()
		if e == nil && fi.Size == req.TargetSize && before.Size() == req.TargetSize {
			hashes = make([]string, deltaCount(req.TargetSize))
			full := sha256.New()
			buf := make([]byte, folder.DeltaBlockSize)
			var changedBytes int64
			good := true
			for i := range hashes {
				n := deltaLength(req.TargetSize, i)
				if _, e = io.ReadFull(f, buf[:n]); e != nil {
					good = false
					break
				}
				full.Write(buf[:n])
				h := sha256.Sum256(buf[:n])
				hashes[i] = hex.EncodeToString(h[:])
				if i >= len(req.Blocks) || deltaLength(req.BaseSize, i) != n || req.Blocks[i] != hashes[i] {
					changed = append(changed, i)
					changedBytes += n
				}
			}
			after, statErr := f.Stat()
			if good && statErr == nil && sameDeltaStat(before, after) && hex.EncodeToString(full.Sum(nil)) == req.TargetHash {
				targetStat = after
				status = "ok"
				if req.TargetSize > 0 && changedBytes*100 >= req.TargetSize*70 {
					status = "full_required"
				}
			}
		}
	}
	if unavailable() {
		http.Error(w, "folder unavailable", 503)
		return
	}
	if !s.authorizeFrame(r) {
		return
	}
	w.Header().Set("Content-Type", "application/x-sharedir-delta;version=2")
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	out := &progressWriter{w: w, idle: s.writeIdle()}
	if writeAll(out, []byte(deltaMagic)) != nil || writeDeltaHeader(out, deltaHeader{Status: status, TargetHash: req.TargetHash, TargetSize: req.TargetSize}) != nil || status != "ok" {
		return
	}
	before, err := f.Stat()
	if err != nil || !sameDeltaStat(targetStat, before) {
		return
	}
	buf := make([]byte, folder.DeltaBlockSize)
	for _, i := range changed {
		if r.Context().Err() != nil || unavailable() || !s.authorizeFrame(r) {
			return
		}
		n := deltaLength(req.TargetSize, i)
		if _, err = f.ReadAt(buf[:n], int64(i)*folder.DeltaBlockSize); err != nil {
			return
		}
		h := sha256.Sum256(buf[:n])
		if hex.EncodeToString(h[:]) != hashes[i] {
			return
		}
		var body bytes.Buffer
		encoding, plain, wire, digest, e := EncodeBody(&body, bytes.NewReader(buf[:n]), req.Name, strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"), n)
		if e != nil || plain != n || wire > folder.DeltaBlockSize+1024 || digest != h {
			return
		}
		if writeDeltaHeader(out, deltaHeader{Offset: int64(i) * folder.DeltaBlockSize, Length: n, Hash: hashes[i], Encoding: encoding, WireSize: wire}) != nil || writeAll(out, body.Bytes()) != nil {
			return
		}
	}
	after, err := f.Stat()
	if err != nil || !sameDeltaStat(before, after) || unavailable() || !s.authorizeFrame(r) || r.Context().Err() != nil {
		return
	}
	if writeDeltaHeader(out, deltaHeader{End: true, TargetHash: req.TargetHash, TargetSize: req.TargetSize, Frames: len(changed)}) == nil && s.OnServed != nil && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		s.OnServed(identity.IDFromCert(r.TLS.PeerCertificates[0].Raw), fi)
	}
}
