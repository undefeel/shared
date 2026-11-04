package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("SHAREDIR_HOME", "")
	short := "/Users/alice/.config/sharedir"
	long := "/tmp/" + strings.Repeat("h", 110)

	for _, tc := range []struct {
		name, home, want string
	}{
		{"short home", short, filepath.Join(short, "ctl.sock")},
		{"long home", long, filepath.Join(tmp, fmt.Sprintf("sharedir-%d", os.Getuid()), "f99d0153b32f.sock")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SocketPath(tc.home)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("SocketPath(%q) = %q, want %q", tc.home, got, tc.want)
			}
		})
	}
}

func TestSocketPathRejectsUnsafePrivateDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	base := filepath.Join(tmp, fmt.Sprintf("sharedir-%d", os.Getuid()))
	long := "/tmp/" + strings.Repeat("h", 110)

	t.Run("permissions", func(t *testing.T) {
		if err := os.Mkdir(base, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(base, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := SocketPath(long); err == nil {
			t.Fatal("SocketPath accepted a private directory with mode 0777")
		}
		os.Remove(base)
	})
	t.Run("symlink", func(t *testing.T) {
		target := t.TempDir()
		if err := os.Symlink(target, base); err != nil {
			t.Fatal(err)
		}
		if _, err := SocketPath(long); err == nil {
			t.Fatal("SocketPath accepted a symlink as private directory")
		}
	})
}

func TestDownloadConcurrencyConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want int
		bad  bool
	}{
		{name: "default", json: `{}`, want: DefaultDownloadConcurrency},
		{name: "minimum", json: `{"download_concurrency":1}`, want: 1},
		{name: "maximum", json: `{"download_concurrency":8}`, want: 8},
		{name: "explicit zero", json: `{"download_concurrency":0}`, bad: true},
		{name: "too high", json: `{"download_concurrency":9}`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(Path(home), []byte(tc.json), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(home)
			if tc.bad {
				if err == nil {
					t.Fatal("Load accepted invalid download_concurrency")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DownloadConcurrency != tc.want {
				t.Fatalf("DownloadConcurrency = %d, want %d", cfg.DownloadConcurrency, tc.want)
			}
		})
	}
}
