package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ethandong16/due-radar/internal/bark"
	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

const (
	multiTestPasswordA = "synthetic-password-alpha"
	multiTestPasswordB = "synthetic-password-bravo"
)

type multiTestUser struct {
	id       string
	username string
	cookie   *http.Cookie
}

func multiTestOpen(t *testing.T, dir, bootstrap string, allowRegistration, demo bool) (*MultiServer, http.Handler) {
	t.Helper()
	m, err := OpenMulti(dir, bootstrap, true, allowRegistration, demo)
	if err != nil {
		t.Fatalf("open multi server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Errorf("cleanup multi server: %v", err)
		}
	})
	return m, m.Handler(fstest.MapFS{"index.html": {Data: []byte("DueRadar")}})
}

func multiTestRequest(h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("X-DueRadar", "1")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func multiTestJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func multiTestStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("HTTP status = %d, want %d", w.Code, want)
	}
}

func multiTestAuth(t *testing.T, h http.Handler, endpoint, username, password string, want int) multiTestUser {
	t.Helper()
	body := multiTestJSON(t, map[string]string{"username": username, "password": password})
	w := multiTestRequest(h, http.MethodPost, endpoint, body, nil)
	multiTestStatus(t, w, want)
	var response struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Authenticated || response.User.ID == "" {
		t.Fatal("authentication did not return a user")
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "dueradar_session" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.MaxAge != int(multiSessionLifetime/time.Second) {
		t.Fatal("missing or incorrect session cookie attributes")
	}
	return multiTestUser{id: response.User.ID, username: response.User.Username, cookie: cookie}
}

func multiTestRegister(t *testing.T, h http.Handler, username, password string) multiTestUser {
	t.Helper()
	return multiTestAuth(t, h, "/api/register", username, password, http.StatusCreated)
}

func multiTestLogin(t *testing.T, h http.Handler, username, password string) multiTestUser {
	t.Helper()
	return multiTestAuth(t, h, "/api/login", username, password, http.StatusOK)
}

func multiTestWorker(t *testing.T, m *MultiServer, id string) *Server {
	t.Helper()
	m.mu.RLock()
	worker := m.tenants[id]
	m.mu.RUnlock()
	if worker == nil {
		t.Fatal("missing account worker")
	}
	return worker
}

func multiTestShutdown(t *testing.T, m *MultiServer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func multiTestSessionUser(t *testing.T, h http.Handler, cookie *http.Cookie, id, username string) {
	t.Helper()
	w := multiTestRequest(h, http.MethodGet, "/api/session", "", cookie)
	multiTestStatus(t, w, http.StatusOK)
	var response struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Authenticated || response.User.ID != id || response.User.Username != username {
		t.Fatal("session routed to the wrong account")
	}
}

func TestMultiRegistrationLoginAndCSRF(t *testing.T) {
	m, h := multiTestOpen(t, t.TempDir(), "", true, false)
	registerBody := multiTestJSON(t, map[string]string{"username": "Alpha", "password": multiTestPasswordA})
	for _, tc := range []struct {
		name, origin, site, marker string
	}{
		{"missing marker", "", "", ""},
		{"foreign origin", "https://attacker.invalid", "", "1"},
		{"cross-site fetch", "", "cross-site", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(registerBody))
			r.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.marker != "" {
				r.Header.Set("X-DueRadar", tc.marker)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			multiTestStatus(t, w, http.StatusForbidden)
		})
	}
	if m.UserCount() != 0 {
		t.Fatal("rejected cross-site registration created an account")
	}
	a := multiTestRegister(t, h, "  Alpha  ", multiTestPasswordA)
	b := multiTestRegister(t, h, "Bravo", multiTestPasswordB)
	if a.id == b.id || a.username != "alpha" || b.username != "bravo" || m.UserCount() != 2 {
		t.Fatal("registration did not create distinct normalized accounts")
	}
	for _, tc := range []struct {
		username, password string
		want               int
	}{
		{"ALPHA", multiTestPasswordB, http.StatusConflict},
		{"bad name!", multiTestPasswordB, http.StatusBadRequest},
		{"short", "short", http.StatusBadRequest},
		{"long", strings.Repeat("x", maxPasswordBytes+1), http.StatusBadRequest},
	} {
		body := multiTestJSON(t, map[string]string{"username": tc.username, "password": tc.password})
		multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/register", body, nil), tc.want)
	}
	for _, tc := range []struct{ username, password string }{{"alpha", multiTestPasswordB}, {"missing", multiTestPasswordA}} {
		body := multiTestJSON(t, map[string]string{"username": tc.username, "password": tc.password})
		multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/login", body, nil), http.StatusUnauthorized)
	}
	a2 := multiTestLogin(t, h, " ALPHA ", multiTestPasswordA)
	b2 := multiTestLogin(t, h, "BRAVO", multiTestPasswordB)
	if a2.id != a.id || b2.id != b.id || a2.cookie.Value == a.cookie.Value {
		t.Fatal("login did not preserve account identity or issue a new session")
	}
	multiTestSessionUser(t, h, a.cookie, a.id, "alpha")
	multiTestSessionUser(t, h, b.cookie, b.id, "bravo")
	for _, path := range []string{"/api/session", "/api/tasks", "/api/events", "/api/settings", "/api/bark"} {
		multiTestStatus(t, multiTestRequest(h, http.MethodGet, path, "", nil), http.StatusUnauthorized)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	r.AddCookie(a.cookie)
	r.Header.Set("Origin", "https://attacker.invalid")
	r.Header.Set("X-DueRadar", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	multiTestStatus(t, w, http.StatusForbidden)
	multiTestSessionUser(t, h, a.cookie, a.id, "alpha")
}

func TestMultiLoginFailuresAreScopedByIP(t *testing.T) {
	m, h := multiTestOpen(t, t.TempDir(), "", true, false)
	a := multiTestRegister(t, h, "alpha", multiTestPasswordA)
	login := func(ip, password string) *httptest.ResponseRecorder {
		body := multiTestJSON(t, map[string]string{"username": "alpha", "password": password})
		r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body))
		r.RemoteAddr = ip + ":1234"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-DueRadar", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 10; i++ {
		multiTestStatus(t, login("192.0.2.1", multiTestPasswordB), http.StatusUnauthorized)
	}
	limited := login("192.0.2.1", multiTestPasswordB)
	multiTestStatus(t, limited, http.StatusTooManyRequests)
	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("rate-limited response has no retry guidance")
	}
	multiTestStatus(t, login("192.0.2.2", multiTestPasswordB), http.StatusUnauthorized)
	// The budget is enforced before password verification; even a correct
	// password must wait for the short shared-IP retry interval.
	multiTestStatus(t, login("192.0.2.1", multiTestPasswordA), http.StatusTooManyRequests)
	m.mu.Lock()
	attempt := m.attempts["192.0.2.1"]
	attempt.NextAllowed = time.Now().Add(-time.Second)
	m.attempts["192.0.2.1"] = attempt
	m.mu.Unlock()
	valid := login("192.0.2.1", multiTestPasswordA)
	multiTestStatus(t, valid, http.StatusOK)
	var cookie *http.Cookie
	for _, c := range valid.Result().Cookies() {
		if c.Name == "dueradar_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("valid login after rate limit did not issue a session")
	}
	multiTestSessionUser(t, h, cookie, a.id, "alpha")
	multiTestStatus(t, login("192.0.2.1", multiTestPasswordB), http.StatusUnauthorized)
}

