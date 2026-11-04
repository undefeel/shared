package transport

import (
	"bytes"
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

const batchRequestLimit = 256 << 10

type BatchItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// files is registered by the v2 router as POST /files.
func (s *Server) files(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		Protocol int         `json:"protocol"`
		Items    []BatchItem `json:"items"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, batchRequestLimit))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		http.Error(w, "invalid batch", 400)
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "trailing batch data", 400)
		return
	}
	if req.Protocol != ProtocolVersion {
		WriteProtocolIncompatible(w, "body")
		return
	}
	ids, names := map[string]bool{}, map[string]bool{}
	var total int64
	if len(req.Items) > BatchMaxItems {
		http.Error(w, "too many items", 400)
		return
	}
	for _, item := range req.Items {
		if item.ID == "" || item.Name == "" || ids[item.ID] || names[item.Name] || item.Size < 0 || item.Size > BatchMaxPlain {
			http.Error(w, "invalid batch item", 400)
			return
		}
		// Validate the escaped wire representation, including worst-case
		// status and numeric fields, before starting the response.
		header, _ := json.Marshal(BatchHeader{ID: item.ID, Name: item.Name, Hash: item.Hash, Status: "version_changed", Size: BatchMaxPlain, WireSize: BatchMaxWire, Encoding: "identity"})
		if len(header) > BatchMaxHeader {
			http.Error(w, "batch header too large", 400)
			return
		}
		ids[item.ID], names[item.Name] = true, true
		total += item.Size
	}
	if total > BatchMaxTotal {
		http.Error(w, "batch too large", 400)
		return
	}
	if unavailable() {
		http.Error(w, "folder unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/x-sharedir-files;version=2")
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	out := &progressWriter{w: w, idle: s.writeIdle()}
	if WriteBatchMagic(out) != nil {
		return
	}
	// Reuse one bounded buffer; never retain the bodies of the whole batch.
	var body bytes.Buffer
	for _, item := range req.Items {
		if r.Context().Err() != nil {
			return
		}
		h := BatchHeader{ID: item.ID, Name: item.Name, Hash: item.Hash, Status: "version_changed"}
		body.Reset()
		var served folder.FileInfo
		if unavailable() {
			h.Status = "blocked"
		} else {
			fi, f, err := s.Folder.OpenVersion(item.Name, item.Hash)
			if err != nil {
				known, _, exists := s.Folder.Lookup(item.Name)
				if os.IsNotExist(err) || !exists || known.Deleted {
					h.Status = "not_found"
				}
			} else {
				encoding, n, wire, hash, err := EncodeBody(&body, f, item.Name, strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"), item.Size)
				f.Close()
				if err == nil && n == item.Size && fi.Size == item.Size && wire <= BatchMaxWire && hex.EncodeToString(hash[:]) == item.Hash {
					h.Status, h.Size, h.WireSize, h.Encoding = "ok", n, wire, encoding
					served = fi
				}
			}
			if unavailable() {
				h.Status = "blocked"
			}
		}
		if !s.authorizeFrame(r) {
			return
		}
		payload := body.Bytes()
		if h.Status != "ok" {
			payload = nil
			h.Size = 0
			h.WireSize = 0
			h.Encoding = ""
		}
		if WriteBatchFrame(out, h, payload) != nil {
			return
		}
		if h.Status == "ok" && s.OnServed != nil && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			// OpenVersion already strips local Disk metadata.
			s.OnServed(identity.IDFromCert(r.TLS.PeerCertificates[0].Raw), served)
		}
	}
	_ = WriteBatchFrame(out, BatchHeader{End: true, Count: len(req.Items)}, nil)
}

func (s *Server) authorizeFrame(r *http.Request) bool {
	return s.Allowed == nil || r.TLS != nil && len(r.TLS.PeerCertificates) > 0 && s.Allowed(identity.IDFromCert(r.TLS.PeerCertificates[0].Raw))
}
