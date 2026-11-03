// Package transport — обмен индексами и файлами между устройствами поверх
// HTTPS со взаимной аутентификацией. Сертификаты самоподписанные; доверие
// строится на ID устройства (хеше сертификата), а не на CA.
//
// Модель «тянем»: каждое устройство держит long-poll на /index пира и само
// скачивает недостающее через /file.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"sharedir/internal/folder"
	"sharedir/internal/identity"
)

const (
	pollTimeout = 50 * time.Second
	// portHeader сообщает серверу, на каком порту слушает клиент: так
	// сторона, к которой подключились, узнаёт обратный адрес пира и адрес
	// достаточно прописать вручную только на одной машине.
	portHeader = "X-Sharedir-Port"
)

type IndexResponse struct {
	Seq   uint64            `json:"seq"`
	Files []folder.FileInfo `json:"files"`
}

func peerID(rawCerts [][]byte) (string, error) {
	if len(rawCerts) == 0 {
		return "", errors.New("пир не предъявил сертификат")
	}
	return identity.IDFromCert(rawCerts[0]), nil
}

// ServerTLS принимает только клиентов, для которых allowed(id) == true.
// Остальные отклоняются на рукопожатии, а onReject узнаёт их ID —
// так незнакомое устройство можно предложить к сопряжению.
func ServerTLS(id *identity.Identity, allowed func(string) bool, onReject func(id string)) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{id.Cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			pid, err := peerID(raw)
			if err != nil {
				return err
			}
			if !allowed(pid) {
				onReject(pid)
				return fmt.Errorf("устройство %s не сопряжено", identity.ShortID(pid))
			}
			return nil
		},
	}
}

// RelayHost — псевдо-адрес пира «через relay». Клиент, получив его,
// дозванивается не по TCP, а через relayDial.
const RelayHost = "relay"

// ErrPeerPaused — пир поставил синхронизацию на паузу.
var ErrPeerPaused = errors.New("пир на паузе")

type Server struct {
	Folder *folder.Folder
	Log    *log.Logger
	// OnPeer вызывается на каждый запрос с ID пира и его обратным адресом.
	OnPeer func(id, addr string)
	// Paused — пока true, пиру отвечаем 503 и ничего не отдаём.
	Paused func() bool
	// OnServed вызывается, когда пир полностью скачал файл.
	OnServed func(peerID string, fi folder.FileInfo)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /index", s.index)
	mux.HandleFunc("GET /file", s.file)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Paused != nil && s.Paused() {
			http.Error(w, "paused", http.StatusServiceUnavailable)
			return
		}
		if s.OnPeer != nil && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			if port, err := strconv.Atoi(r.Header.Get(portHeader)); err == nil && port > 0 && host != "" {
				s.OnPeer(identity.IDFromCert(r.TLS.PeerCertificates[0].Raw), net.JoinHostPort(host, strconv.Itoa(port)))
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	s.Folder.Wait(since, pollTimeout)
	if s.Paused != nil && s.Paused() {
		http.Error(w, "paused", http.StatusServiceUnavailable)
		return
	}
	files, seq := s.Folder.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(IndexResponse{Seq: seq, Files: files})
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fi, p, ok := s.Folder.Lookup(q.Get("name"))
	if !ok || fi.Deleted || fi.Hash != q.Get("hash") {
		http.Error(w, "версия устарела", http.StatusConflict)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "файл недоступен", http.StatusConflict)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size, 10))
	if _, err := io.Copy(w, f); err == nil && s.OnServed != nil && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		s.OnServed(identity.IDFromCert(r.TLS.PeerCertificates[0].Raw), fi)
	}
}

type Client struct {
	http *http.Client
	port int
}

// NewClient создаёт клиента, который доверяет только серверу с ID peer.
// port — порт нашего сервера, его узнает пир. relayDial открывает канал
// к пиру через relay, когда адрес — RelayHost (может быть nil).
func NewClient(id *identity.Identity, peer string, port int, relayDial func(context.Context) (net.Conn, error)) *Client {
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{id.Cert},
		InsecureSkipVerify: true, // цепочку не проверяем: проверяем ID ниже
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			pid, err := peerID(raw)
			if err != nil {
				return err
			}
			if pid != peer {
				return fmt.Errorf("по адресу отвечает другое устройство (%s)", identity.ShortID(pid))
			}
			return nil
		},
	}
	return &Client{port: port, http: &http.Client{Transport: &http.Transport{
		TLSClientConfig:     tlsCfg,
		DialContext:         dialer(relayDial),
		TLSHandshakeTimeout: 5 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}}}
}

func dialer(relayDial func(context.Context) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	direct := (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(addr); host == RelayHost {
			if relayDial == nil {
				return nil, errors.New("relay is not configured")
			}
			return relayDial(ctx)
		}
		return direct(ctx, network, addr)
	}
}

func (c *Client) get(ctx context.Context, addr, path string, q url.Values) (*http.Response, error) {
	u := url.URL{Scheme: "https", Host: addr, Path: path, RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(portHeader, strconv.Itoa(c.port))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		resp.Body.Close()
		return nil, ErrPeerPaused
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", resp.Status, body)
	}
	return resp, nil
}

// Index ждёт изменения индекса пира относительно since (long-poll).
func (c *Client) Index(ctx context.Context, addr string, since uint64) (*IndexResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout+20*time.Second)
	defer cancel()
	resp, err := c.get(ctx, addr, "/index", url.Values{"since": {strconv.FormatUint(since, 10)}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out IndexResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Fetcher возвращает загрузчик файлов с пира по адресу addr.
func (c *Client) Fetcher(addr string) folder.Fetcher {
	return func(ctx context.Context, fi folder.FileInfo, w io.Writer) error {
		resp, err := c.get(ctx, addr, "/file", url.Values{"name": {fi.Name}, "hash": {fi.Hash}})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(w, resp.Body)
		return err
	}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }
