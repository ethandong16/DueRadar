package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ethandong16/due-radar/internal/bark"
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

func TestSecurityHeadersDisableUnneededBrowserCapabilities(t *testing.T) {
	_, h := testApp(t)
	w := request(h, "GET", "/healthz", "", nil, "")
	if got := w.Header().Get("Permissions-Policy"); got != "camera=(), microphone=(), geolocation=(), payment=()" {
		t.Fatalf("permissions policy=%q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Fatalf("csp missing frame protection: %q", got)
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

func TestBarkTestDoesNotExposeTransportError(t *testing.T) {
	s, h := testApp(t)
	c := loginCookie(t, h)
	if err := s.Vault.Update(func(config *vault.Config) error {
		config.BarkURL, config.BarkKey = "https://api.day.app", "test-key"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const secret = "https://api.day.app/push?device_key=private-test-key"
	s.sendBark = func(context.Context, string, string, string, string, bark.Options) error {
		return errors.New(secret)
	}
	if w := request(h, "POST", "/api/bark/test", "", c, ""); w.Code != 502 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	} else if strings.Contains(w.Body.String(), secret) {
		t.Fatal("transport error leaked in test response")
	}
	w := request(h, "GET", "/api/bark", "", c, "")
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("transport error leaked in bark status")
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
	// The limiter must not let unauthenticated guesses lock out the owner. A
	// reverse proxy makes every client share one RemoteAddr, so the correct
	// password has to keep working even after the failure budget is spent.
	w = request(h, "POST", "/api/login", `{"password":"test-password-long-enough"}`, nil, "")
	if w.Code != 200 {
		t.Fatalf("correct password was locked out by earlier failures: %d", w.Code)
	}
}

func TestSettingsResponseReflectsStoredDefaults(t *testing.T) {
	_, h := testApp(t)
	c := loginCookie(t, h)
	payload := `{"timezone":"Asia/Shanghai","quiet_enabled":true,"quiet_start":"23:00","quiet_end":"08:00","deadline_minutes":[1440],"exam_start_minutes":[60],"video_minutes":[4320],"in_class_enabled":true,"in_class_poll_seconds":0}`
	w := request(h, "PUT", "/api/settings", payload, c, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var updated core.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.InClassPollSeconds != core.DefaultInClassPollSeconds {
		t.Fatalf("PUT echoed normalized value: %d", updated.InClassPollSeconds)
	}
	w = request(h, "GET", "/api/settings", "", c, "")
	var stored core.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated, stored) {
		t.Fatalf("response and stored settings differ: %+v vs %+v", updated, stored)
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

func TestEventPayloadKeepsScheduledReminders(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	events := []core.Event{
		{ID: "later-reminder", Status: "pending", CreatedAt: base, DueAt: base.Add(48 * time.Hour)},
		{ID: "earlier-reminder", Status: "sending", CreatedAt: base, DueAt: base.Add(24 * time.Hour)},
	}
	for i := 0; i < 250; i++ {
		events = append(events, core.Event{ID: fmt.Sprintf("sent-%03d", i), Status: "sent", CreatedAt: base.Add(time.Duration(i+1) * time.Minute)})
	}
	got := trimEvents(events, 200)
	if len(got) != 200 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].ID != "earlier-reminder" || got[1].ID != "later-reminder" {
		t.Fatalf("scheduled reminders missing or out of order: %s, %s", got[0].ID, got[1].ID)
	}
	for _, event := range got {
		if event.ID == "sent-000" {
			t.Fatal("oldest history outranked newer history")
		}
	}
	// An actionable backlog larger than the limit is itself bounded.
	backlog := make([]core.Event, 0, 5)
	for i := 0; i < 5; i++ {
		backlog = append(backlog, core.Event{ID: fmt.Sprintf("pending-%d", i), Status: "pending", DueAt: base.Add(time.Duration(i) * time.Minute)})
	}
	if got := trimEvents(backlog, 3); len(got) != 3 || got[0].ID != "pending-0" {
		t.Fatalf("active backlog not bounded by due order: %v", got)
	}
}
