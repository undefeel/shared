package app

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

func TestReconnectLogWindowsAndRecovery(t *testing.T) {
	var out bytes.Buffer
	r := newReconnectLog(log.New(&out, "", 0), "ABCDEFG123")
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.failure(start, "network")
	r.failure(start.Add(time.Minute), "network")
	r.failure(start.Add(5*time.Minute-time.Nanosecond), "network")
	if got := strings.Count(out.String(), "reconnect failure"); got != 1 {
		t.Fatalf("before boundary: %d: %s", got, out.String())
	}
	r.failure(start.Add(5*time.Minute), "network")
	if !strings.Contains(out.String(), "code=network count=3 last=2026-01-01T00:05:00Z") {
		t.Fatal(out.String())
	}
	r.failure(start.Add(6*time.Minute), "network")
	r.failure(start.Add(10*time.Minute), "network")
	if !strings.Contains(out.String(), "code=network count=2 last=2026-01-01T00:10:00Z") {
		t.Fatal(out.String())
	}
	r.failure(start.Add(11*time.Minute), "network")
	r.failure(start.Add(11*time.Minute), "offline")
	r.failure(start.Add(12*time.Minute), "offline")
	r.recovered(start.Add(12 * time.Minute))
	if !strings.Contains(out.String(), "code=network count=1 last=") || !strings.Contains(out.String(), "code=offline count=1 last=") || !strings.Contains(out.String(), "connection recovered") {
		t.Fatal(out.String())
	}
	before := strings.Count(out.String(), "reconnect failure")
	r.failure(start.Add(13*time.Minute), "network")
	if strings.Count(out.String(), "reconnect failure") != before+1 || !strings.HasSuffix(out.String(), "code=network count=1\n") {
		t.Fatal(out.String())
	}
}

func TestReconnectLogNeverConnectedAndSecrets(t *testing.T) {
	var out bytes.Buffer
	r := newReconnectLog(log.New(&out, "", 0), "secret://user@192.0.2.1")
	now := time.Unix(0, 0)
	r.failure(now, "protocol_incompatible")
	r.failure(now.Add(time.Minute), "protocol_incompatible")
	r.failure(now.Add(5*time.Minute), "protocol_incompatible")
	r.failure(now, "secret://password@192.0.2.1")
	if strings.Count(out.String(), "code=protocol_incompatible") != 2 || !strings.Contains(out.String(), "count=2") {
		t.Fatal(out.String())
	}
	for _, secret := range []string{"secret", "192.0.2.1", "password", "user"} {
		if strings.Contains(out.String(), secret) {
			t.Fatal(out.String())
		}
	}
}

type reconnectSignalWriter struct{ lines chan string }

func (w reconnectSignalWriter) Write(p []byte) (int, error) {
	select {
	case w.lines <- string(p):
	default:
	}
	return len(p), nil
}

func TestRunPeerReconnectDoesNotLogName(t *testing.T) {
	d := folderDaemon(t)
	lines := make(chan string, 4)
	d.Log = log.New(reconnectSignalWriter{lines}, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.runPeer(ctx, &peerRunner{st: PeerStatus{ID: "ABCDEFG2345", Name: "secret-name-192.0.2.1"}})
	}()
	select {
	case line := <-lines:
		if strings.Contains(line, "secret") || strings.Contains(line, "192.0.2.1") || !strings.Contains(line, "ABCDEFG") {
			t.Fatalf("unsafe reconnect log: %q", line)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("no reconnect log")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop")
	}
}
