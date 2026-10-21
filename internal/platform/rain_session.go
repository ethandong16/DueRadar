package platform

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

const rainOrigin = "https://www.yuketang.cn"
const maxRainSession = 64 << 10
const maxRainCookies = 256

var errRainSession = errors.New("雨课堂会话格式无效")

// The cookie source is fixed to rainOrigin, so an origin URL is neither needed
// nor accepted in stored credentials. Relative Max-Age never enters this format.
type rainStoredCookie struct {
	Name     string        `json:"name"`
	Value    string        `json:"value"`
	Domain   string        `json:"domain,omitempty"`
	Path     string        `json:"path"`
	Expires  time.Time     `json:"expires,omitzero"`
	Secure   bool          `json:"secure,omitempty"`
	HTTPOnly bool          `json:"http_only,omitempty"`
	SameSite http.SameSite `json:"same_site,omitempty"`
	Quoted   bool          `json:"quoted,omitempty"`
	// Raw Cookie headers omit scope. Keep that uncertainty until the server
	// supplies authoritative metadata for the same name and path.
	Legacy bool `json:"legacy,omitempty"`
}

type rainSavedSession struct {
	Version int                `json:"version"`
	Cookies []rainStoredCookie `json:"cookies"`
}

func (c rainStoredCookie) cookie() *http.Cookie {
	return &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Expires: c.Expires, Secure: c.Secure, HttpOnly: c.HTTPOnly, SameSite: c.SameSite, Quoted: c.Quoted}
}

func (c rainStoredCookie) key() string {
	domain := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
	if domain == "" {
		domain = "www.yuketang.cn"
	}
	return domain + "\x00" + c.Path + "\x00" + c.Name
}

func rainCookieScope(domain string) bool {
	switch strings.TrimPrefix(strings.ToLower(domain), ".") {
	case "", "yuketang.cn", "www.yuketang.cn":
		return true
	}
	return false
}

func rainCookieOrigin(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Host == "www.yuketang.cn" && u.User == nil
}

type rainCookieJar struct {
	mu      sync.Mutex
	jar     http.CookieJar
	records []rainStoredCookie
	err     error
}

func newRainCookieJar() *rainCookieJar {
	jar, _ := cookiejar.New(nil)
	return &rainCookieJar{jar: jar, records: []rainStoredCookie{}}
}

func (j *rainCookieJar) Cookies(u *url.URL) []*http.Cookie {
	if !rainCookieOrigin(u) {
		return nil
	}
	return j.jar.Cookies(u)
}

func (j *rainCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if !rainCookieOrigin(u) {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return
	}
	if len(cookies) > maxRainCookies {
		j.err = errRainSession
		return
	}
	now := time.Now()
	for _, cookie := range cookies {
		if cookie != nil && len(cookie.Name)+len(cookie.Value)+len(cookie.Domain)+len(cookie.Path) > 32768 {
			j.err = errRainSession
			return
		}
		if cookie == nil || cookie.Valid() != nil || !rainCookieScope(cookie.Domain) {
			continue
		}
		c := *cookie
		c.Domain = strings.ToLower(c.Domain)
		if c.Path == "" || c.Path[0] != '/' {
			c.Path = "/"
			if i := strings.LastIndex(u.Path, "/"); i > 0 {
				c.Path = u.Path[:i]
			}
		}
		if c.MaxAge > 0 {
			last := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
			if int64(c.MaxAge) > last.Unix()-now.Unix() {
				c.Expires = last
			} else {
				c.Expires = time.Unix(now.Unix()+int64(c.MaxAge), int64(now.Nanosecond())).UTC()
			}
			c.MaxAge = 0
		}
		c.Expires = c.Expires.UTC()
		record := rainStoredCookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Expires: c.Expires, Secure: c.Secure, HTTPOnly: c.HttpOnly, SameSite: c.SameSite, Quoted: c.Quoted}
		remove := c.MaxAge < 0 || !c.Expires.IsZero() && !c.Expires.After(now)
		found := false
		kept := j.records[:0]
		for _, previous := range j.records {
			if previous.Legacy && previous.Name == record.Name && previous.Path == record.Path && previous.key() != record.key() {
				// A parent-domain renewal must replace an imported placeholder,
				// otherwise its old value sorts ahead of the renewed value.
				deleted := previous.cookie()
				deleted.MaxAge = -1
				j.jar.SetCookies(u, []*http.Cookie{deleted})
				continue
			}
			if previous.key() == record.key() {
				found = true
				if !remove {
					kept = append(kept, record)
				}
			} else if previous.Expires.IsZero() || previous.Expires.After(now) {
				kept = append(kept, previous)
			}
		}
		j.records = kept
		if !remove && !found {
			if len(j.records) == maxRainCookies {
				j.err = errRainSession
				return
			}
			j.records = append(j.records, record)
		}
		if _, err := encodeRainSession(rainSavedSession{Version: 1, Cookies: j.records}); err != nil {
			j.err = errRainSession
			return
		}
		j.jar.SetCookies(u, []*http.Cookie{&c})
	}
}

