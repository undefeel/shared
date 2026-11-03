// Package config хранит настройки sharedir и пути к его файлам.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultPort          = 22100
	DefaultDiscoveryPort = 22101
)

type Peer struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Addrs []string `json:"addrs,omitempty"` // host:port, для VPN или если броадкаст не доходит
}

type Config struct {
	Name          string `json:"name"`
	Folder        string `json:"folder"`
	Port          int    `json:"port"`
	Discovery     bool   `json:"discovery"`
	DiscoveryPort int    `json:"discovery_port"`
	Peers         []Peer `json:"peers"`
}

// Home — каталог с конфигом, ключами и индексом.
// SHAREDIR_HOME переопределяет его (удобно для нескольких экземпляров).
func Home() (string, error) {
	if h := os.Getenv("SHAREDIR_HOME"); h != "" {
		return filepath.Abs(h)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sharedir"), nil
}

func Path(home string) string { return filepath.Join(home, "config.json") }

// SocketPath возвращает путь управляющего сокета. Unix-сокеты ограничены
// ~104 байтами, поэтому для длинных путей уходим в TMPDIR.
func SocketPath(home string) string {
	p := filepath.Join(home, "ctl.sock")
	if len(p) < 100 {
		return p
	}
	sum := sha256.Sum256([]byte(home))
	return filepath.Join(os.TempDir(), "sharedir-"+hex.EncodeToString(sum[:6])+".sock")
}

func Load(home string) (*Config, error) {
	data, err := os.ReadFile(Path(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("конфиг не найден, сначала выполните `sharedir init`")
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(home), err)
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.DiscoveryPort == 0 {
		c.DiscoveryPort = DefaultDiscoveryPort
	}
	c.Folder = ExpandHome(c.Folder)
	return &c, nil
}

func (c *Config) Save(home string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(home) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(home))
}

func (c *Config) FindPeer(id string) *Peer {
	for i := range c.Peers {
		if c.Peers[i].ID == id {
			return &c.Peers[i]
		}
	}
	return nil
}

func (c *Config) RemovePeer(id string) bool {
	for i := range c.Peers {
		if c.Peers[i].ID == id {
			c.Peers = append(c.Peers[:i], c.Peers[i+1:]...)
			return true
		}
	}
	return false
}

func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}
