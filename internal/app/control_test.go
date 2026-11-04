package app

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sharedir/internal/config"
)

func TestListenControlDoesNotTrustSilentSocket(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	home := filepath.Join("/tmp", strings.Repeat("h", 110), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := config.SocketPath(home)
	if err != nil {
		t.Fatal(err)
	}
	fake, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()

	ln, err := listenControl(home)
	if err != nil {
		t.Fatalf("silent socket was mistaken for daemon: %v", err)
	}
	defer ln.Close()
}
