package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type rainSessionTransport func(*http.Request) (*http.Response, error)

func (f rainSessionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func rainSessionResponse(r *http.Request, cookies ...string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": cookies}, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"id":1}}`)), Request: r}
}

func exportRainSession(t *testing.T, r *Rain) string {
	t.Helper()
	exporter, ok := any(r).(interface{ Credentials() (string, error) })
	if !ok {
		t.Fatal("Rain cannot persist renewed credentials")
	}
	data, err := exporter.Credentials()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRainRenewalUsedByNextRequestAndCSRF(t *testing.T) {
	r := NewRain("sessionid=old; csrftoken=old-csrf")
	calls := 0
	r.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return rainSessionResponse(req, "sessionid=new; Path=/; Secure; HttpOnly", "csrftoken=root-csrf; Path=/; Secure", "csrftoken=scoped-csrf; Path=/mooc-api; Secure"), nil
		}
		session, err := req.Cookie("sessionid")
		if err != nil || session.Value != "new" || strings.Contains(req.Header.Get("Cookie"), "old") {
			t.Error("next request used stale credentials")
		}
		if req.Method == "POST" && req.Header.Get("X-CSRFToken") != "scoped-csrf" {
			t.Error("CSRF did not use the most specific applicable path")
		}
		if strings.HasPrefix(req.URL.Path, "/api/") && strings.Contains(req.Header.Get("Cookie"), "scoped-csrf") {
			t.Error("path-scoped cookie escaped its path")
		}
		return rainSessionResponse(req), nil
	})
	if err := r.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.request(context.Background(), "POST", "/mooc-api/query", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRainLegacyDomainRenewalSurvivesRestart(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		for _, remove := range []bool{false, true} {
			name := "direct"
			if reopen {
				name = "reopened"
			}
			if remove {
				name += "-deletion"
			}
			t.Run(name, func(t *testing.T) {
				r := NewRain("sessionid=old; csrftoken=old-csrf; keep=value")
				if reopen {
					r = NewRain(exportRainSession(t, r))
				}
				if err := r.ensureSession(); err != nil {
					t.Fatal(err)
				}
				u, _ := url.Parse(rainOrigin + "/api/v3/user/basic-info")
				cookies := []*http.Cookie{
					{Name: "sessionid", Value: "new", Domain: ".yuketang.cn", Path: "/", Secure: true},
					{Name: "csrftoken", Value: "new-csrf", Domain: ".yuketang.cn", Path: "/", Secure: true},
				}
				if remove {
					for _, cookie := range cookies {
						cookie.MaxAge = -1
					}
				}
				r.jar.SetCookies(u, cookies)
				saved := exportRainSession(t, r)
				if strings.Contains(saved, "old") {
					t.Error("renewal retained stale legacy credentials")
				}
				for _, adapter := range []*Rain{r, NewRain(saved)} {
					adapter.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
						if strings.Contains(req.Header.Get("Cookie"), "old") {
							t.Error("legacy cookie shadows authoritative domain cookie")
						}
						for _, name := range []string{"sessionid", "csrftoken"} {
							cookie, err := req.Cookie(name)
							if remove {
								if !errors.Is(err, http.ErrNoCookie) {
									t.Error("deleted legacy cookie was revived")
								}
							} else if err != nil || !strings.HasPrefix(cookie.Value, "new") {
								t.Error("request did not use renewed cookie")
							}
						}
						wantCSRF := "new-csrf"
						if remove {
							wantCSRF = ""
						}
						if req.Header.Get("X-CSRFToken") != wantCSRF {
							t.Error("request did not use renewed CSRF")
						}
						return rainSessionResponse(req), nil
					})
					if _, err := adapter.request(context.Background(), "POST", "/api/query", "", nil); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestRainLegacyReconciliationPreservesKnownScopes(t *testing.T) {
	r := NewRain("sessionid=root")
	if err := r.ensureSession(); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(rainOrigin + "/api/query")
	r.jar.SetCookies(u, []*http.Cookie{{Name: "sessionid", Value: "narrow", Domain: ".yuketang.cn", Path: "/api"}})
	if got := r.jar.Cookies(u); len(got) != 2 || got[0].Value != "narrow" || got[1].Value != "root" {
		t.Fatal("a different-path cookie removed the legacy fallback")
	}
	known := NewRain(`{"version":1,"cookies":[{"name":"sessionid","value":"host","path":"/"}]}`)
	if err := known.ensureSession(); err != nil {
		t.Fatal(err)
	}
	known.jar.SetCookies(u, []*http.Cookie{{Name: "sessionid", Value: "parent", Domain: ".yuketang.cn", Path: "/"}})
	for _, adapter := range []*Rain{known, NewRain(exportRainSession(t, known))} {
		if err := adapter.ensureSession(); err != nil {
			t.Fatal(err)
		}
		if got := adapter.jar.Cookies(u); len(got) != 2 {
			t.Fatal("a genuine host cookie was replaced by a different domain cookie")
		}
	}
}

func TestRainSessionScopesAndAbsoluteExpirySurviveRestart(t *testing.T) {
	r := NewRain("sessionid=legacy")
	r.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		return rainSessionResponse(req,
			"scope=root; Path=/; Secure", "scope=narrow; Path=/api; Secure",
			"defaultpath=value; Secure", "sessionid=renewed; Path=/; Max-Age=3600; Expires=Thu, 01 Jan 1970 00:00:01 GMT; Secure; HttpOnly",
			"domain=value; Domain=.yuketang.cn; Path=/; Secure"), nil
	})
	if err := r.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	serialized := exportRainSession(t, r)
	var session struct {
		Version int `json:"version"`
		Cookies []struct {
			Name, Value, Domain, Path string
			Expires                   time.Time
		} `json:"cookies"`
	}
	if json.Unmarshal([]byte(serialized), &session) != nil || session.Version != 1 || len(session.Cookies) != 5 {
		t.Fatal("serialized session lost scoped cookies")
	}
	var expiry time.Time
	for _, c := range session.Cookies {
		if c.Name == "sessionid" {
			expiry = c.Expires
		}
		if c.Name == "defaultpath" && c.Path != "/api/v3/user" {
			t.Fatal("default cookie path changed")
		}
	}
	if expiry.IsZero() || time.Until(expiry) < 59*time.Minute || time.Until(expiry) > time.Hour {
		t.Fatal("relative Max-Age was not converted to absolute expiry")
	}
	restored := NewRain(serialized)
	if got := exportRainSession(t, restored); got != serialized {
		t.Fatal("restart changed the session or extended its lifetime")
	}
	restored.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		var values []string
		for _, cookie := range req.Cookies() {
			if cookie.Name == "scope" {
				values = append(values, cookie.Value)
			}
		}
		want := "root"
		if strings.HasPrefix(req.URL.Path, "/api/") {
			want = "narrow,root"
		}
		if strings.Join(values, ",") != want {
			t.Errorf("scoped cookies = %v; want %s", values, want)
		}
		return rainSessionResponse(req), nil
	})
	if err := restored.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.get(context.Background(), "/other"); err != nil {
		t.Fatal(err)
	}
}

