package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

func testApp(t *testing.T) (*Server, http.Handler) {
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
	return s, s.Handler(fstest.MapFS{"index.html": {Data: []byte("DueRadar")}})
}
func request(h http.Handler, method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func loginCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := request(h, "POST", "/api/login", `{"password":"test-password-long-enough"}`, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	return w.Result().Cookies()[0]
}

func TestAuthenticatedAPIAndCSRF(t *testing.T) {
	_, h := testApp(t)
	for _, path := range []string{"/api/tasks", "/api/accounts", "/api/settings", "/api/bark", "/api/events", "/api/status"} {
		if w := request(h, "GET", path, "", nil, ""); w.Code != 401 {
			t.Fatalf("%s status=%d", path, w.Code)
		}
	}
	if w := request(h, "POST", "/api/login", `{"password":"test-password-long-enough"}`, nil, "https://attacker.example"); w.Code != 403 {
		t.Fatal("cross origin login accepted")
	}
	c := loginCookie(t, h)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("missing secure cookie attributes")
	}
	if w := request(h, "GET", "/api/tasks", "", c, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(h, "PUT", "/api/settings", `{}`, c, "https://attacker.example"); w.Code != 403 {
		t.Fatal("cross origin mutation accepted")
	}
	request(h, "POST", "/api/logout", `{}`, c, "")
	if w := request(h, "GET", "/api/tasks", "", c, ""); w.Code != 401 {
		t.Fatal("logged-out session remains valid")
	}
}

func TestCredentialsNeverReturnedOrStoredInState(t *testing.T) {
	s, h := testApp(t)
	c := loginCookie(t, h)
	w := request(h, "PUT", "/api/bark", `{"server":"https://api.day.app","key":"TEST_SECRET_KEY"}`, c, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request(h, "GET", "/api/bark", "", c, "")
	if strings.Contains(w.Body.String(), "TEST_SECRET_KEY") {
		t.Fatal("secret leaked")
	}
	w = request(h, "PUT", "/api/bark", `{"server":"https://api.day.app","key":""}`, c, "")
	if w.Code != 200 || s.Vault.Get().BarkKey != "TEST_SECRET_KEY" {
		t.Fatal("blank key did not retain saved key")
	}
	w = request(h, "PUT", "/api/bark", `{"server":"https://other.example","key":""}`, c, "")
	if w.Code != 400 {
		t.Fatal("saved secret reused at different host")
	}
	state, _ := json.Marshal(s.Engine.Snapshot())
	if bytes.Contains(state, []byte("TEST_SECRET_KEY")) {
		t.Fatal("secret present in task state")
	}
	if w := request(h, "POST", "/api/sync", `{}`, c, ""); w.Code != 409 {
		t.Fatal("unverified platforms reported success")
	}
}

func TestInvalidSettingsDoNotOverwrite(t *testing.T) {
	s, h := testApp(t)
	c := loginCookie(t, h)
	before := s.Engine.Snapshot().Settings
	w := request(h, "PUT", "/api/settings", `{"timezone":"made-up-zone"}`, c, "")
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	after := s.Engine.Snapshot().Settings
	if before.Timezone != after.Timezone {
		t.Fatal("invalid state saved")
	}
}
func TestLoginRateLimitAndStrictJSON(t *testing.T) {
	_, h := testApp(t)
	w := request(h, "POST", "/api/login", `{"password":"x"} {}`, nil, "")
	if w.Code != 400 {
		t.Fatal("trailing JSON accepted")
	}
	for i := 0; i < 10; i++ {
		w = request(h, "POST", "/api/login", `{"password":"wrong"}`, nil, "")
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	w = request(h, "POST", "/api/login", `{"password":"wrong"}`, nil, "")
	if w.Code != 429 {
		t.Fatal("missing rate limit")
	}
}
func TestDemoDoesNotSendOrSaveCredentials(t *testing.T) {
	s, h := testApp(t)
	s.Demo = true
	c := loginCookie(t, h)
	for _, path := range []string{"/api/bark", "/api/bark/test"} {
		method := "POST"
		if path == "/api/bark" {
			method = "PUT"
		}
		w := request(h, method, path, `{}`, c, "")
		if w.Code != 409 {
			b, _ := io.ReadAll(w.Result().Body)
			t.Fatal(string(b))
		}
	}
}
