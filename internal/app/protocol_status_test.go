package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sharedir/internal/config"
	"sharedir/internal/folder"
	"sharedir/internal/identity"
)

func TestRunPeerReportsIncompatibleAndCancelsRetry(t *testing.T) {
	dir := t.TempDir()
	peerHome, localHome := filepath.Join(dir, "peer-id"), filepath.Join(dir, "local-id")
	_ = os.MkdirAll(peerHome, 0700)
	_ = os.MkdirAll(localHome, 0700)
	peerID, err := identity.LoadOrCreate(peerHome)
	requireNoError(t, err)
	localID, err := identity.LoadOrCreate(localHome)
	requireNoError(t, err)
	old := httptest.NewUnstartedServer(http.NotFoundHandler())
	old.TLS = &tls.Config{Certificates: []tls.Certificate{peerID.Cert}, MinVersion: tls.VersionTLS13}
	old.StartTLS()
	defer old.Close()
	addr := strings.TrimPrefix(old.URL, "https://")
	root, state := filepath.Join(dir, "root"), filepath.Join(dir, "state", "index.json")
	_ = os.MkdirAll(root, 0700)
	_ = os.MkdirAll(filepath.Dir(state), 0700)
	f, err := folder.Open(root, state, "local", log.New(io.Discard, "", 0))
	requireNoError(t, err)
	defer f.Close()
	pr := &peerRunner{st: PeerStatus{ID: peerID.ID, Name: "old-peer"}, lan: make(chan struct{}, 1)}
	d := &Daemon{ID: localID, folder: f, Log: log.New(io.Discard, "", 0), cfg: &config.Config{Port: 0, Peers: []config.Peer{{ID: peerID.ID, Name: "old-peer", Addrs: []string{addr}}}}, peers: map[string]*peerRunner{peerID.ID: pr}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.runPeer(ctx, pr); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		pr.mu.Lock()
		st := pr.st
		pr.mu.Unlock()
		if st.State == StateIncompatible {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("peer never entered incompatible state")
		case <-time.After(10 * time.Millisecond):
		}
	}
	pr.mu.Lock()
	raw, _ := json.Marshal(pr.st)
	st := pr.st
	pr.mu.Unlock()
	if st.Connected || st.Syncing || st.Progress != nil || st.ExpectedProtocol != 2 || st.ErrorCode != "protocol_incompatible" {
		t.Fatalf("incompatible status = %+v", st)
	}
	for _, key := range []string{`"error_code"`, `"error_message"`, `"expected_protocol"`, `"received_protocol"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("status JSON lacks %s: %s", key, raw)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("60 second handshake retry was not cancellable")
	}
}
