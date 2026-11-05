package transport

import (
	"crypto/tls"
	"crypto/x509"
	"net/http/httptest"
	"strings"
	"testing"

	"sharedir/internal/identity"
)

func TestAckAuthenticationAndValidation(t *testing.T) {
	f, _ := fileFixture(t)
	raw := []byte("certified test peer")
	id := identity.IDFromCert(raw)
	if err := f.SetPeers([]string{id}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Folder: f, Allowed: func(p string) bool { return p == id }}
	for _, tc := range []struct {
		body string
		cert bool
		code int
	}{
		{`{}`, false, 403},
		{`{"storeEpoch":"x","membershipEpoch":1,"unknown":1}`, true, 400},
		{`{"storeEpoch":"x","membershipEpoch":1} {}`, true, 400},
		{`{"storeEpoch":"x","membershipEpoch":1,"deletions":[{"name":"x","version":{"p":0}}]}`, true, 400},
		{`{"storeEpoch":"x","membershipEpoch":1}`, true, 409},
		{strings.Repeat(" ", ackBodyLimit+1), true, 400},
	} {
		r := httptest.NewRequest("POST", "/ack", strings.NewReader(tc.body))
		if tc.cert {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: raw}}}
			s.sessions.Store(connectionKey(r), Hello{StoreEpoch: "remote"})
		}
		w := httptest.NewRecorder()
		s.ack(w, r)
		if w.Code != tc.code {
			t.Fatalf("code %d, want %d: %s", w.Code, tc.code, w.Body.String())
		}
	}
}
