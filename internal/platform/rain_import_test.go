package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func rainBrowserCookieFixture() map[string]any {
	return map[string]any{"name": "sessionid", "value": "fixture-secret", "domain": "www.yuketang.cn", "path": "/", "secure": true, "http_only": true, "same_site": 2}
}

func rainBrowserPayload(t *testing.T, cookies ...map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"version": 1, "cookies": cookies})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestImportRainBrowserSessionPreservesScopesAcrossReopen(t *testing.T) {
	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Millisecond)
	session := rainBrowserCookieFixture()
	session["domain"] = "WWW.YUKETANG.CN"
	session["expires"] = expiry.In(time.FixedZone("browser", 8*60*60)).Format(time.RFC3339Nano)
	cookies := []map[string]any{
		session,
		{"name": "csrftoken", "value": "root-token", "domain": "yuketang.cn", "path": "/", "same_site": 1},
		{"name": "csrftoken", "value": "api-token", "domain": ".yuketang.cn", "path": "/api", "same_site": 3, "secure": true},
		{"name": "extra", "value": "elsewhere", "domain": "www.yuketang.cn", "path": "/elsewhere", "same_site": 4, "secure": true},
		{"name": "expired", "value": "discard", "domain": "www.yuketang.cn", "path": "/", "expires": "2000-01-01T00:00:00Z"},
	}
	encoded, err := ImportRainBrowserSession(rainBrowserPayload(t, cookies...))
	if err != nil {
		t.Fatal(err)
	}
	var saved rainSavedSession
	if json.Unmarshal([]byte(encoded), &saved) != nil || len(saved.Cookies) != 4 {
		t.Fatal("effective cookies or scope metadata lost")
	}
	if got := saved.Cookies[0]; got.Domain != "www.yuketang.cn" || !got.HTTPOnly || !got.Secure || got.SameSite != http.SameSiteLaxMode || !got.Expires.Equal(expiry) || got.Expires.Location() != time.UTC {
		t.Fatal("browser cookie metadata changed")
	}
	if saved.Cookies[1].Domain != ".yuketang.cn" || saved.Cookies[2].Path != "/api" || saved.Cookies[2].SameSite != http.SameSiteStrictMode || saved.Cookies[3].SameSite != http.SameSiteNoneMode {
		t.Fatal("domain, path or SameSite was flattened")
	}
	if strings.Contains(encoded, "legacy") || strings.Contains(encoded, "quoted") || strings.Contains(encoded, "expired") {
		t.Fatal("import retained private markers or expired cookies")
	}
	for i := 0; i < 2; i++ {
		r := NewRain(encoded)
		calls := 0
		r.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.URL.Host != "www.yuketang.cn" {
				t.Fatal("credentials left the Rain origin")
			}
			cookie, err := req.Cookie("sessionid")
			if err != nil || cookie.Value != "fixture-secret" || strings.Contains(req.Header.Get("Cookie"), "elsewhere") {
				t.Fatal("request lost cookie values or path boundaries")
			}
			if req.Method == "POST" && req.Header.Get("X-CSRFToken") != "api-token" {
				t.Fatal("path-specific CSRF did not survive import")
			}
			return rainSessionResponse(req), nil
		})
		if err := r.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := r.request(context.Background(), "POST", "/api/query", "", nil); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatal("independent validation was not performed")
		}
		reopened, err := r.Credentials()
		if err != nil || reopened != encoded {
			t.Fatal("save/reopen changed scoped credentials or absolute expiry")
		}
		encoded = reopened
	}
}

func TestImportRainBrowserSessionDoesNotAuthenticate(t *testing.T) {
	encoded, err := ImportRainBrowserSession(rainBrowserPayload(t, rainBrowserCookieFixture()))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRain(encoded)
	r.client.Transport = rainSessionTransport(func(req *http.Request) (*http.Response, error) {
		response := rainSessionResponse(req)
		response.StatusCode = http.StatusUnauthorized
		return response, nil
	})
	if !errors.Is(r.Validate(context.Background()), ErrSessionExpired) {
		t.Fatal("a syntactically valid browser import bypassed independent authentication")
	}
}