func TestMultiFirstRegistrationWhenClosed(t *testing.T) {
	dir := t.TempDir()
	m, h := multiTestOpen(t, dir, "", false, false)
	start := make(chan struct{})
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			<-start
			body, _ := json.Marshal(map[string]string{"username": fmt.Sprintf("user-%d", i), "password": multiTestPasswordA})
			results <- multiTestRequest(h, http.MethodPost, "/api/register", string(body), nil).Code
		}(i)
	}
	close(start)
	counts := map[int]int{<-results: 1}
	counts[<-results]++
	if counts[http.StatusCreated] != 1 || counts[http.StatusForbidden] != 1 || m.UserCount() != 1 {
		t.Fatalf("first-registration results = %v, users = %d", counts, m.UserCount())
	}
	body := multiTestJSON(t, map[string]string{"username": "after-first", "password": multiTestPasswordA})
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/register", body, nil), http.StatusForbidden)
	multiTestShutdown(t, m)
	restarted, h2 := multiTestOpen(t, dir, "", false, false)
	multiTestStatus(t, multiTestRequest(h2, http.MethodPost, "/api/register", body, nil), http.StatusForbidden)
	if restarted.UserCount() != 1 {
		t.Fatal("disabled registration changed the persisted account count")
	}
}

func TestMultiConcurrentRegistrationThenClose(t *testing.T) {
	dir := t.TempDir()
	m, h := multiTestOpen(t, dir, "", true, false)
	start := make(chan struct{})
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			<-start
			body, _ := json.Marshal(map[string]string{"username": fmt.Sprintf("parallel-%d", i), "password": multiTestPasswordA})
			results <- multiTestRequest(h, http.MethodPost, "/api/register", string(body), nil).Code
		}(i)
	}
	close(start)
	for i := 0; i < 2; i++ {
		if status := <-results; status != http.StatusCreated {
			t.Fatalf("concurrent registration status = %d", status)
		}
	}
	if m.UserCount() != 2 {
		t.Fatal("concurrent registrations did not create two accounts")
	}
	multiTestShutdown(t, m)
	closed, h2 := multiTestOpen(t, dir, "", false, false)
	body := multiTestJSON(t, map[string]string{"username": "blocked-user", "password": multiTestPasswordB})
	multiTestStatus(t, multiTestRequest(h2, http.MethodPost, "/api/register", body, nil), http.StatusForbidden)
	if closed.UserCount() != 2 {
		t.Fatal("closed registration changed existing accounts")
	}
	multiTestLogin(t, h2, "parallel-0", multiTestPasswordA)
	multiTestLogin(t, h2, "parallel-1", multiTestPasswordA)
}

