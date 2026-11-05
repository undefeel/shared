package transport

import (
	"crypto/tls"
	"crypto/x509"
	"net/http/httptest"
	"strings"
	"testing"

	"sharedir/internal/folder"
	"sharedir/internal/identity"
)

func testAckAuthenticationAndValidation(t *testing.T, f *folder.Folder) {
	raw := []byte("certified test peer")
	id := identity.IDFromCert(raw)
	requireNoError(t, f.SetPeers([]string{id}))
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
		requireEqualf(t, w.Code, tc.code, "code %d, want %d: %s", w.Code, tc.code, w.Body.String())
	}
}