func TestRainCookieIngestionStopsAtLimits(t *testing.T) {
	u, _ := url.Parse("https://www.yuketang.cn/")
	for _, input := range []string{"batch", "value"} {
		t.Run(input, func(t *testing.T) {
			jar := newRainCookieJar()
			cookies := []*http.Cookie{{Name: "large", Value: strings.Repeat("x", 65537)}}
			if input == "batch" {
				cookies = nil
				for i := 0; i < 300; i++ {
					cookies = append(cookies, &http.Cookie{Name: "item" + strconv.Itoa(i), Value: "value"})
				}
			}
			jar.SetCookies(u, cookies)
			if jar.check() == nil || len(jar.records) != 0 || len(jar.Cookies(u)) != 0 {
				t.Error("oversized input was retained before rejecting the session")
			}
			before := len(jar.records)
			jar.SetCookies(u, []*http.Cookie{{Name: "later", Value: "must-not-be-processed"}})
			if len(jar.records) != before || len(jar.Cookies(u)) != 0 {
				t.Error("invalid jar continued accepting cookies")
			}
		})
	}
}

func TestRainLegacySessionAndExpiredStoredSession(t *testing.T) {
	legacy := NewRain("sessionid=legacy; csrftoken=token")
	serialized := exportRainSession(t, legacy)
	if !json.Valid([]byte(serialized)) || strings.Contains(serialized, "expires") {
		t.Fatal("legacy import invented an expiration")
	}
	for _, credentials := range []string{
		`{"version":1,"cookies":[]}`,
		`{"version":1,"cookies":[{"name":"sessionid","value":"expired","path":"/","expires":"2000-01-01T00:00:00Z"}]}`,
	} {
		r := NewRain(credentials)
		r.client.Transport = rainSessionTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("expired session reached the network")
			return nil, nil
		})
		if err := r.Validate(context.Background()); !errors.Is(err, ErrSessionExpired) {
			t.Fatal("empty or expired saved session was revived", err)
		}
		if got := exportRainSession(t, r); got != `{"version":1,"cookies":[]}` {
			t.Fatal("expired cookie remained in saved credentials")
		}
	}
}

func TestRainCookieDomainsAndQRRedirectRemainRestricted(t *testing.T) {
	r := NewRain("sessionid=keep")
	r.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		return rainSessionResponse(req, "foreign=value; Domain=evil.example; Path=/", "wide=value; Domain=cn; Path=/", "allowed=value; Domain=.yuketang.cn; Path=/"), nil
	})
	if err := r.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	serialized := exportRainSession(t, r)
	if strings.Contains(serialized, "foreign") || strings.Contains(serialized, "wide") || !strings.Contains(serialized, "allowed") {
		t.Fatal("foreign or public-suffix cookie scope persisted")
	}
	flow := &RainLogin{base: "https://www.yuketang.cn", token: "fixture-token", client: &http.Client{}}
	calls := 0
	flow.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "www.yuketang.cn" {
			t.Fatal("login redirected outside Rain")
		}
		resp := rainSessionResponse(req)
		resp.StatusCode = 302
		resp.Header.Set("Location", "https://evil.example")
		return resp, nil
	})
	if _, err := flow.Wait(context.Background()); err == nil || calls != 1 {
		t.Fatal("QR login followed a redirect")
	}
}

