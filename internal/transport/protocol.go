package transport

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"sharedir/internal/identity"
)

const (
	ProtocolVersion         = 2
	ProtocolHeader          = "Sharedir-Protocol"
	ProtocolHelloPath       = "/hello"
	maxHelloBytes     int64 = 64 << 10
)

// Hello is the application-level compatibility handshake exchanged after
// the mutual TLS connection has been authenticated. The remote identity is
// deliberately not part of this JSON; it is derived from the TLS certificate.
type Hello struct {
	Protocol     int               `json:"protocol"`
	StoreEpoch   string            `json:"storeEpoch"`
	PeerSetEpoch string            `json:"peerSetEpoch"`
	Features     []string          `json:"features"`
	Limits       map[string]uint64 `json:"limits,omitempty"`
}

// ProtocolError describes a missing or unsupported application protocol
// version. Received is empty when the request omitted the header.
type ProtocolError struct {
	Expected int
	Received string
}

func (e *ProtocolError) Error() string {
	if e.Received == "" {
		return fmt.Sprintf("protocol version required (expected %d)", e.Expected)
	}
	return fmt.Sprintf("protocol version %q is incompatible (expected %d)", e.Received, e.Expected)
}

// RequireProtocol rejects requests that do not explicitly select v2. It is
// intended to wrap all peer HTTP endpoints, including /hello.
func RequireProtocol(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received := r.Header.Get(ProtocolHeader)
		if received != strconv.Itoa(ProtocolVersion) {
			WriteProtocolIncompatible(w, received)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WriteProtocolIncompatible writes the stable v2 error response used for
// both missing-version and old-version requests.
func WriteProtocolIncompatible(w http.ResponseWriter, received string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUpgradeRequired)
	_ = json.NewEncoder(w).Encode(struct {
		Code     string `json:"code"`
		Expected int    `json:"expected"`
		Received string `json:"received"`
	}{"protocol_incompatible", ProtocolVersion, received})
}

// DecodeHello reads one bounded JSON handshake object and rejects malformed,
// trailing, or structurally unknown input.
func DecodeHello(r io.Reader) (Hello, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxHelloBytes+1))
	if err != nil {
		return Hello{}, fmt.Errorf("read hello: %w", err)
	}
	if int64(len(body)) > maxHelloBytes {
		return Hello{}, errors.New("decode hello: body exceeds size limit")
	}
	var hello Hello
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&hello); err != nil {
		return Hello{}, fmt.Errorf("decode hello: %w", err)
	}
	var extra json.RawMessage
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Hello{}, errors.New("decode hello: trailing JSON value")
		}
		return Hello{}, fmt.Errorf("decode hello: %w", err)
	}
	if hello.Protocol != ProtocolVersion {
		return Hello{}, &ProtocolError{Expected: ProtocolVersion, Received: strconv.Itoa(hello.Protocol)}
	}
	return hello, nil
}

// CertifiedPeerID returns the ID of the certificate authenticated by the TLS
// handshake. No identity supplied in request JSON is trusted or consulted.
func CertifiedPeerID(certificates []*x509.Certificate) (string, error) {
	if len(certificates) == 0 || len(certificates[0].Raw) == 0 {
		return "", errors.New("peer did not present a certificate")
	}
	return identity.IDFromCert(certificates[0].Raw), nil
}
