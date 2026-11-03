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
	"time"

	"sharedir/internal/config"
)

// ErrNotRunning — демон не запущен (сокет недоступен).
var ErrNotRunning = errors.New("daemon is not running")

func listenControl(home string) (net.Listener, error) {
	p := config.SocketPath(home)
	if c, err := net.DialTimeout("unix", p, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("sharedir уже запущен (%s)", p)
	}
	os.Remove(p) // остался от упавшего процесса
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	os.Chmod(p, 0o600)
	return ln, nil
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
	return mux
}

// Ctl — клиент управляющего сокета для CLI.
type Ctl struct{ http *http.Client }

func NewCtl(home string) *Ctl {
	p := config.SocketPath(home)
	return &Ctl{http: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
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
