package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

func seedSlideCredentials(t *testing.T, s *Server, session string) {
	t.Helper()
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.ZhidaoPhone = "13800000000"
		c.ZhidaoPassword = "secret-password"
		c.ZhidaoSession = session
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSlideLoginUsesStoredCredentialsAndStoresSession(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	old := testZhidaoSession(t, time.Now().Add(time.Hour), "user-1")
	seedSlideCredentials(t, s, old)
	updated := testZhidaoSession(t, time.Now().Add(2*time.Hour), "user-1")

	var got []string
	s.slideLogin = func(_ context.Context, phone, password, validate string, _ platform.SMSCollector) (string, error) {
		got = []string{phone, password, validate}
		return updated, nil
	}
	w := request(h, "POST", "/api/zhidao/slider", `{"validate":"  token-1  "}`, cookie, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	want := []string{"13800000000", "secret-password", "token-1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("slide login received %q", got)
	}
	if s.Vault.Get().ZhidaoSession != updated {
		t.Fatal("new session was not stored")
	}
}

func TestSlideLoginRejectsCaptchaFailureWithoutReplacingSession(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	old := testZhidaoSession(t, time.Now().Add(time.Hour), "user-1")
	seedSlideCredentials(t, s, old)
	s.slideLogin = func(context.Context, string, string, string, platform.SMSCollector) (string, error) {
		return "", platform.ErrCaptchaRejected
	}
	w := request(h, "POST", "/api/zhidao/slider", `{"validate":"token-1"}`, cookie, "")
	if w.Code != 401 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "重新滑动") {
		t.Fatalf("unexpected message: %s", w.Body.String())
	}
	if s.Vault.Get().ZhidaoSession != old {
		t.Fatal("failed login replaced the stored session")
	}
}

func TestSlideLoginRejectsAnotherIdentity(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	old := testZhidaoSession(t, time.Now().Add(time.Hour), "user-1")
	seedSlideCredentials(t, s, old)
	s.slideLogin = func(context.Context, string, string, string, platform.SMSCollector) (string, error) {
		return testZhidaoSession(t, time.Now().Add(2*time.Hour), "user-2"), nil
	}
	w := request(h, "POST", "/api/zhidao/slider", `{"validate":"token-1"}`, cookie, "")
	if w.Code != 409 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if s.Vault.Get().ZhidaoSession != old {
		t.Fatal("mismatched identity replaced the stored session")
	}
}

func TestSlideLoginRequiresCredentialsAndToken(t *testing.T) {
	_, h := testApp(t)
	cookie := loginCookie(t, h)
	if w := request(h, "POST", "/api/zhidao/slider", `{"validate":""}`, cookie, ""); w.Code != 400 {
		t.Fatalf("empty token: %d %s", w.Code, w.Body.String())
	}
	w := request(h, "POST", "/api/zhidao/slider", `{"validate":"token-1"}`, cookie, "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "手机号") {
		t.Fatalf("missing credentials: %d %s", w.Code, w.Body.String())
	}
}

func TestSlideLoginReportsPasswordRejection(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	seedSlideCredentials(t, s, testZhidaoSession(t, time.Now().Add(time.Hour), "user-1"))
	s.slideLogin = func(context.Context, string, string, string, platform.SMSCollector) (string, error) {
		return "", platform.ErrPasswordRejected
	}
	w := request(h, "POST", "/api/zhidao/slider", `{"validate":"token-1"}`, cookie, "")
	if w.Code != 401 || !strings.Contains(w.Body.String(), "密码错误") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestSlideLoginRejectsMalformedSession(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	seedSlideCredentials(t, s, "")
	s.slideLogin = func(context.Context, string, string, string, platform.SMSCollector) (string, error) { return "", nil }
	w := request(h, "POST", "/api/zhidao/slider", `{"validate":"token-1"}`, cookie, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "未返回有效会话") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if s.Vault.Get().ZhidaoSession != "" {
		t.Fatal("empty login result was stored")
	}
}

// A transport failure must not surface as 5xx: Cloudflare replaces those bodies
// with its own error page, which hides the message from the slider UI.
func TestSlideLoginTransportFailureUsesJSONEnvelope(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	seedSlideCredentials(t, s, "")
	s.slideLogin = func(context.Context, string, string, string, platform.SMSCollector) (string, error) {
		return "", platform.ErrLoginIncomplete
	}
	w := request(h, "POST", "/api/zhidao/slider", `{"validate":"token-1"}`, cookie, "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Error == "" {
		t.Fatalf("unexpected envelope: %s", w.Body.String())
	}
}

func TestSlideLoginBusyWhileBinding(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	seedSlideCredentials(t, s, "")
	s.zhidaoMu.Lock()
	s.zhidaoState = "waiting"
	s.zhidaoMu.Unlock()
	s.slideLogin = func(context.Context, string, string, string, platform.SMSCollector) (string, error) {
		t.Error("login ran while a QR binding was active")
		return "", errors.New("unexpected")
	}
	if w := request(h, "POST", "/api/zhidao/slider", `{"validate":"token-1"}`, cookie, ""); w.Code != 409 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestSliderPageGetsScopedCSP(t *testing.T) {
	s, _ := testApp(t)
	h := s.Handler(fstest.MapFS{
		"index.html":  {Data: []byte("<html></html>")},
		"slider.html": {Data: []byte("<html></html>")},
	})
	w := request(h, "GET", "/slider.html", "", nil, "")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "*.126.net") {
		t.Fatalf("slider page CSP does not allow the captcha CDN: %q", csp)
	}
	if w := request(h, "GET", "/index.html", "", nil, ""); strings.Contains(w.Header().Get("Content-Security-Policy"), "*.126.net") {
		t.Fatal("dashboard CSP was relaxed")
	}
}