func (j *rainCookieJar) check() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

func (j *rainCookieJar) snapshot() (rainSavedSession, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	session := rainSavedSession{Version: 1, Cookies: []rainStoredCookie{}}
	if j.err != nil {
		return session, j.err
	}
	for _, cookie := range j.records {
		if cookie.Expires.IsZero() || cookie.Expires.After(time.Now()) {
			session.Cookies = append(session.Cookies, cookie)
		}
	}
	return session, nil
}

func encodeRainSession(session rainSavedSession) (string, error) {
	// Bound both storage and the worst-case Cookie header independently.
	headerSize := 0
	for _, cookie := range session.Cookies {
		headerSize += len(cookie.Name) + len(cookie.Value) + 5
	}
	if len(session.Cookies) > maxRainCookies || headerSize > 32768 {
		return "", errRainSession
	}
	data, err := json.Marshal(session)
	if err != nil || len(data) > maxRainSession {
		return "", errRainSession
	}
	return string(data), nil
}

func restoreRainSession(credentials string) (*rainCookieJar, error) {
	if len(credentials) > maxRainSession {
		return nil, errRainSession
	}
	credentials = strings.TrimSpace(credentials)
	if credentials == "" {
		return nil, ErrSessionExpired
	}
	jar := newRainCookieJar()
	origin, _ := url.Parse(rainOrigin)
	if strings.HasPrefix(credentials, "{") {
		var session rainSavedSession
		decoder := json.NewDecoder(strings.NewReader(credentials))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&session) != nil || session.Version != 1 || session.Cookies == nil {
			return nil, errRainSession
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, errRainSession
		}
		if _, err := encodeRainSession(session); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, record := range session.Cookies {
			if record.Path == "" || record.Path[0] != '/' || !rainCookieScope(record.Domain) || record.cookie().Valid() != nil || seen[record.key()] || record.SameSite < 0 || record.SameSite > http.SameSiteNoneMode {
				return nil, errRainSession
			}
			if record.Legacy && (record.Domain != "" || record.Path != "/" || !record.Expires.IsZero()) {
				return nil, errRainSession
			}
			seen[record.key()] = true
			// Loading metadata is not a new server response. Preserve known
			// scopes and legacy uncertainty without reconciling either here.
			if record.Expires.IsZero() || record.Expires.After(time.Now()) {
				jar.records = append(jar.records, record)
				jar.jar.SetCookies(origin, []*http.Cookie{record.cookie()})
			}
		}
	} else {
		if len(credentials) > 32768 || strings.ContainsAny(credentials, "\r\n") {
			return nil, errRainSession
		}
		cookies, err := http.ParseCookie(credentials)
		if err != nil || len(cookies) == 0 {
			return nil, errRainSession
		}
		for _, cookie := range cookies {
			cookie.Path = "/"
			cookie.Secure = true
		}
		jar.SetCookies(origin, cookies)
		for i := range jar.records {
			jar.records[i].Legacy = true
		}
		if _, err := encodeRainSession(rainSavedSession{Version: 1, Cookies: jar.records}); err != nil {
			return nil, err
		}
	}
	return jar, jar.check()
}

func rainSessionClient(client *http.Client, jar *rainCookieJar) *http.Client {
	copy := *client
	copy.Jar = jar
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func (r *Rain) ensureSession() error {
	r.once.Do(func() {
		r.jar, r.initErr = restoreRainSession(r.cookie)
		if r.initErr == nil {
			r.client = rainSessionClient(r.client, r.jar)
		}
	})
	if r.initErr != nil {
		return r.initErr
	}
	return r.jar.check()
}

func (l *RainLogin) ensureSession() error {
	l.once.Do(func() {
		l.jar = newRainCookieJar()
		l.client = rainSessionClient(l.client, l.jar)
	})
	return l.jar.check()
}

// Credentials exports the latest session for encrypted storage only. Save it
// under the same generation guard as the successful login or complete sync.
func (r *Rain) Credentials() (string, error) {
	if err := r.ensureSession(); err != nil {
		return "", err
	}
	session, err := r.jar.snapshot()
	if err != nil {
		return "", err
	}
	return encodeRainSession(session)
}
