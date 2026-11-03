// Package identity управляет ключом устройства. ID устройства — это хеш его
// самоподписанного сертификата, поэтому он одновременно служит отпечатком
// для mTLS: подделать ID без приватного ключа нельзя.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

type Identity struct {
	Cert tls.Certificate
	ID   string // полный ID без разделителей, 52 символа base32
}

// Short — первые 7 символов ID: ключ в векторах версий и метка в UI.
func (id *Identity) Short() string { return ShortID(id.ID) }

func ShortID(id string) string {
	if len(id) < 7 {
		return id
	}
	return id[:7]
}

// IDFromCert вычисляет ID по DER-сертификату.
func IDFromCert(der []byte) string {
	sum := sha256.Sum256(der)
	return b32.EncodeToString(sum[:])
}

// Pretty форматирует ID группами по 4 символа для чтения человеком.
func Pretty(id string) string {
	var parts []string
	for i := 0; i < len(id); i += 4 {
		parts = append(parts, id[i:min(i+4, len(id))])
	}
	return strings.Join(parts, "-")
}

// Normalize принимает ID в любом виде (с дефисами, пробелами, строчными) и
// возвращает канонический.
func Normalize(s string) (string, error) {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
	raw, err := b32.DecodeString(s)
	if err != nil || len(raw) != sha256.Size {
		return "", errors.New("invalid device ID")
	}
	return s, nil
}

func paths(home string) (cert, key string) {
	return filepath.Join(home, "cert.pem"), filepath.Join(home, "key.pem")
}

// LoadOrCreate читает ключ из home или генерирует новый.
func LoadOrCreate(home string) (*Identity, error) {
	certPath, keyPath := paths(home)
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		if err := generate(certPath, keyPath); err != nil {
			return nil, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &Identity{Cert: cert, ID: IDFromCert(cert.Certificate[0])}, nil
}

func generate(certPath, keyPath string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "sharedir"},
		NotBefore:    time.Now().Add(-24 * time.Hour),
		NotAfter:     time.Now().AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"sharedir"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}
