package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sharedir/internal/folder"
	"sharedir/internal/identity"
	"sharedir/internal/relay"
	"sharedir/internal/transport"
)

type deltaCountingWriter struct {
	http.ResponseWriter
	count   *atomic.Int64
	onWrite func()
}

func (w *deltaCountingWriter) Write(b []byte) (int, error) {
	if w.onWrite != nil {
		fn := w.onWrite
		w.onWrite = nil
		fn()
	}
	n, e := w.ResponseWriter.Write(b)
	w.count.Add(int64(n))
	return n, e
}
func (w *deltaCountingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestDeltaPeerPipeline(t *testing.T) {
	for _, mode := range []string{"direct", "relay", "local_edit"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			open := func(tag string) *folder.Folder {
				root := filepath.Join(base, tag, "root")
				os.MkdirAll(root, 0700)
				f, e := folder.Open(root, filepath.Join(base, tag, "index"), tag, log.New(io.Discard, "", 0))
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { f.Close() })
				return f
			}
			source, dest := open("source"), open("destination")
			original := make([]byte, 8<<20)
			if _, e := rand.Read(original); e != nil {
				t.Fatal(e)
			}
			target := bytes.Clone(original)
			if _, e := rand.Read(target[1<<20 : 2<<20]); e != nil {
				t.Fatal(e)
			}
			os.WriteFile(filepath.Join(dest.Root, "a"), original, 0600)
			os.WriteFile(filepath.Join(source.Root, "a"), target, 0600)
			if e := dest.Scan(); e != nil {
				t.Fatal(e)
			}
			if e := source.Scan(); e != nil {
				t.Fatal(e)
			}
			local, _, _ := dest.Lookup("a")
			remote, _, _ := source.Lookup("a")
			for k, v := range local.Version {
				remote.Version[k] = v
			}
			remote.Version["source"]++
			for _, name := range []string{"sid", "cid", "rid"} {
				if e := os.MkdirAll(filepath.Join(base, name), 0700); e != nil {
					t.Fatal(e)
				}
			}
			serverID, e := identity.LoadOrCreate(filepath.Join(base, "sid"))
			if e != nil {
				t.Fatal(e)
			}
			clientID, e := identity.LoadOrCreate(filepath.Join(base, "cid"))
			if e != nil {
				t.Fatal(e)
			}
			api := (&transport.Server{Folder: source, State: folderPeerState{source}}).Handler()
			var deltas, full atomic.Int32
			var transferred atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/delta" {
					deltas.Add(1)
					cw := &deltaCountingWriter{ResponseWriter: w, count: &transferred}
					if mode == "local_edit" {
						cw.onWrite = func() { os.WriteFile(filepath.Join(dest.Root, "a"), bytes.Repeat([]byte{5}, len(original)), 0600) }
					}
					api.ServeHTTP(cw, r)
					return
				}
				if r.URL.Path == "/file" {
					full.Add(1)
				}
				api.ServeHTTP(w, r)
			})
			srv := httptest.NewUnstartedServer(handler)
			srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverID.Cert}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
			srv.StartTLS()
			defer srv.Close()
			addr := strings.TrimPrefix(srv.URL, "https://")
			var relayDial func(context.Context) (net.Conn, error)
			if mode == "relay" {
				relayID, e := identity.LoadOrCreate(filepath.Join(base, "rid"))
				if e != nil {
					t.Fatal(e)
				}
				ln, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				defer ln.Close()
				rs := &relay.Server{ID: relayID, Token: relay.NewToken(), Log: log.New(io.Discard, "", 0)}
				go rs.Serve(ln)
				ep := relay.Endpoint{Addr: ln.Addr().String(), RelayID: relayID.ID, Token: rs.Token}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				incoming := newConnListener()
				defer incoming.Close()
				hs := &http.Server{Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{serverID.Cert}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}}
				defer hs.Close()
				go hs.Serve(tls.NewListener(incoming, hs.TLSConfig))
				ready := make(chan struct{})
				rc := &relay.Client{Endpoint: ep, ID: serverID}
				go rc.Listen(ctx, func() { close(ready) }, incoming.push)
				select {
				case <-ready:
				case <-time.After(10 * time.Second):
					t.Fatal("relay not ready")
				}
				outgoing := &relay.Client{Endpoint: ep, ID: clientID}
				relayDial = func(ctx context.Context) (net.Conn, error) { return outgoing.Connect(ctx, serverID.ID) }
				addr = transport.RelayHost
			}
			client := transport.NewClient(clientID, serverID.ID, 0, relayDial)
			defer client.Close()
			q := newPeerQueue()
			_, epoch, cursor := source.DurableState()
			if !q.accept(serverID.ID, epoch, cursor, false, []folder.FileInfo{remote}) {
				t.Fatal("queue")
			}
			d := &Daemon{folder: dest}
			applied, failed := d.applyPeerDue(context.Background(), client, addr, serverID.ID, "source", epoch, cursor, []folder.FileInfo{remote}, q, nil)
			if failed != 0 || (mode != "local_edit" && applied != 1) || (mode == "local_edit" && applied != 0) {
				t.Fatalf("apply=%d failed=%d", applied, failed)
			}
			if deltas.Load() != 1 {
				t.Fatalf("delta calls=%d", deltas.Load())
			}
			if mode == "local_edit" {
				if full.Load() != 1 {
					t.Fatalf("full fallback=%d", full.Load())
				}
			} else {
				if full.Load() != 0 || transferred.Load() < 1<<20 || transferred.Load() > (1<<20)+2048 {
					t.Fatalf("full=%d delta wire=%d", full.Load(), transferred.Load())
				}
			}
			if mode == "local_edit" {
				data, e := os.ReadFile(filepath.Join(dest.Root, "a"))
				if e != nil || !bytes.Equal(data, bytes.Repeat([]byte{5}, len(original))) {
					t.Fatal("local edit was not preserved", e)
				}
				return
			}
			got, _, ok := dest.Lookup("a")
			if !ok || got.Hash != remote.Hash || got.Version.Compare(remote.Version) != folder.Equal {
				t.Fatalf("final index %+v", got)
			}
			data, e := os.ReadFile(filepath.Join(dest.Root, "a"))
			if e != nil || !bytes.Equal(data, target) {
				t.Fatalf("final bytes %v", e)
			}
		})
	}
}
