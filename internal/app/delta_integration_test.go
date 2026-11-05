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
	base := t.TempDir()
	open := func(tag string) *folder.Folder {
		return testOpenFolder(t, filepath.Join(base, tag, "root"), filepath.Join(base, tag, "index"), tag)
	}
	source, dest := open("source"), open("destination")
	modes := []string{"direct", "relay", "local_edit"}
	originals, targets := make(map[string][]byte), make(map[string][]byte)
	for _, mode := range modes {
		original := make([]byte, 8<<20)
		if _, err := rand.Read(original); err != nil {
			t.Fatal(err)
		}
		target := bytes.Clone(original)
		if _, err := rand.Read(target[1<<20 : 2<<20]); err != nil {
			t.Fatal(err)
		}
		originals[mode], targets[mode] = original, target
		writeTestFile(t, filepath.Join(dest.Root, mode), original, 0600)
		writeTestFile(t, filepath.Join(source.Root, mode), target, 0600)
	}
	errScan := make(chan error, 2)
	go func() { errScan <- dest.Scan() }()
	go func() { errScan <- source.Scan() }()
	err1, err2 := <-errScan, <-errScan
	if err1 != nil || err2 != nil {
		t.Fatalf("initial scans: %v; %v", err1, err2)
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			original, target := originals[mode], targets[mode]
			base := t.TempDir()
			local, _, _ := dest.Lookup(mode)
			remote, _, _ := source.Lookup(mode)
			for k, v := range local.Version {
				remote.Version[k] = v
			}
			remote.Version["source"]++
			for _, name := range []string{"sid", "cid", "rid"} {
				mustMkdirAll(t, filepath.Join(base, name), 0700)
			}
			serverID, e := identity.LoadOrCreate(filepath.Join(base, "sid"))
			requireNoError(t, e)
			clientID, e := identity.LoadOrCreate(filepath.Join(base, "cid"))
			requireNoError(t, e)
			api := (&transport.Server{Folder: source, State: folderPeerState{source}}).Handler()
			var deltas, full atomic.Int32
			var transferred atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/delta" {
					deltas.Add(1)
					cw := &deltaCountingWriter{ResponseWriter: w, count: &transferred}
					if mode == "local_edit" {
						cw.onWrite = func() { os.WriteFile(filepath.Join(dest.Root, mode), bytes.Repeat([]byte{5}, len(original)), 0600) }
					}
					api.ServeHTTP(cw, r)
					return
				}
				if r.URL.Path == "/file" {
					full.Add(1)
				}
				api.ServeHTTP(w, r)
			})
			srv := testTLSServer(t, handler, serverID)
			addr := strings.TrimPrefix(srv.URL, "https://")
			var relayDial func(context.Context) (net.Conn, error)
			if mode == "relay" {
				relayID, e := identity.LoadOrCreate(filepath.Join(base, "rid"))
				requireNoError(t, e)
				ln, e := net.Listen("tcp", "127.0.0.1:0")
				requireNoError(t, e)
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
			requireEqualf(t, deltas.Load(), 1, "delta calls=%d", deltas.Load())
			if mode == "local_edit" {
				requireEqualf(t, full.Load(), 1, "full fallback=%d", full.Load())
			} else {
				if full.Load() != 0 || transferred.Load() < 1<<20 || transferred.Load() > (1<<20)+2048 {
					t.Fatalf("full=%d delta wire=%d", full.Load(), transferred.Load())
				}
			}
			if mode == "local_edit" {
				requireFileContents(t, filepath.Join(dest.Root, mode), bytes.Repeat([]byte{5}, len(original)), "local edit was not preserved")
				return
			}
			got, _, ok := dest.Lookup(mode)
			if !ok || got.Hash != remote.Hash || got.Version.Compare(remote.Version) != folder.Equal {
				t.Fatalf("final index %+v", got)
			}
			requireFileContents(t, filepath.Join(dest.Root, mode), target, "final bytes")
		})
	}
}