func TestMultiAccountHeaderPinsRequestsAndLogout(t *testing.T) {
	m, h := multiTestOpen(t, t.TempDir(), "", true, false)
	a := multiTestRegister(t, h, "alpha", multiTestPasswordA)
	b := multiTestRegister(t, h, "bravo", multiTestPasswordB)
	wa := multiTestWorker(t, m, a.id)
	before := wa.Engine.Snapshot().Settings
	updated := before
	updated.Timezone = "UTC"

	send := func(method, path, body string, cookie *http.Cookie, accountID string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if method != http.MethodGet {
			r.Header.Set("X-DueRadar", "1")
			r.Header.Set("Content-Type", "application/json")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if accountID != "" {
			r.Header.Set("X-DueRadar-Account", accountID)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	multiTestStatus(t, send(http.MethodGet, "/api/settings", "", a.cookie, a.id), http.StatusOK)
	multiTestStatus(t, send(http.MethodGet, "/api/settings", "", a.cookie, b.id), http.StatusConflict)
	multiTestStatus(t, send(http.MethodGet, "/api/settings", "", nil, a.id), http.StatusUnauthorized)
	multiTestStatus(t, send(http.MethodPut, "/api/settings", multiTestJSON(t, updated), a.cookie, b.id), http.StatusConflict)
	if wa.Engine.Snapshot().Settings.Timezone != before.Timezone {
		t.Fatal("stale account header changed the wrong account")
	}
	multiTestStatus(t, send(http.MethodPut, "/api/settings", multiTestJSON(t, updated), a.cookie, a.id), http.StatusOK)
	if wa.Engine.Snapshot().Settings.Timezone != "UTC" {
		t.Fatal("matching account header did not reach the account")
	}
	multiTestStatus(t, send(http.MethodPost, "/api/logout", "", a.cookie, b.id), http.StatusConflict)
	multiTestSessionUser(t, h, a.cookie, a.id, "alpha")
	multiTestSessionUser(t, h, b.cookie, b.id, "bravo")
	multiTestStatus(t, send(http.MethodPost, "/api/logout", "", a.cookie, a.id), http.StatusOK)
	multiTestStatus(t, multiTestRequest(h, http.MethodGet, "/api/session", "", a.cookie), http.StatusUnauthorized)
	multiTestSessionUser(t, h, b.cookie, b.id, "bravo")
}

func TestMultiTenantDataIsolation(t *testing.T) {
	dir := t.TempDir()
	m, h := multiTestOpen(t, dir, "", true, false)
	a := multiTestRegister(t, h, "alpha", multiTestPasswordA)
	b := multiTestRegister(t, h, "bravo", multiTestPasswordB)
	wa, wb := multiTestWorker(t, m, a.id), multiTestWorker(t, m, b.id)
	m.mu.Lock()
	m.allowRegistration = false
	m.mu.Unlock()
	registration := multiTestJSON(t, map[string]string{"username": "charlie", "password": multiTestPasswordA})
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/register", registration, nil), http.StatusForbidden)
	if m.UserCount() != 2 {
		t.Fatal("closed registration changed the account count")
	}
	if wa == wb || wa.Engine == wb.Engine || wa.Vault == wb.Vault {
		t.Fatal("accounts share a worker, engine, or vault")
	}
	now := time.Now()
	end := now.Add(48 * time.Hour)
	for _, tc := range []struct {
		worker *Server
		title  string
	}{{wa, "Alpha task"}, {wb, "Bravo task"}} {
		if err := tc.worker.Engine.Sync(core.PlatformRain, []core.Task{{ID: "same-id", Title: tc.title, Kind: "homework", Status: "pending", EndAt: &end}}, now); err != nil {
			t.Fatal(err)
		}
	}
	settingsA := core.DefaultSettings()
	settingsA.Timezone, settingsA.QuietEnabled = "UTC", false
	w := multiTestRequest(h, http.MethodPut, "/api/settings", multiTestJSON(t, settingsA), a.cookie)
	multiTestStatus(t, w, http.StatusOK)

	for _, tc := range []struct {
		user         multiTestUser
		worker       *Server
		server, key  string
		rain, zhidao string
	}{
		{a, wa, "https://alpha.example", "synthetic-bark-alpha", "synthetic-rain-alpha", "synthetic-zhidao-alpha"},
		{b, wb, "https://bravo.example", "synthetic-bark-bravo", "synthetic-rain-bravo", "synthetic-zhidao-bravo"},
	} {
		body := multiTestJSON(t, map[string]string{"server": tc.server, "key": tc.key})
		multiTestStatus(t, multiTestRequest(h, http.MethodPut, "/api/bark", body, tc.user.cookie), http.StatusOK)
		if err := tc.worker.Vault.Update(func(c *vault.Config) error {
			c.RainCookie, c.ZhidaoSession = tc.rain, tc.zhidao
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		called := false
		tc.worker.sendBark = func(_ context.Context, server, key, _, _ string, _ bark.Options) error {
			called = true
			if server != tc.server || key != tc.key {
				return errors.New("wrong account notification configuration")
			}
			return nil
		}
		multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/bark/test", "", tc.user.cookie), http.StatusOK)
		if !called {
			t.Fatal("notification was not sent through the account's transport")
		}
	}
	assertViews := func(h http.Handler) {
		t.Helper()
		for _, tc := range []struct {
			user         multiTestUser
			title, other string
			zone, server string
		}{
			{a, "Alpha task", "Bravo task", "UTC", "https://alpha.example"},
			{b, "Bravo task", "Alpha task", "Asia/Shanghai", "https://bravo.example"},
		} {
			for _, path := range []string{"/api/tasks", "/api/events"} {
				w := multiTestRequest(h, http.MethodGet, path, "", tc.user.cookie)
				multiTestStatus(t, w, http.StatusOK)
				if !strings.Contains(w.Body.String(), tc.title) || strings.Contains(w.Body.String(), tc.other) {
					t.Fatalf("%s leaked or omitted account data", path)
				}
			}
			w := multiTestRequest(h, http.MethodGet, "/api/settings", "", tc.user.cookie)
			multiTestStatus(t, w, http.StatusOK)
			var got core.Settings
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Timezone != tc.zone {
				t.Fatal("settings routed to the wrong account", err)
			}
			w = multiTestRequest(h, http.MethodGet, "/api/bark", "", tc.user.cookie)
			multiTestStatus(t, w, http.StatusOK)
			if !strings.Contains(w.Body.String(), tc.server) || strings.Contains(w.Body.String(), "synthetic-bark-") || strings.Contains(w.Body.String(), "synthetic-rain-") || strings.Contains(w.Body.String(), "synthetic-zhidao-") {
				t.Fatal("notification settings leaked credentials or crossed accounts")
			}
		}
	}
	assertViews(h)
	for _, id := range []string{a.id, b.id} {
		ciphertext, err := os.ReadFile(filepath.Join(dir, "users", id, "secrets.enc"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ciphertext, []byte("synthetic-bark-")) || bytes.Contains(ciphertext, []byte("synthetic-rain-")) {
			t.Fatal("plaintext credential present in encrypted vault")
		}
	}
	multiTestShutdown(t, m)
	restarted, restartedHandler := multiTestOpen(t, dir, "", false, false)
	multiTestStatus(t, multiTestRequest(restartedHandler, http.MethodPost, "/api/register", registration, nil), http.StatusForbidden)
	assertViews(restartedHandler)
	if gotA, gotB := multiTestWorker(t, restarted, a.id).Vault.Get(), multiTestWorker(t, restarted, b.id).Vault.Get(); gotA.BarkKey != "synthetic-bark-alpha" || gotA.RainCookie != "synthetic-rain-alpha" || gotA.ZhidaoSession != "synthetic-zhidao-alpha" || gotB.BarkKey != "synthetic-bark-bravo" || gotB.RainCookie != "synthetic-rain-bravo" || gotB.ZhidaoSession != "synthetic-zhidao-bravo" {
		t.Fatal("vault contents did not survive separately")
	}
}

func TestMultiLegacyOwnerMigration(t *testing.T) {
	dir := t.TempDir()
	engine, err := core.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	end := now.Add(48 * time.Hour)
	if err := engine.Sync(core.PlatformRain, []core.Task{{ID: "legacy", Title: "Legacy task", Kind: "homework", Status: "pending", EndAt: &end}}, now); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Update(func(config *vault.Config) error {
		config.BarkURL, config.BarkKey = "https://legacy.example", "synthetic-legacy-bark"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := OpenMulti(dir, "", true, true, false); err == nil {
		multiTestShutdown(t, unexpected)
		t.Fatal("legacy data was opened without a bootstrap password")
	}
	m, h := multiTestOpen(t, dir, multiTestPasswordA, true, false)
	owner := multiTestLogin(t, h, "owner", multiTestPasswordA)
	if owner.id != "owner" || m.UserCount() != 1 {
		t.Fatal("legacy owner was not bootstrapped")
	}
	w := multiTestRequest(h, http.MethodGet, "/api/tasks", "", owner.cookie)
	multiTestStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), "Legacy task") || multiTestWorker(t, m, owner.id).Vault.Get().BarkKey != "synthetic-legacy-bark" {
		t.Fatal("legacy data was not attached to the owner")
	}
	other := multiTestRegister(t, h, "bravo", multiTestPasswordB)
	w = multiTestRequest(h, http.MethodGet, "/api/tasks", "", other.cookie)
	multiTestStatus(t, w, http.StatusOK)
	if strings.Contains(w.Body.String(), "Legacy task") || multiTestWorker(t, m, other.id).Vault.Get().BarkKey != "" {
		t.Fatal("legacy owner data was exposed to the registered user")
	}
	multiTestShutdown(t, m)
	restarted, h2 := multiTestOpen(t, dir, "", false, false)
	multiTestSessionUser(t, h2, owner.cookie, owner.id, "owner")
	ownerAgain := multiTestLogin(t, h2, "owner", multiTestPasswordA)
	if ownerAgain.id != owner.id {
		t.Fatal("bootstrap password did not survive restart")
	}
	if multiTestWorker(t, restarted, owner.id).Vault.Get().BarkKey != "synthetic-legacy-bark" {
		t.Fatal("legacy owner vault did not survive restart")
	}
	w = multiTestRequest(h2, http.MethodGet, "/api/tasks", "", owner.cookie)
	multiTestStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), "Legacy task") {
		t.Fatal("legacy owner task did not survive restart")
	}
}

func TestMultiLegacyCiphertextWithoutKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	credentials, err := vault.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.Update(func(config *vault.Config) error {
		config.BarkURL, config.BarkKey = "https://legacy.example", "synthetic-legacy-bark"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	keyPath, ciphertextPath := filepath.Join(dir, "master.key"), filepath.Join(dir, "secrets.enc")
	before, err := os.ReadFile(ciphertextPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := OpenMulti(dir, multiTestPasswordA, true, true, false); err == nil {
		multiTestShutdown(t, unexpected)
		t.Fatal("legacy ciphertext without its key opened successfully")
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a replacement key was generated for existing ciphertext")
	}
	after, err := os.ReadFile(ciphertextPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("legacy ciphertext changed after the failed open")
	}
}

func TestMultiMissingTenantFilesFailClosed(t *testing.T) {
	for _, name := range []string{"state.json", "master.key"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			m, h := multiTestOpen(t, dir, "", true, false)
			user := multiTestRegister(t, h, "alpha", multiTestPasswordA)
			multiTestShutdown(t, m)
			missing := filepath.Join(dir, "users", user.id, name)
			if err := os.Remove(missing); err != nil {
				t.Fatal(err)
			}
			if unexpected, err := OpenMulti(dir, "", true, true, false); err == nil {
				multiTestShutdown(t, unexpected)
				t.Fatal("missing tenant data was silently recreated")
			}
			if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("opening a broken account recreated its missing data")
			}
		})
	}
}

func TestMultiSessionsSurviveRestartAndLogout(t *testing.T) {
	dir := t.TempDir()
	m, h := multiTestOpen(t, dir, "", true, false)
	a := multiTestRegister(t, h, "alpha", multiTestPasswordA)
	b := multiTestRegister(t, h, "bravo", multiTestPasswordB)
	path := filepath.Join(dir, "user-sessions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(a.cookie.Value)) || bytes.Contains(data, []byte(b.cookie.Value)) {
		t.Fatal("raw session token persisted")
	}
	var file multiSessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Sessions) != 2 {
		t.Fatal("registration sessions were not persisted")
	}
	multiTestShutdown(t, m)
	file.Sessions = append(file.Sessions,
		multiStoredSession{Key: multiSessionKey("expired-fixture"), UserID: a.id, Expires: time.Now().Add(-time.Hour)},
		multiStoredSession{Key: multiSessionKey("unknown-user-fixture"), UserID: strings.Repeat("a", 32), Expires: time.Now().Add(time.Hour)},
		multiStoredSession{Key: "bad-key", UserID: a.id, Expires: time.Now().Add(time.Hour)},
	)
	if err := os.WriteFile(path, []byte(multiTestJSON(t, file)), 0600); err != nil {
		t.Fatal(err)
	}
	m2, h2 := multiTestOpen(t, dir, "", true, false)
	multiTestSessionUser(t, h2, a.cookie, a.id, "alpha")
	multiTestSessionUser(t, h2, b.cookie, b.id, "bravo")
	for _, token := range []string{"expired-fixture", "unknown-user-fixture"} {
		multiTestStatus(t, multiTestRequest(h2, http.MethodGet, "/api/session", "", &http.Cookie{Name: "dueradar_session", Value: token}), http.StatusUnauthorized)
	}
	multiTestStatus(t, multiTestRequest(h2, http.MethodPost, "/api/logout", "", a.cookie), http.StatusOK)
	multiTestStatus(t, multiTestRequest(h2, http.MethodGet, "/api/session", "", a.cookie), http.StatusUnauthorized)
	multiTestSessionUser(t, h2, b.cookie, b.id, "bravo")
	multiTestShutdown(t, m2)
	m3, h3 := multiTestOpen(t, dir, "", true, false)
	multiTestStatus(t, multiTestRequest(h3, http.MethodGet, "/api/session", "", a.cookie), http.StatusUnauthorized)
	multiTestSessionUser(t, h3, b.cookie, b.id, "bravo")
	multiTestShutdown(t, m3)
	if err := os.WriteFile(path, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	_, h4 := multiTestOpen(t, dir, "", true, false)
	multiTestStatus(t, multiTestRequest(h4, http.MethodGet, "/api/session", "", b.cookie), http.StatusUnauthorized)
	b2 := multiTestLogin(t, h4, "bravo", multiTestPasswordB)
	if b2.id != b.id {
		t.Fatal("corrupt disposable sessions changed account identity")
	}
}

func multiTestCreateAssist(t *testing.T, h http.Handler, cookie *http.Cookie) assistCreated {
	t.Helper()
	w := multiTestRequest(h, http.MethodPost, "/api/accounts/rain/assist", "", cookie)
	multiTestStatus(t, w, http.StatusOK)
	var flow assistCreated
	if err := json.Unmarshal(w.Body.Bytes(), &flow); err != nil || len(flow.ID) != 64 || len(flow.Code) != 64 {
		t.Fatal("invalid assist response", err)
	}
	return flow
}

func multiTestConnect(t *testing.T, h http.Handler, flow assistCreated, cookie *http.Cookie) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/accounts/rain/assist/"+flow.ID+"/connect", nil)
	r.Header.Set("Authorization", "Bearer "+flow.Code)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	multiTestStatus(t, w, http.StatusOK)
	var result struct {
		Token string `json:"upload_token"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Token) != 64 || result.State != "awaiting_user" {
		t.Fatal("invalid assist connect response", err)
	}
	return result.Token
}

func TestMultiAssistOwnershipAndLogout(t *testing.T) {
	m, h := multiTestOpen(t, t.TempDir(), "", true, false)
	a := multiTestRegister(t, h, "alpha", multiTestPasswordA)
	b := multiTestRegister(t, h, "bravo", multiTestPasswordB)
	a2 := multiTestLogin(t, h, "alpha", multiTestPasswordA)
	wa, wb := multiTestWorker(t, m, a.id), multiTestWorker(t, m, b.id)
	wa.newRain = func(string) rainAdapter {
		return fakeRainAdapter{tasks: func(context.Context) ([]core.Task, error) { return nil, errors.New("synthetic task failure") }}
	}
	flowA := multiTestCreateAssist(t, h, a.cookie)
	flowB := multiTestCreateAssist(t, h, b.cookie)
	baseA, baseB := "/api/accounts/rain/assist/"+flowA.ID, "/api/accounts/rain/assist/"+flowB.ID
	for _, cookie := range []*http.Cookie{a2.cookie, b.cookie} {
		multiTestStatus(t, multiTestRequest(h, http.MethodGet, baseA, "", cookie), http.StatusNotFound)
		multiTestStatus(t, multiTestRequest(h, http.MethodPost, baseA+"/cancel", "", cookie), http.StatusNotFound)
	}
	multiTestStatus(t, multiTestRequest(h, http.MethodGet, baseA, "", nil), http.StatusUnauthorized)
	multiTestStatus(t, helperRequest(h, http.MethodPost, baseA+"/connect", "", flowB.Code), http.StatusUnauthorized)
	multiTestStatus(t, helperRequest(h, http.MethodPost, baseB+"/connect", "", flowA.Code), http.StatusUnauthorized)
	// Native capability routing uses the flow ID and bearer, not an unrelated browser cookie.
	tokenA := multiTestConnect(t, h, flowA, b.cookie)
	tokenB := multiTestConnect(t, h, flowB, nil)
	multiTestStatus(t, helperRequest(h, http.MethodPost, baseA+"/connect", "", flowA.Code), http.StatusUnauthorized)
	multiTestStatus(t, helperRequest(h, http.MethodGet, baseA+"/helper", "", tokenB), http.StatusUnauthorized)
	multiTestStatus(t, helperRequest(h, http.MethodGet, baseB+"/helper", "", tokenA), http.StatusUnauthorized)
	r := httptest.NewRequest(http.MethodGet, baseA+"/helper", nil)
	r.Header.Set("Authorization", "Bearer "+tokenA)
	r.Header.Set("Origin", "http://example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	multiTestStatus(t, w, http.StatusForbidden)
	multiTestStatus(t, helperRequest(h, http.MethodPost, baseA+"/session", assistBrowserSession, tokenA), http.StatusAccepted)
	eventually(t, func() bool { return assistState(wa, flowA.ID) == "logged_in" })
	if !strings.Contains(wa.Vault.Get().RainCookie, "synthetic-secret") || wb.Vault.Get().RainCookie != "" {
		t.Fatal("validated assist credentials crossed accounts")
	}
	activeA := multiTestCreateAssist(t, h, a.cookie)
	activeBase := "/api/accounts/rain/assist/" + activeA.ID
	activeToken := multiTestConnect(t, h, activeA, nil)
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/logout", "", b.cookie), http.StatusOK)
	multiTestStatus(t, helperRequest(h, http.MethodGet, baseB+"/helper", "", tokenB), http.StatusUnauthorized)
	multiTestStatus(t, helperRequest(h, http.MethodGet, activeBase+"/helper", "", activeToken), http.StatusOK)
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/logout", "", a2.cookie), http.StatusOK)
	multiTestStatus(t, helperRequest(h, http.MethodGet, activeBase+"/helper", "", activeToken), http.StatusOK)
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/logout", "", a.cookie), http.StatusOK)
	multiTestStatus(t, helperRequest(h, http.MethodGet, activeBase+"/helper", "", activeToken), http.StatusUnauthorized)
	if assistState(wa, activeA.ID) != "canceled" || assistState(wb, flowB.ID) != "canceled" {
		t.Fatal("logout did not cancel its own active assist")
	}
}

func TestMultiAssistCreationRechecksLogout(t *testing.T) {
	m, h := multiTestOpen(t, t.TempDir(), "", true, false)
	a := multiTestRegister(t, h, "alpha", multiTestPasswordA)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRequest()
	m.mu.Lock()
	original, ok := m.tenantHandlerLocked(a.id)
	if !ok {
		m.mu.Unlock()
		t.Fatal("missing tenant handler")
	}
	m.handlers[a.id] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/accounts/rain/assist" {
			close(entered)
			<-release
		}
		original.ServeHTTP(w, r)
	})
	m.mu.Unlock()
	result := make(chan int, 1)
	go func() { result <- multiTestRequest(h, http.MethodPost, "/api/accounts/rain/assist", "", a.cookie).Code }()
	await(t, entered)
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/logout", "", a.cookie), http.StatusOK)
	releaseRequest()
	select {
	case status := <-result:
		if status != http.StatusUnauthorized {
			t.Fatalf("assist created after logout: status %d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("assist request did not finish")
	}
}

func multiTestWorkerRunning(worker *Server) bool {
	worker.lifeMu.Lock()
	defer worker.lifeMu.Unlock()
	return worker.runStarted
}

func TestMultiWorkerLifecycle(t *testing.T) {
	dir := t.TempDir()
	m, h := multiTestOpen(t, dir, "", true, true)
	a := multiTestRegister(t, h, "alpha", multiTestPasswordA)
	wa := multiTestWorker(t, m, a.id)
	if multiTestWorkerRunning(wa) || len(wa.Engine.Snapshot().Tasks) != 0 {
		t.Fatal("worker ran before MultiServer.Run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { defer close(runDone); m.Run(ctx) }()
	eventually(t, func() bool { return multiTestWorkerRunning(wa) && len(wa.Engine.Snapshot().Tasks) > 0 })
	b := multiTestRegister(t, h, "bravo", multiTestPasswordB)
	wb := multiTestWorker(t, m, b.id)
	eventually(t, func() bool { return multiTestWorkerRunning(wb) && len(wb.Engine.Snapshot().Tasks) > 0 })
	secondRunDone := make(chan struct{})
	go func() { defer close(secondRunDone); m.Run(ctx) }()
	await(t, secondRunDone)

	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseJob := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseJob()
	if !wb.startJob(func(ctx context.Context) { close(started); <-ctx.Done(); close(canceled); <-release }) {
		t.Fatal("could not create controlled worker job")
	}
	await(t, started)
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		shutdownDone <- m.Shutdown(shutdownCtx)
	}()
	await(t, canceled)
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before worker cleanup: %v", err)
	default:
	}
	releaseJob()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	await(t, runDone)
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/sync", "", a.cookie), http.StatusServiceUnavailable)
	body := multiTestJSON(t, map[string]string{"username": "charlie", "password": multiTestPasswordA})
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/register", body, nil), http.StatusServiceUnavailable)

	restarted, _ := multiTestOpen(t, dir, "", true, true)
	wa2, wb2 := multiTestWorker(t, restarted, a.id), multiTestWorker(t, restarted, b.id)
	if multiTestWorkerRunning(wa2) || multiTestWorkerRunning(wb2) {
		t.Fatal("reopened workers ran before Run")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	restartedDone := make(chan struct{})
	go func() { defer close(restartedDone); restarted.Run(ctx2) }()
	eventually(t, func() bool { return multiTestWorkerRunning(wa2) && multiTestWorkerRunning(wb2) })
	cancel2()
	await(t, restartedDone)
}

func TestMultiShutdownWithoutRunWaitsForOwnedJobs(t *testing.T) {
	m, h := multiTestOpen(t, t.TempDir(), "", true, false)
	a := multiTestRegister(t, h, "alpha", multiTestPasswordA)
	worker := multiTestWorker(t, m, a.id)
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseJob := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseJob()
	if !worker.startJob(func(ctx context.Context) { close(started); <-ctx.Done(); close(canceled); <-release }) {
		t.Fatal("could not start controlled job")
	}
	await(t, started)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown did not wait for the job: %v", err)
	}
	await(t, canceled)
	releaseJob()
	multiTestShutdown(t, m)
	if worker.startJob(func(context.Context) {}) {
		t.Fatal("worker accepted a job after shutdown")
	}
}

type multiTestSession struct {
	ID      string `json:"id"`
	Current bool   `json:"current"`
}

func multiTestSessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "dueradar_session" {
			return cookie
		}
	}
	t.Fatal("response did not set a session cookie")
	return nil
}

func multiTestSessions(t *testing.T, h http.Handler, cookie *http.Cookie) []multiTestSession {
	t.Helper()
	w := multiTestRequest(h, http.MethodGet, "/api/account/sessions", "", cookie)
	multiTestStatus(t, w, http.StatusOK)
	var sessions []multiTestSession
	if err := json.Unmarshal(w.Body.Bytes(), &sessions); err != nil {
		t.Fatal(err)
	}
	return sessions
}

func TestPasswordChangeRevokesEveryOtherSession(t *testing.T) {
	_, h := multiTestOpen(t, t.TempDir(), "", true, false)
	first := multiTestRegister(t, h, "alice", multiTestPasswordA)
	second := multiTestLogin(t, h, "alice", multiTestPasswordA)

	sessions := multiTestSessions(t, h, first.cookie)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v, want two", sessions)
	}
	current := 0
	for _, session := range sessions {
		if session.Current {
			current++
		}
	}
	if current != 1 || sessions[0].ID == "" {
		t.Fatalf("session view = %+v", sessions)
	}

	change := func(cookie *http.Cookie, from, to string) *httptest.ResponseRecorder {
		body := multiTestJSON(t, map[string]string{"current_password": from, "new_password": to})
		return multiTestRequest(h, http.MethodPost, "/api/account/password", body, cookie)
	}
	multiTestStatus(t, change(first.cookie, "wrong-current-password", multiTestPasswordB), http.StatusForbidden)

	w := change(first.cookie, multiTestPasswordA, multiTestPasswordB)
	multiTestStatus(t, w, http.StatusOK)
	fresh := multiTestSessionCookie(t, w)
	if fresh.Value == first.cookie.Value || fresh.Value == second.cookie.Value {
		t.Fatal("password change did not issue a new session")
	}
	for _, cookie := range []*http.Cookie{first.cookie, second.cookie} {
		multiTestStatus(t, multiTestRequest(h, http.MethodGet, "/api/session", "", cookie), http.StatusUnauthorized)
	}
	multiTestStatus(t, multiTestRequest(h, http.MethodGet, "/api/session", "", fresh), http.StatusOK)
	if sessions = multiTestSessions(t, h, fresh); len(sessions) != 1 || !sessions[0].Current {
		t.Fatalf("post-change sessions = %+v", sessions)
	}
	body := multiTestJSON(t, map[string]string{"username": "alice", "password": multiTestPasswordA})
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/login", body, nil), http.StatusUnauthorized)
	multiTestLogin(t, h, "alice", multiTestPasswordB)
}

func TestPasswordChangeThrottlesWrongCurrentPassword(t *testing.T) {
	_, h := multiTestOpen(t, t.TempDir(), "", true, false)
	user := multiTestRegister(t, h, "alice", multiTestPasswordA)
	body := multiTestJSON(t, map[string]string{"current_password": "wrong-current-password", "new_password": multiTestPasswordB})
	limited := false
	for i := 0; i < 12; i++ {
		w := multiTestRequest(h, http.MethodPost, "/api/account/password", body, user.cookie)
		if w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
		multiTestStatus(t, w, http.StatusForbidden)
	}
	if !limited {
		t.Fatal("repeated wrong current passwords were not rate limited")
	}
	// The limiter applies before verification, so even the correct password
	// waits out the retry interval instead of bypassing the throttle.
	body = multiTestJSON(t, map[string]string{"current_password": multiTestPasswordA, "new_password": multiTestPasswordB})
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/account/password", body, user.cookie), http.StatusTooManyRequests)
}

func TestSessionRevokeStaysInsideTheAccount(t *testing.T) {
	_, h := multiTestOpen(t, t.TempDir(), "", true, false)
	aliceFirst := multiTestRegister(t, h, "alice", multiTestPasswordA)
	aliceSecond := multiTestLogin(t, h, "alice", multiTestPasswordA)
	bob := multiTestRegister(t, h, "bob", multiTestPasswordB)

	bobSessions := multiTestSessions(t, h, bob.cookie)
	if len(bobSessions) != 1 {
		t.Fatalf("bob sessions = %+v", bobSessions)
	}
	body := multiTestJSON(t, map[string]string{"id": bobSessions[0].ID})
	multiTestStatus(t, multiTestRequest(h, http.MethodPost, "/api/account/sessions/revoke", body, aliceFirst.cookie), http.StatusNotFound)
	multiTestStatus(t, multiTestRequest(h, http.MethodGet, "/api/session", "", bob.cookie), http.StatusOK)

	var other string
	for _, session := range multiTestSessions(t, h, aliceFirst.cookie) {
		if !session.Current {
			other = session.ID
		}
	}
	if other == "" {
		t.Fatal("missing second alice session")
	}
	body = multiTestJSON(t, map[string]string{"id": other})
	w := multiTestRequest(h, http.MethodPost, "/api/account/sessions/revoke", body, aliceFirst.cookie)
	multiTestStatus(t, w, http.StatusOK)
	multiTestStatus(t, multiTestRequest(h, http.MethodGet, "/api/session", "", aliceSecond.cookie), http.StatusUnauthorized)
	multiTestStatus(t, multiTestRequest(h, http.MethodGet, "/api/session", "", aliceFirst.cookie), http.StatusOK)

	w = multiTestRequest(h, http.MethodPost, "/api/account/sessions/revoke-others", "", aliceFirst.cookie)
	multiTestStatus(t, w, http.StatusOK)
	var result struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 {
		t.Fatalf("removed = %d, want 0", result.Removed)
	}
	multiTestStatus(t, multiTestRequest(h, http.MethodGet, "/api/session", "", aliceFirst.cookie), http.StatusOK)
}