func TestImportRainBrowserSessionRejectsAmbiguousJSON(t *testing.T) {
	cookie := `{"name":"sessionid","value":"fixture-secret","domain":"www.yuketang.cn","path":"/"}`
	valid := `{"version":1,"cookies":[` + cookie + `]}`
	cases := map[string]string{
		"raw cookie":              "sessionid=fixture-secret",
		"string":                  `"fixture-secret"`,
		"array":                   `[` + cookie + `]`,
		"null":                    `null`,
		"truncated":               valid[:len(valid)-1],
		"trailing object":         valid + `{}`,
		"trailing junk":           valid + `garbage`,
		"missing version":         `{"cookies":[` + cookie + `]}`,
		"string version":          strings.Replace(valid, `"version":1`, `"version":"1"`, 1),
		"float version":           strings.Replace(valid, `"version":1`, `"version":1.0`, 1),
		"unknown version":         strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"null version":            strings.Replace(valid, `"version":1`, `"version":null`, 1),
		"duplicate version":       strings.Replace(valid, `"version":1`, `"version":2,"version":1`, 1),
		"escaped duplicate":       strings.Replace(valid, `"version":1`, `"version":1,"\u0076ersion":1`, 1),
		"case insensitive field":  strings.Replace(valid, `"version"`, `"Version"`, 1),
		"unknown root":            strings.Replace(valid, `"version":1`, `"secret":"fixture-secret","version":1`, 1),
		"missing cookies":         `{"version":1}`,
		"null cookies":            `{"version":1,"cookies":null}`,
		"empty cookies":           `{"version":1,"cookies":[]}`,
		"object cookies":          `{"version":1,"cookies":{}}`,
		"null cookie":             `{"version":1,"cookies":[null]}`,
		"cookie array":            `{"version":1,"cookies":[[]]}`,
		"duplicate cookie field":  strings.Replace(valid, `"value":"fixture-secret"`, `"value":"old","value":"fixture-secret"`, 1),
		"case cookie field":       strings.Replace(valid, `"domain"`, `"Domain"`, 1),
		"nested cookie field":     strings.Replace(valid, `"value":"fixture-secret"`, `"value":{"nested":"fixture-secret"}`, 1),
		"legacy false prohibited": strings.Replace(valid, `"path":"/"`, `"path":"/","legacy":false`, 1),
		"quoted false prohibited": strings.Replace(valid, `"path":"/"`, `"path":"/","quoted":false`, 1),
		"unknown cookie field":    strings.Replace(valid, `"path":"/"`, `"path":"/","max_age":60`, 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) { rejectRainBrowserPayload(t, []byte(input)) })
	}
	t.Run("invalid UTF8", func(t *testing.T) {
		rejectRainBrowserPayload(t, bytes.Replace([]byte(valid), []byte("fixture-secret"), []byte{0xff}, 1))
	})
}

func rejectRainBrowserPayload(t *testing.T, data []byte) {
	t.Helper()
	encoded, err := ImportRainBrowserSession(data)
	if encoded != "" || !errors.Is(err, errRainSession) || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("invalid browser payload accepted or error disclosed input")
	}
}

func TestImportRainBrowserSessionRejectsInvalidCookies(t *testing.T) {
	for _, name := range []string{"name", "value", "domain", "path"} {
		t.Run("missing "+name, func(t *testing.T) {
			cookie := rainBrowserCookieFixture()
			delete(cookie, name)
			rejectRainBrowserPayload(t, rainBrowserPayload(t, cookie))
		})
	}
	for _, name := range []string{"name", "value", "domain", "path", "secure", "http_only", "same_site", "expires"} {
		t.Run("null "+name, func(t *testing.T) {
			cookie := rainBrowserCookieFixture()
			cookie[name] = nil
			rejectRainBrowserPayload(t, rainBrowserPayload(t, cookie))
		})
	}
	cases := []struct {
		field string
		value any
	}{
		{"name", ""}, {"name", "bad name"}, {"name", "bad;name"},
		{"value", "fixture-secret\r\nInjected: yes"}, {"value", "bad;value"},
		{"domain", ""}, {"domain", ".www.yuketang.cn"}, {"domain", "evil.example"},
		{"domain", "www.yuketang.cn.evil.example"}, {"domain", "cn"}, {"domain", "www.yuketang.cn:443"},
		{"path", ""}, {"path", "api"}, {"path", "/api;invalid"}, {"path", "/api\r\n"},
		{"secure", "true"}, {"http_only", 1},
		{"same_site", 0}, {"same_site", -1}, {"same_site", 5}, {"same_site", "Lax"}, {"same_site", true},
		{"expires", ""}, {"expires", 1800000000}, {"expires", "2030-01-01"},
		{"expires", "2030-01-01T1:00:00Z"}, {"expires", "2030-01-01T00:00:00,5Z"},
		{"expires", "2030-01-01T00:00:00+24:00"}, {"expires", "2030-01-01T00:00:00+08:60"},
	}
	for i, test := range cases {
		t.Run(test.field+"-"+strconv.Itoa(i), func(t *testing.T) {
			cookie := rainBrowserCookieFixture()
			cookie[test.field] = test.value
			rejectRainBrowserPayload(t, rainBrowserPayload(t, cookie))
		})
	}
	for _, domain := range []string{"yuketang.cn", ".YUKETANG.CN"} {
		t.Run("duplicate canonical scope "+domain, func(t *testing.T) {
			first, second := rainBrowserCookieFixture(), rainBrowserCookieFixture()
			first["domain"], second["domain"] = ".yuketang.cn", domain
			second["value"] = "second"
			rejectRainBrowserPayload(t, rainBrowserPayload(t, first, second))
		})
	}
}

