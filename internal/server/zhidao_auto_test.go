package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

func testZhidaoSession(t *testing.T, expiry time.Time, user string) string {
	t.Helper()
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiry.Unix())))
	token := "{{SECRET_9gpcfa31}}." + claims + ".signature"
	raw, err := json.Marshal(map[string]any{
		"token":    "ai-poly",
		"user_nid": user,
		"cookies": []map[string]any{{
			"url":    "https://cloudapi.polymas.com",
			"cookie": map[string]any{"name": "jt-cas", "value": token, "path": "/", "domain": ".polymas.com"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func enableZhidaoAutoLogin(t *testing.T, s *Server) {
	t.Helper()
	settings := s.Engine.Snapshot().Settings
	settings.ZhidaoAutoLogin = true
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
}

func TestZhidaoAutoLoginCredentialsStayWriteOnly(t *testing.T) {
	s, h := testApp(t)
	s.smsSource = &fakeSMSSource{}
	c := loginCookie(t, h)
	w := request(h, "PUT", "/api/zhidao/auto-login", `{"enabled":true,"phone":"138 0000 0000","password":"secret-password"}`, c, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	config := s.Vault.Get()
	if config.ZhidaoPhone != "13800000000" || config.ZhidaoPassword != "secret-password" {
		t.Fatal("credentials were not normalized and stored")
	}
	if !s.Engine.Snapshot().Settings.ZhidaoAutoLogin {
		t.Fatal("auto-login was not enabled")
	}
	w = request(h, "GET", "/api/zhidao", "", c, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "secret-password") || strings.Contains(body, "13800000000") {
		t.Fatal("GET leaked a stored credential")
	}
	if !strings.Contains(body, "138****0000") {
		t.Fatalf("masked phone missing from %s", body)
	}
	// Clearing removes both values and disables renewal.
	w = request(h, "PUT", "/api/zhidao/auto-login", `{"clear":true}`, c, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	config = s.Vault.Get()
	if config.ZhidaoPhone != "" || config.ZhidaoPassword != "" || s.Engine.Snapshot().Settings.ZhidaoAutoLogin {
		t.Fatal("clear did not remove credentials or disable renewal")
	}
	if w = request(h, "PUT", "/api/zhidao/auto-login", `{"phone":"12345"}`, c, ""); w.Code != 400 {
		t.Fatalf("invalid phone accepted: %d", w.Code)
	}
	if w = request(h, "PUT", "/api/zhidao/auto-login", `{"enabled":true,"phone":"13800000000"}`, c, ""); w.Code != 400 {
		t.Fatalf("enabled without a password: %d", w.Code)
	}
}

func TestZhidaoAutoLoginRenewsBeforeExpiry(t *testing.T) {
	s, _ := testApp(t)
	s.smsSource = &fakeSMSSource{}
	oldSession := testZhidaoSession(t, time.Now().Add(30*time.Minute), "user-1")
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.ZhidaoSession, c.ZhidaoPhone, c.ZhidaoPassword = oldSession, "13800000000", "password"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	enableZhidaoAutoLogin(t, s)
	updated := testZhidaoSession(t, time.Now().Add(24*time.Hour), "user-1")
	var gotPhone, gotPassword string
	var collector platform.SMSCollector
	s.passwordLogin = func(_ context.Context, phone, password string, sms platform.SMSCollector) (string, error) {
		gotPhone, gotPassword, collector = phone, password, sms
		return updated, nil
	}
	s.newZhidao = func(serialized string) rainAdapter {
		if serialized != updated {
			t.Errorf("sync received stale credentials")
		}
		return fakeRainAdapter{}
	}
	s.maybeAutoLoginZhidao()
	eventually(t, func() bool {
		s.zhidaoAutoMu.Lock()
		defer s.zhidaoAutoMu.Unlock()
		return s.zhidaoAutoState == "success"
	})
	if gotPhone != "13800000000" || gotPassword != "password" || collector == nil {
		t.Fatal("renewal did not receive the stored credentials and SMS collector")
	}
	if s.Vault.Get().ZhidaoSession != updated {
		t.Fatal("renewed session was not persisted")
	}
	if expiry, ok := platform.ZhidaoSessionExpiry(s.Vault.Get().ZhidaoSession); !ok || time.Until(expiry) < 23*time.Hour {
		t.Fatal("persisted session does not carry the renewed expiry")
	}
}

func TestZhidaoAutoLoginRejectsIdentityMismatch(t *testing.T) {
	s, _ := testApp(t)
	s.smsSource = &fakeSMSSource{}
	oldSession := testZhidaoSession(t, time.Now().Add(30*time.Minute), "user-1")
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.ZhidaoSession, c.ZhidaoPhone, c.ZhidaoPassword = oldSession, "13800000000", "password"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	enableZhidaoAutoLogin(t, s)
	s.passwordLogin = func(context.Context, string, string, platform.SMSCollector) (string, error) {
		return testZhidaoSession(t, time.Now().Add(24*time.Hour), "user-2"), nil
	}
	s.maybeAutoLoginZhidao()
	eventually(t, func() bool {
		s.zhidaoAutoMu.Lock()
		defer s.zhidaoAutoMu.Unlock()
		return s.zhidaoAutoState == "failed"
	})
	if s.Vault.Get().ZhidaoSession != oldSession {
		t.Fatal("a different Zhidao identity replaced the bound session")
	}
	if !strings.Contains(s.zhidaoAutoView().Error, "不一致") {
		t.Fatalf("unexpected error: %q", s.zhidaoAutoView().Error)
	}
}

func TestZhidaoAutoLoginRespectsDisabledSetting(t *testing.T) {
	s, _ := testApp(t)
	s.smsSource = &fakeSMSSource{}
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.ZhidaoSession, c.ZhidaoPhone, c.ZhidaoPassword = testZhidaoSession(t, time.Now().Add(time.Minute), "user-1"), "13800000000", "password"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.maybeAutoLoginZhidao()
	s.zhidaoAutoMu.Lock()
	state := s.zhidaoAutoState
	s.zhidaoAutoMu.Unlock()
	if state != "" {
		t.Fatalf("disabled renewal started: %q", state)
	}
}
