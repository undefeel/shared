// Package relay — посредник для устройств, которые не видят друг друга
// напрямую (разные сети, NAT). Обе стороны подключаются к relay исходящим
// соединением, relay склеивает их и пересылает байты. Внутри склеенного
// канала пиры делают свой mTLS-handshake, так что relay видит только
// шифротекст и не может ни прочитать, ни подменить данные.
//
// Протокол поверх TLS 1.3 к relay (клиент предъявляет сертификат
// устройства, его хеш — ID устройства):
//
//	клиент → relay: одна JSON-строка Hello
//	  listen  — управляющий канал: relay присылает сюда Notice о входящих
//	  connect — «соедини с To»; после {"ok":true} поток — канал к пиру
//	  accept  — ответ на входящее (Session); после {"ok":true} — канал к пиру
//	relay → клиент: JSON-строки Message
package relay

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"sharedir/internal/identity"
)

const (
	Scheme      = "sharedir-relay"
	DefaultPort = "22200"
	maxLine     = 4096
)

type Hello struct {
	Op      string `json:"op"` // listen | connect | accept
	Token   string `json:"token"`
	To      string `json:"to,omitempty"`
	Session string `json:"session,omitempty"`
}

type Message struct {
	Op      string `json:"op,omitempty"` // incoming | ping
	OK      bool   `json:"ok,omitempty"`
	Error   string `json:"error,omitempty"`
	From    string `json:"from,omitempty"`
	Session string `json:"session,omitempty"`
}

// Endpoint — куда и с чем подключаться к relay.
type Endpoint struct {
	Addr    string // host:port
	RelayID string // ID сертификата relay, чтобы не подключиться к самозванцу
	Token   string // общий секрет: без него relay не обслуживает
}

// ParseURL разбирает sharedir-relay://<RELAYID>:<TOKEN>@host:port.
func ParseURL(s string) (Endpoint, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != Scheme || u.User == nil || u.Host == "" {
		return Endpoint{}, errors.New("invalid relay address: expected " + Scheme + "://ID:TOKEN@host:port")
	}
	id, err := identity.Normalize(u.User.Username())
	if err != nil {
		return Endpoint{}, err
	}
	token, _ := u.User.Password()
	if token == "" {
		return Endpoint{}, errors.New("relay address has no token")
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, DefaultPort)
	}
	return Endpoint{Addr: host, RelayID: id, Token: token}, nil
}

func (e Endpoint) URL() string {
	return fmt.Sprintf("%s://%s:%s@%s", Scheme, e.RelayID, e.Token, e.Addr)
}

func NewToken() string {
	b := make([]byte, 20)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// readLine читает одну строку по байту: всё после \n — уже поток пира
// (TLS-рукопожатие), его нельзя забрать в буфер.
func readLine(r io.Reader) ([]byte, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < maxLine {
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		if b[0] == '\n' {
			return line, nil
		}
		line = append(line, b[0])
	}
	return nil, errors.New("relay: line too long")
}

func readJSON(r io.Reader, v any) error {
	line, err := readLine(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

func writeJSON(w io.Writer, v any) error {
	b, _ := json.Marshal(v)
	_, err := w.Write(append(b, '\n'))
	return err
}