func TestRainDeletedAndExpiredCookiesStayGone(t *testing.T) {
	r := NewRain("sessionid=keep; remove=old")
	r.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		return rainSessionResponse(req, "remove=deleted; Path=/; Max-Age=0", "past=value; Path=/; Expires=Thu, 01 Jan 1970 00:00:01 GMT", "sessionid=keep; Path=/"), nil
	})
	if err := r.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	serialized := exportRainSession(t, r)
	if strings.Contains(serialized, "remove") || strings.Contains(serialized, "past") {
		t.Fatal("deleted or expired cookie was persisted")
	}
	restored := NewRain(serialized)
	restored.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Cookie") != "sessionid=keep" {
			t.Fatal("restart revived a deleted cookie")
		}
		return rainSessionResponse(req), nil
	})
	if err := restored.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRainRejectsMalformedSessionBeforeNetwork(t *testing.T) {
	for _, credentials := range []string{
		`{"version":1,"cookies":`, `{"version":2,"cookies":[]}`, `{"version":1,"cookies":null}`,
		`{"version":1,"cookies":[{"name":"sessionid","value":"private-value","path":"/","domain":"evil.example"}]}`,
		`{"version":1,"cookies":[{"name":"sessionid","value":"private-value","path":"invalid"}]}`,
		`{"version":1,"cookies":[{"name":"sessionid","value":"private-value","path":"/","max_age":3600}]}`,
		`{"version":1,"cookies":[{"name":"sessionid","value":"private-value","path":"/","domain":".yuketang.cn","legacy":true}]}`,
		`{"version":1,"cookies":[{"name":"sessionid","value":"private-value","path":"/api","legacy":true}]}`,
		`{"version":1,"cookies":[{"name":"sessionid","value":"private-value","path":"/","expires":"2030-01-01T00:00:00Z","legacy":true}]}`,
		"sessionid=private-value\r\nInjected: private-value", "not-a-cookie", strings.Repeat("x", 65537),
	} {
		t.Run(credentials[:min(25, len(credentials))], func(t *testing.T) {
			r := NewRain(credentials)
			called := false
			r.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
				called = true
				return rainSessionResponse(req), nil
			})
			err := r.Validate(context.Background())
			if err == nil || called || strings.Contains(err.Error(), "private-value") {
				t.Fatal("malformed credentials escaped validation or leaked")
			}
		})
	}
}

func TestRainSessionRejectsForeignOrigin(t *testing.T) {
	r := NewRain("sessionid=private-value")
	r.baseURL = "https://evil.example"
	called := false
	r.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		called = true
		return rainSessionResponse(req), nil
	})
	if err := r.Validate(context.Background()); err == nil || called {
		t.Fatal("Rain sent credentials outside its fixed origin")
	}
}

func TestRainQRExportsScopedSession(t *testing.T) {
	flow := &RainLogin{base: "https://www.yuketang.cn", token: "fixture-token", client: &http.Client{}}
	flow.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		resp := rainSessionResponse(req)
		switch req.URL.Path {
		case "/api/v3/user/login/app-web-login":
			resp.Header.Set("Set-Cookie", "csrftoken=pc-only; Path=/pc; Secure")
			resp.Body = io.NopCloser(strings.NewReader(`{"code":0,"data":{"UserID":1,"Auth":"fixture-auth"}}`))
		case "/pc/web_login":
			if req.Header.Get("X-CSRFToken") != "pc-only" {
				t.Error("QR confirmation lost path-scoped CSRF")
			}
			resp.Header["Set-Cookie"] = []string{"sessionid=new; Path=/; Max-Age=3600; Secure; HttpOnly", "api-only=value; Path=/api; Secure"}
			resp.Body = io.NopCloser(strings.NewReader(`{"success":true}`))
		}
		return resp, nil
	})
	// The old implementation requires a jar to reach the serialization assertion.
	flow.client.Jar, _ = cookiejar.New(nil)
	serialized, err := flow.Wait(context.Background())
	if err != nil || !json.Valid([]byte(serialized)) || !strings.Contains(serialized, "api-only") || !strings.Contains(serialized, "pc-only") {
		t.Fatal("QR flow flattened or lost its session", err)
	}
	restored := NewRain(serialized)
	restored.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		if _, err := req.Cookie("api-only"); err != nil {
			t.Error("QR path cookie did not survive import")
		}
		if _, err := req.Cookie("csrftoken"); !errors.Is(err, http.ErrNoCookie) {
			t.Error("QR CSRF cookie escaped /pc path")
		}
		return rainSessionResponse(req), nil
	})
	if err := restored.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}
