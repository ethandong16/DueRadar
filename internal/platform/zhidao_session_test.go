package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestZhidaoCookieSnapshotReplacementDeletionAndExpiry(t *testing.T) {
	_, jar := newZhidaoClient()
	u, _ := url.Parse("https://cloudapi.polymas.com/oauth/login/ssoLogin?ticket=private")
	for _, value := range []string{"old", "new"} {
		jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: value, MaxAge: 3600, Secure: true}})
	}
	records := jar.snapshot()
	if len(records) != 1 || records[0].Cookie.Value != "new" || records[0].Cookie.MaxAge != 0 || records[0].Cookie.Expires.IsZero() {
		t.Fatal("cookie replacement or absolute expiry lost")
	}
	if strings.Contains(records[0].URL, "ticket") || records[0].Cookie.Path != "/oauth/login" {
		t.Fatal("cookie origin retained query or lost default path")
	}
	expires := records[0].Cookie.Expires
	data, _ := json.Marshal(ZhidaoSession{Token: "token", Cookies: records})
	restored := NewZhidao(string(data))
	serialized, err := restored.Credentials()
	if err != nil {
		t.Fatal(err)
	}
	var session ZhidaoSession
	if err := json.Unmarshal([]byte(serialized), &session); err != nil || len(session.Cookies) != 1 || !session.Cookies[0].Cookie.Expires.Equal(expires) {
		t.Fatal("restoring a session extended its cookie lifetime")
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "session", MaxAge: -1, Secure: true}})
	if len(jar.snapshot()) != 0 || len(jar.Cookies(u)) != 0 {
		t.Fatal("deleted cookie remained available")
	}
}

func TestZhidaoRestoreRejectsExpiredAndUnboundedRelativeCookies(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	records := []zhidaoCookie{
		{URL: zhidaoCloud, Cookie: &http.Cookie{Name: "expired", Value: "old", Path: "/", MaxAge: 3600, Expires: past}},
		{URL: zhidaoCloud, Cookie: &http.Cookie{Name: "relative", Value: "old", Path: "/", MaxAge: 3600}},
		{URL: zhidaoCloud, Cookie: &http.Cookie{Name: "session", Value: "keep", Path: "/"}},
	}
	data, _ := json.Marshal(ZhidaoSession{Token: "token", Cookies: records})
	z := NewZhidao(string(data))
	u, _ := url.Parse(zhidaoCloud)
	cookies := z.client.Jar.Cookies(u)
	if len(cookies) != 1 || cookies[0].Name != "session" {
		t.Fatal("restoring old relative expiry revived a cookie")
	}
}

func TestZhidaoCookieScopeAndSnapshots(t *testing.T) {
	_, jar := newZhidaoClient()
	u, _ := url.Parse(zhidaoCloud)
	jar.SetCookies(u, []*http.Cookie{
		{Name: "wide", Value: "bad", Domain: ".com", Path: "/"},
		{Name: "other", Value: "bad", Domain: ".zhihuishu.com", Path: "/"},
		{Name: "good", Value: "value", Domain: ".polymas.com", Path: "/"},
	})
	records := jar.snapshot()
	if len(records) != 1 || len(jar.Cookies(u)) != 1 {
		t.Fatal("invalid cookie scope was persisted")
	}
	records[0].Cookie.Value = "mutated"
	if jar.snapshot()[0].Cookie.Value != "value" {
		t.Fatal("snapshot aliases mutable cookie state")
	}
}

func TestZhidaoCredentialsPreserveRenewedSession(t *testing.T) {
	data, _ := json.Marshal(ZhidaoSession{Token: "old", Cookies: []zhidaoCookie{
		{URL: zhidaoCloud, Cookie: &http.Cookie{Name: "jt-cas", Value: "old", Domain: ".polymas.com", Path: "/"}},
		{URL: "https://studyresources.zhihuishu.com", Cookie: &http.Cookie{Name: "legacy", Value: "keep", Domain: ".zhihuishu.com", Path: "/"}},
	}})
	z := NewZhidao(string(data))
	requests := 0
	z.client.Transport = zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		want := "old"
		if requests > 1 {
			want = "new token"
		}
		if r.Header.Get("Authorization") != want {
			t.Error("rotated token not used by next request")
		}
		return zhsResponse(r, 200, `{"code":200,"data":{"userNid":"user","schoolCertificationNid":"school"}}`, http.Header{"Set-Cookie": {"jt-cas=new%20token; Domain=.polymas.com; Path=/; Max-Age=3600; Secure; HttpOnly"}}), nil
	})
	for i := 0; i < 2; i++ {
		if err := z.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	serialized, err := z.Credentials()
	if err != nil {
		t.Fatal(err)
	}
	var session ZhidaoSession
	if json.Unmarshal([]byte(serialized), &session) != nil || session.Token != "new token" || len(session.Cookies) != 2 || session.UserNID != "user" {
		t.Fatal("renewed token, identity, or untouched legacy cookie was lost")
	}
	restored := NewZhidao(serialized)
	if restored.session.Token != session.Token {
		t.Fatal("renewed token was lost after restart")
	}
}
