package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"sharedir/internal/config"
	"sharedir/internal/folder"
)

// ErrNotRunning — демон не запущен (сокет недоступен).
var ErrNotRunning = errors.New("daemon is not running")

func listenControl(home string) (net.Listener, error) {
	p, err := config.SocketPath(home)
	if err != nil {
		return nil, err
	}
	if fi, statErr := os.Lstat(p); statErr == nil {
		if !ownedSocket(fi) {
			return nil, fmt.Errorf("небезопасный управляющий сокет %s: владелец не совпадает с текущим пользователем", p)
		}
		if daemonAtSocket(p) {
			return nil, fmt.Errorf("sharedir уже запущен (%s)", p)
		}
		if err := os.Remove(p); err != nil {
			return nil, fmt.Errorf("не удалось удалить неактивный управляющий сокет %s: %w", p, err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("не удалось проверить управляющий сокет %s: %w", p, statErr)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(p, 0o600); err != nil {
		ln.Close()
		os.Remove(p)
		return nil, fmt.Errorf("не удалось ограничить права управляющего сокета %s: %w", p, err)
	}
	return ln, nil
}

func ownedSocket(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode()&os.ModeSocket != 0 && fi.Mode()&os.ModeSymlink == 0 && uint32(st.Uid) == uint32(os.Getuid())
}

func daemonAtSocket(p string) bool {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p)
	}}}
	resp, err := client.Get("http://sharedir/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var status map[string]any
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&status) == nil && status != nil
}

type pairRequest struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Addr string `json:"addr,omitempty"`
}

func (d *Daemon) controlHandler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, err error) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(d.Status())
	})
	mux.HandleFunc("POST /pair", func(w http.ResponseWriter, r *http.Request) {
		var req pairRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, err)
			return
		}
		reply(w, d.Pair(req.ID, req.Name, req.Addr))
	})
	mux.HandleFunc("POST /unpair", func(w http.ResponseWriter, r *http.Request) {
		var req pairRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, err)
			return
		}
		reply(w, d.Unpair(req.ID))
	})
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) {
		reply(w, d.Reload())
	})
	mux.HandleFunc("POST /pause", func(w http.ResponseWriter, r *http.Request) {
		reply(w, d.SetPaused(true))
	})
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, r *http.Request) {
		reply(w, d.SetPaused(false))
	})
	mux.HandleFunc("POST /folder/confirm", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token  string `json:"token"`
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, err)
			return
		}
		if err := d.ConfirmFolder(req.Token, req.Action); err != nil {
			if errors.Is(err, folder.ErrStaleToken) {
				http.Error(w, err.Error(), http.StatusConflict)
			} else {
				reply(w, err)
			}
		}
	})
	return mux
}

// Ctl — клиент управляющего сокета для CLI.
type Ctl struct{ http *http.Client }

func NewCtl(home string) *Ctl {
	p, pathErr := config.SocketPath(home)
	return &Ctl{http: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if pathErr != nil {
				return nil, pathErr
			}
			fi, err := os.Lstat(p)
			if err != nil || !ownedSocket(fi) {
				return nil, fmt.Errorf("небезопасный управляющий сокет %s: владелец не совпадает с текущим пользователем", p)
			}
			return (&net.Dialer{}).DialContext(ctx, "unix", p)
		}},
	}}
}

func (c *Ctl) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://sharedir"+path, body)
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrNotRunning
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return errors.New(string(bytes.TrimSpace(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Ctl) Status() (*Status, error) {
	var st Status
	return &st, c.do("GET", "/status", nil, &st)
}

func (c *Ctl) Pair(id, name, addr string) error {
	return c.do("POST", "/pair", pairRequest{id, name, addr}, nil)
}

func (c *Ctl) Unpair(id string) error {
	return c.do("POST", "/unpair", pairRequest{ID: id}, nil)
}

func (c *Ctl) Reload() error {
	return c.do("POST", "/reload", nil, nil)
}

func (c *Ctl) SetPaused(paused bool) error {
	if paused {
		return c.do("POST", "/pause", nil, nil)
	}
	return c.do("POST", "/resume", nil, nil)
}

// PairOffline правит конфиг напрямую, когда демон не запущен.
func PairOffline(home, id, name, addr string) error {
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	if err := pairInConfig(cfg, id, name, addr); err != nil {
		return err
	}
	return cfg.Save(home)
}

func (c *Ctl) ConfirmFolder(token, action string) error {
	return c.do("POST", "/folder/confirm", map[string]string{"token": token, "action": action}, nil)
}
