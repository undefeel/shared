package app

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"sharedir/internal/config"
)

func TestListenControlDoesNotTrustSilentSocket(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	home := filepath.Join("/tmp", strings.Repeat("h", 110), "home")
	mustMkdirAll(t, home, 0o700)
	p, err := config.SocketPath(home)
	requireNoError(t, err)
	fake, err := net.Listen("unix", p)
	requireNoError(t, err)
	defer fake.Close()

	ln, err := listenControl(home)
	if err != nil {
		t.Fatalf("silent socket was mistaken for daemon: %v", err)
	}
	defer ln.Close()
}
