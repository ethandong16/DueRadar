package platform

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func zhidaoCookieDomain(host string) bool {
	return host == "zhihuishu.com" || host == "polymas.com" || endsWithDomain(host, "zhihuishu.com") || endsWithDomain(host, "polymas.com")
}

func cookieRecordKey(record zhidaoCookie) string {
	domain := strings.TrimPrefix(strings.ToLower(record.Cookie.Domain), ".")
	if domain == "" {
		u, _ := url.Parse(record.URL)
		domain = strings.ToLower(u.Hostname())
	}
	return domain + "\x00" + record.Cookie.Path + "\x00" + record.Cookie.Name
}

// SetCookies records the current cookie set using absolute expiry times. A
// restart must never restart a Max-Age countdown or revive a deleted cookie.
func (j *zhidaoJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || !zhidaoCookieDomain(host) {
		return
	}
	for _, c := range cookies {
		if c == nil || c.Valid() != nil {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if domain != "" && (!zhidaoCookieDomain(domain) || (host != domain && !endsWithDomain(host, domain))) {
			continue
		}
		clone := *c
		if clone.Path == "" || clone.Path[0] != '/' {
			clone.Path = "/"
			if i := strings.LastIndex(u.Path, "/"); i > 0 {
				clone.Path = u.Path[:i]
			}
		}
		if clone.MaxAge > 0 {
			// Clamp like the Rain jar: a server-supplied Max-Age near the int64
			// limit would otherwise overflow into the past and silently drop a
			// refreshed session cookie.
			last := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
			if int64(clone.MaxAge) > last.Unix()-now.Unix() {
				clone.Expires = last
			} else {
				clone.Expires = time.Unix(now.Unix()+int64(clone.MaxAge), 0).UTC()
			}
			clone.MaxAge = 0
		}
		j.CookieJar.SetCookies(u, []*http.Cookie{&clone})
		record := zhidaoCookie{URL: u.Scheme + "://" + u.Host + "/", Cookie: &clone}
		key := cookieRecordKey(record)
		kept := j.records[:0]
		for _, previous := range j.records {
			if cookieRecordKey(previous) != key && (previous.Cookie.Expires.IsZero() || previous.Cookie.Expires.After(now)) {
				kept = append(kept, previous)
			}
		}
		j.records = kept
		if clone.MaxAge >= 0 && (clone.Expires.IsZero() || clone.Expires.After(now)) {
			j.records = append(j.records, record)
		}
	}
}

func (j *zhidaoJar) snapshot() []zhidaoCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	result := make([]zhidaoCookie, 0, len(j.records))
	for _, record := range j.records {
		if !record.Cookie.Expires.IsZero() && !record.Cookie.Expires.After(now) {
			continue
		}
		clone := *record.Cookie
		result = append(result, zhidaoCookie{URL: record.URL, Cookie: &clone})
	}
	return result
}

func (z *Zhidao) refreshToken() {
	if z.client == nil || z.client.Jar == nil {
		return
	}
	for _, name := range []string{"jt-cas", "ai-poly", "openapi-token", "cloudapi-token"} {
		for _, origin := range []string{z.base, zhidaoCenter} {
			u, err := url.Parse(origin)
			if err != nil {
				continue
			}
			for _, cookie := range z.client.Jar.Cookies(u) {
				if cookie.Name == name && cookie.Value != "" {
					z.session.Token = cookie.Value
					if decoded, err := url.PathUnescape(cookie.Value); err == nil {
						z.session.Token = decoded
					}
					return
				}
			}
		}
	}
}

// Credentials returns an updated session for the encrypted vault only. Callers
// must save it under the same generation guard as the completed sync so a stale
// operation cannot restore a credential after logout or a replacement login.
func (z *Zhidao) Credentials() (string, error) {
	if z.initErr != nil {
		return "", z.initErr
	}
	z.refreshToken()
	session := z.session
	if jar, ok := z.client.Jar.(*zhidaoJar); ok {
		session.Cookies = jar.snapshot()
	}
	data, err := json.Marshal(session)
	return string(data), err
}

// ZhidaoSessionExpiry reads the absolute expiry of the stored jt-cas JWT. The
// other tokens in the same session share its server-side lifetime, so this is
// the deadline the renewal scheduler must beat. It returns false when the
// stored value predates the JWT format or cannot be decoded; callers must not
// overwrite a session whose expiry is unknown.
func ZhidaoSessionExpiry(serialized string) (time.Time, bool) {
	if strings.TrimSpace(serialized) == "" {
		return time.Time{}, false
	}
	var session ZhidaoSession
	if json.Unmarshal([]byte(serialized), &session) != nil {
		return time.Time{}, false
	}
	for _, record := range session.Cookies {
		if record.Cookie != nil && record.Cookie.Name == "jt-cas" {
			if expiry, ok := jwtExpiry(record.Cookie.Value); ok {
				return expiry, true
			}
		}
	}
	return jwtExpiry(session.Token)
}

func jwtExpiry(token string) (time.Time, bool) {
	if decoded, err := url.PathUnescape(token); err == nil {
		token = decoded
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
