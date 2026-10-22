package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

func sessionApp(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	e, err := core.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(e, v, "test-password-long-enough", false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s, s.Handler(fstest.MapFS{"index.html": {Data: []byte("DueRadar")}}), dir
}

func TestAdminSessionLastsFifteenDaysAndSurvivesRestart(t *testing.T) {
	s, h, dir := sessionApp(t)
	if err := s.EnableSessionPersistence(dir); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, h)
	if cookie.MaxAge != int(sessionLifetime/time.Second) {
		t.Fatalf("cookie max age = %d", cookie.MaxAge)
	}
	s.mu.Lock()
	entry, ok := s.sessions[sessionKey(cookie.Value)]
	s.mu.Unlock()
	if !ok {
		t.Fatal("session missing after login")
	}
	if remaining := time.Until(entry.expires); remaining < 14*24*time.Hour || remaining > 16*24*time.Hour {
		t.Fatalf("session remaining = %v", remaining)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cookie.Value) {
		t.Fatal("raw session token written to disk")
	}

	restarted, err := New(s.Engine, s.Vault, "test-password-long-enough", false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		restarted.Shutdown(ctx)
	})
	if err := restarted.EnableSessionPersistence(dir); err != nil {
		t.Fatal(err)
	}
	restartedHandler := restarted.Handler(fstest.MapFS{"index.html": {Data: []byte("DueRadar")}})
	if w := request(restartedHandler, "GET", "/api/session", "", cookie, ""); w.Code != 200 {
		t.Fatalf("session did not survive restart: %d %s", w.Code, w.Body.String())
	}
	if w := request(restartedHandler, "POST", "/api/logout", `{}`, cookie, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}

	again, err := New(s.Engine, s.Vault, "test-password-long-enough", false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		again.Shutdown(ctx)
	})
	if err := again.EnableSessionPersistence(dir); err != nil {
		t.Fatal(err)
	}
	againHandler := again.Handler(fstest.MapFS{"index.html": {Data: []byte("DueRadar")}})
	if w := request(againHandler, "GET", "/api/session", "", cookie, ""); w.Code != 401 {
		t.Fatalf("logged-out session persisted: %d", w.Code)
	}
}

func TestSessionCapEvictsOldestInsteadOfBlockingLogin(t *testing.T) {
	s, h, _ := sessionApp(t)
	now := time.Now()
	oldest := sessionKey("oldest")
	s.mu.Lock()
	s.sessions[oldest] = session{expires: now.Add(time.Minute)}
	for i := 1; i < 128; i++ {
		s.sessions[sessionKey(fmt.Sprintf("filler-%d", i))] = session{expires: now.Add(time.Duration(i+1) * time.Minute)}
	}
	s.mu.Unlock()

	loginCookie(t, h)

	s.mu.Lock()
	count := len(s.sessions)
	_, oldestRetained := s.sessions[oldest]
	s.mu.Unlock()
	if count != 128 {
		t.Fatalf("session count = %d", count)
	}
	if oldestRetained {
		t.Fatal("oldest session was not evicted")
	}
}

func TestStoredSessionsDropExpiredAndIgnoreCorruption(t *testing.T) {
	dir := t.TempDir()
	expired := sessionFile{Version: 1, Sessions: []storedSession{{Key: sessionKey("expired-token"), Expires: time.Now().Add(-time.Hour)}}}
	data, err := json.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, h, _ := sessionApp(t)
	if err := s.EnableSessionPersistence(dir); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	count := len(s.sessions)
	s.mu.Unlock()
	if count != 0 {
		t.Fatalf("expired session loaded: %d", count)
	}
	if w := request(h, "GET", "/api/session", "", &http.Cookie{Name: "dueradar_session", Value: "expired-token"}, ""); w.Code != 401 {
		t.Fatalf("expired token accepted: %d", w.Code)
	}

	corrupt, _, corruptDir := sessionApp(t)
	if err := os.WriteFile(filepath.Join(corruptDir, "sessions.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := corrupt.EnableSessionPersistence(corruptDir); err != nil {
		t.Fatalf("corrupt file blocked startup: %v", err)
	}
}