func TestImportRainBrowserSessionRequiresEffectiveValidationCookies(t *testing.T) {
	for _, expired := range []string{"2000-01-01T00:00:00Z", "0001-01-01T00:00:00Z"} {
		cookie := rainBrowserCookieFixture()
		cookie["expires"] = expired
		rejectRainBrowserPayload(t, rainBrowserPayload(t, cookie))
	}
	for _, change := range []map[string]any{{"path": "/unrelated"}, {"value": ""}} {
		cookie := rainBrowserCookieFixture()
		for key, value := range change {
			cookie[key] = value
		}
		rejectRainBrowserPayload(t, rainBrowserPayload(t, cookie))
	}
	cookie := map[string]any{"name": "sessionid", "value": "fixture-secret", "domain": "www.yuketang.cn", "path": "/api"}
	if _, err := ImportRainBrowserSession(rainBrowserPayload(t, cookie)); err != nil {
		t.Fatal("API-scoped session cookie was rejected", err)
	}
}

func TestImportRainBrowserSessionBounds(t *testing.T) {
	valid := rainBrowserPayload(t, rainBrowserCookieFixture())
	padded := append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), maxRainSession-len(valid))...)
	if _, err := ImportRainBrowserSession(padded); err != nil {
		t.Fatal("exact body bound rejected", err)
	}
	rejectRainBrowserPayload(t, append(padded, ' '))
	cookies := make([]map[string]any, 0, maxRainCookies+1)
	for i := 0; i <= maxRainCookies; i++ {
		cookies = append(cookies, map[string]any{"name": "c" + strconv.Itoa(i), "value": "v", "domain": "www.yuketang.cn", "path": "/"})
	}
	if _, err := ImportRainBrowserSession(rainBrowserPayload(t, cookies[:maxRainCookies]...)); err != nil {
		t.Fatal("exact cookie count rejected", err)
	}
	rejectRainBrowserPayload(t, rainBrowserPayload(t, cookies...))
	first, second := rainBrowserCookieFixture(), rainBrowserCookieFixture()
	first["name"], second["name"] = "a", "b"
	first["value"], second["value"] = strings.Repeat("x", 16378), strings.Repeat("x", 16378)
	if _, err := ImportRainBrowserSession(rainBrowserPayload(t, first, second)); err != nil {
		t.Fatal("exact header bound rejected", err)
	}
	second["value"] = strings.Repeat("x", 16379)
	rejectRainBrowserPayload(t, rainBrowserPayload(t, first, second))
	largePath := rainBrowserCookieFixture()
	largePath["path"] = "/" + strings.Repeat("x", 32768)
	rejectRainBrowserPayload(t, rainBrowserPayload(t, largePath))
	// Output encoding may expand a legal input; bound the saved form as well.
	ampersands := []byte(`{"version":1,"cookies":[{"name":"sessionid","value":"` + strings.Repeat("&", 12000) + `","domain":"www.yuketang.cn","path":"/"}]}`)
	rejectRainBrowserPayload(t, ampersands)
}
