package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sharedir/internal/app"
	"sharedir/internal/config"
	"sharedir/internal/folder"
)

type mockControlServer struct {
	mu           sync.Mutex
	status       app.Status
	lastAction   string
	lastToken    string
	confirmCalls int
	confirmCode  int
	confirmErr   string
}

func startMockDaemon(t *testing.T, m *mockControlServer) (string, func()) {
	t.Helper()
	home := t.TempDir()
	sockPath, err := config.SocketPath(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		st := m.status
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /folder/confirm", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token  string `json:"token"`
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.confirmCalls++
		m.lastToken = req.Token
		m.lastAction = req.Action
		code := m.confirmCode
		errMsg := m.confirmErr
		m.mu.Unlock()

		if code != 0 && code != http.StatusOK {
			http.Error(w, errMsg, code)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	cleanup := func() {
		_ = srv.Close()
		_ = ln.Close()
	}
	return home, cleanup
}

func TestFolderConfirmExplicitActionAndTokenRequired(t *testing.T) {
	home := t.TempDir()

	// Missing action
	err := cmdFolderConfirm(home, []string{"--token", "tok123"})
	if err == nil || !strings.Contains(err.Error(), "требуется явное действие") {
		t.Fatalf("expected explicit action error, got %v", err)
	}

	// Invalid action
	err = cmdFolderConfirm(home, []string{"--action", "invalid", "--token", "tok123"})
	if err == nil || !strings.Contains(err.Error(), "требуется явное действие") {
		t.Fatalf("expected explicit action error for invalid action, got %v", err)
	}

	// Missing token
	err = cmdFolderConfirm(home, []string{"--action", "reset"})
	if err == nil || !strings.Contains(err.Error(), "требуется токен подтверждения") {
		t.Fatalf("expected token required error, got %v", err)
	}

	// Empty token
	err = cmdFolderConfirm(home, []string{"--action", "rebind", "--token", "   "})
	if err == nil || !strings.Contains(err.Error(), "требуется токен подтверждения") {
		t.Fatalf("expected token required error for empty token, got %v", err)
	}
}

func TestFolderConfirmInvalidOrStaleToken(t *testing.T) {
	mock := &mockControlServer{
		confirmCode: http.StatusConflict,
		confirmErr:  folder.ErrStaleToken.Error(),
	}
	home, cleanup := startMockDaemon(t, mock)
	defer cleanup()

	err := cmdFolderConfirm(home, []string{"--action", "reset", "--token", "stale_token_123"})
	if err == nil {
		t.Fatal("expected error on stale/invalid token, got nil")
	}
	if !strings.Contains(err.Error(), folder.ErrStaleToken.Error()) {
		t.Fatalf("expected error containing %q, got %v", folder.ErrStaleToken.Error(), err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.confirmCalls != 1 || mock.lastToken != "stale_token_123" || mock.lastAction != "reset" {
		t.Fatalf("unexpected call state: calls=%d token=%q action=%q", mock.confirmCalls, mock.lastToken, mock.lastAction)
	}
}

func TestFolderConfirmSuccessfulControl(t *testing.T) {
	mock := &mockControlServer{
		confirmCode: http.StatusOK,
	}
	home, cleanup := startMockDaemon(t, mock)
	defer cleanup()

	// reset
	err := cmdFolderConfirm(home, []string{"--action", "reset", "--token", "tok_reset_abc"})
	if err != nil {
		t.Fatalf("unexpected error for reset: %v", err)
	}
	mock.mu.Lock()
	if mock.confirmCalls != 1 || mock.lastToken != "tok_reset_abc" || mock.lastAction != "reset" {
		t.Fatalf("unexpected call state: calls=%d token=%q action=%q", mock.confirmCalls, mock.lastToken, mock.lastAction)
	}
	mock.mu.Unlock()

	// rebind
	err = cmdFolderConfirm(home, []string{"--action", "rebind", "--token", "tok_rebind_xyz"})
	if err != nil {
		t.Fatalf("unexpected error for rebind: %v", err)
	}
	mock.mu.Lock()
	if mock.confirmCalls != 2 || mock.lastToken != "tok_rebind_xyz" || mock.lastAction != "rebind" {
		t.Fatalf("unexpected call state: calls=%d token=%q action=%q", mock.confirmCalls, mock.lastToken, mock.lastAction)
	}
	mock.mu.Unlock()
}

func TestFolderConfirmDoesNotAutoConfirm(t *testing.T) {
	mock := &mockControlServer{
		status: app.Status{
			Running: true,
			FolderError: &folder.RootError{
				Code:    "root_empty",
				Message: "folder is empty",
				Token:   "auto_token_test",
			},
		},
	}
	home, cleanup := startMockDaemon(t, mock)
	defer cleanup()

	// Calling status must NOT trigger any confirmation call.
	if err := cmdStatus(home, nil); err != nil {
		t.Fatalf("cmdStatus error: %v", err)
	}
	mock.mu.Lock()
	calls := mock.confirmCalls
	mock.mu.Unlock()
	if calls != 0 {
		t.Fatalf("expected 0 confirm calls from cmdStatus, got %d", calls)
	}
}

func TestStatusWithoutFolderError(t *testing.T) {
	mock := &mockControlServer{
		status: app.Status{
			Running:     true,
			Name:        "test-node",
			Folder:      "/tmp/sharedir-test",
			FolderError: nil,
			ScanError:   nil,
		},
	}
	home, cleanup := startMockDaemon(t, mock)
	defer cleanup()

	if err := cmdStatus(home, nil); err != nil {
		t.Fatalf("cmdStatus without error failed: %v", err)
	}
	mock.mu.Lock()
	calls := mock.confirmCalls
	mock.mu.Unlock()
	if calls != 0 {
		t.Fatalf("expected 0 confirm calls, got %d", calls)
	}
}
