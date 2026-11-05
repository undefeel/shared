package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"sharedir/internal/folder"
)

var ErrStaleACK = errors.New("stale ack")

const ackBodyLimit = 1 << 20
const ackMaxDeletions = 256

type AckDeletion struct {
	Name    string        `json:"name"`
	Version folder.Vector `json:"version"`
}
type AckMessage struct {
	StoreEpoch        string        `json:"storeEpoch"`
	MembershipEpoch   uint64        `json:"membershipEpoch"`
	BaselineChallenge string        `json:"baselineChallenge,omitempty"`
	Deletions         []AckDeletion `json:"deletions,omitempty"`
}

func (s *Server) authenticatedAckPeer(r *http.Request) (string, Hello, error) {
	if r.TLS == nil || s.Allowed == nil {
		return "", Hello{}, errors.New("certificate required")
	}
	peer, err := CertifiedPeerID(r.TLS.PeerCertificates)
	if err != nil || !s.Allowed(peer) {
		return "", Hello{}, errors.New("certificate rejected")
	}
	session, ok := s.sessions.Load(connectionKey(r))
	if !ok {
		return "", Hello{}, errors.New("hello required")
	}
	hello, ok := session.(Hello)
	if !ok || hello.StoreEpoch == "" {
		return "", Hello{}, errors.New("store epoch required")
	}
	return peer, hello, nil
}

// A challenge is created only after the full index snapshot exists and is
// bound durably to that exact cursor and the peer's authenticated hello epoch.
func (s *Server) prepareAckBaseline(w http.ResponseWriter, r *http.Request, idx *IndexResponse) bool {
	if s.Folder == nil {
		return true
	}
	peer, hello, err := s.authenticatedAckPeer(r)
	if err != nil {
		return true
	}
	epoch, membership, _ := s.Folder.PeerACKState()
	if epoch != idx.StoreEpoch {
		http.Error(w, "stale index", http.StatusConflict)
		return false
	}
	idx.MembershipEpoch = membership
	if !idx.Reset || r.URL.Query().Get("name") != "" {
		return true
	}
	hasDeletion := false
	for _, fi := range idx.Files {
		if fi.Deleted {
			hasDeletion = true
			break
		}
	}
	if !hasDeletion || !s.Folder.NeedsPeerBaseline(peer, hello.StoreEpoch) {
		return true
	}
	token, err := s.Folder.BeginPeerBaseline(peer, hello.StoreEpoch, epoch, idx.Cursor)
	if err != nil {
		http.Error(w, "baseline unavailable", http.StatusConflict)
		return false
	}
	idx.BaselineChallenge = token
	return true
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	peer, hello, err := s.authenticatedAckPeer(r)
	if err != nil || s.Folder == nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, ackBodyLimit+1))
	if err != nil || len(body) > ackBodyLimit {
		http.Error(w, "invalid ack", http.StatusBadRequest)
		return
	}
	var msg AckMessage
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&msg) != nil {
		http.Error(w, "invalid ack", http.StatusBadRequest)
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF || msg.StoreEpoch == "" || len(msg.StoreEpoch) > 128 || len(msg.BaselineChallenge) > 128 || len(msg.Deletions) > ackMaxDeletions {
		http.Error(w, "invalid ack", http.StatusBadRequest)
		return
	}
	files := make([]folder.FileInfo, 0, len(msg.Deletions))
	seen := map[string]bool{}
	for _, item := range msg.Deletions {
		if item.Name == "" || len(item.Name) > 4096 || seen[item.Name] || len(item.Version) == 0 || len(item.Version) > 64 {
			http.Error(w, "invalid ack", http.StatusBadRequest)
			return
		}
		for id, counter := range item.Version {
			if id == "" || len(id) > 256 || counter == 0 {
				http.Error(w, "invalid ack", http.StatusBadRequest)
				return
			}
		}
		seen[item.Name] = true
		files = append(files, folder.FileInfo{Name: item.Name, Deleted: true, Version: item.Version})
	}
	if err = s.Folder.Acknowledge(peer, msg.StoreEpoch, hello.StoreEpoch, msg.MembershipEpoch, files, msg.BaselineChallenge); err != nil {
		if errors.Is(err, folder.ErrPeerState) {
			http.Error(w, "stale ack", http.StatusConflict)
		} else {
			http.Error(w, "ack persistence failed", http.StatusServiceUnavailable)
		}
		return
	}
	if _, err = s.Folder.CollectTombstones(time.Now()); err != nil {
		http.Error(w, "collector failed", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Ack uses the same certified TLS session and protocol negotiation as Index.
func (c *Client) Ack(ctx context.Context, addr, peerID string, msg AckMessage) error {
	if peerID == "" || peerID != c.peer {
		return errors.New("unexpected peer")
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(body) > ackBodyLimit || len(msg.Deletions) > ackMaxDeletions {
		return errors.New("ack too large")
	}
	u := url.URL{Scheme: "https", Host: addr, Path: "/ack"}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return ErrStaleACK
	}
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ack %s: %s", resp.Status, b)
	}
	return nil
}
