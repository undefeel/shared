package transport

import (
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sharedir/internal/identity"
)

func TestRequireProtocolRejectsV1AndMissingVersion(t *testing.T) {
	called := false
	h := RequireProtocol(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	for _, version := range []string{"1", ""} {
		t.Run("version="+version, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/index", nil)
			if version != "" {
				r.Header.Set(ProtocolHeader, version)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUpgradeRequired {
				t.Fatalf("status=%d, want 426", w.Code)
			}
			var got struct {
				Code     string `json:"code"`
				Expected int    `json:"expected"`
				Received string `json:"received"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Code != "protocol_incompatible" || got.Expected != 2 || got.Received != version {
				t.Fatalf("unexpected error response: %+v", got)
			}
		})
	}
	if called {
		t.Fatal("handler ran for an incompatible protocol")
	}

	r := httptest.NewRequest(http.MethodGet, "/index", nil)
	r.Header.Set(ProtocolHeader, "2")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !called {
		t.Fatalf("v2 request not passed through: status=%d called=%v", w.Code, called)
	}
}

func TestDecodeHelloRejectsBadJSON(t *testing.T) {
	for _, body := range []string{`{"protocol":2`, `{"protocol":2} {}`, `{"protocol":2,"unexpected":true}`} {
		if _, err := DecodeHello(strings.NewReader(body)); err == nil {
			t.Fatalf("DecodeHello(%q) succeeded", body)
		}
	}
}

func TestDecodeHelloAndCertifiedPeerIdentity(t *testing.T) {
	hello, err := DecodeHello(strings.NewReader(`{"protocol":2,"storeEpoch":"store","peerSetEpoch":"peers","features":["files"],"limits":{"files":64}}`))
	if err != nil {
		t.Fatal(err)
	}
	if hello.Protocol != 2 || hello.StoreEpoch != "store" || hello.PeerSetEpoch != "peers" || len(hello.Features) != 1 || hello.Limits["files"] != 64 {
		t.Fatalf("decoded hello: %+v", hello)
	}

	id, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(id.Cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := CertifiedPeerID([]*x509.Certificate{cert})
	if err != nil {
		t.Fatal(err)
	}
	if got != id.ID {
		t.Fatalf("certified identity=%q, want %q", got, id.ID)
	}
	if _, err := CertifiedPeerID(nil); err == nil {
		t.Fatal("missing certificate accepted")
	}
}
