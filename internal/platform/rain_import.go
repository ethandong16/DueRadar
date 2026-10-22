package platform

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var rainBrowserExpiry = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`)

// ImportRainBrowserSession validates an untrusted browser export and converts it
// to the existing saved-session format. It performs no requests and does not
// establish authentication; callers must independently validate the account.
func ImportRainBrowserSession(data []byte) (string, error) {
	if len(data) == 0 || len(data) > maxRainSession || !utf8.Valid(data) {
		return "", errRainSession
	}
	object, err := rainImportObject(data, "version", "cookies")
	if err != nil {
		return "", errRainSession
	}
	var version int
	var cookies []json.RawMessage
	if json.Unmarshal(object["version"], &version) != nil || version != 1 || json.Unmarshal(object["cookies"], &cookies) != nil || len(cookies) == 0 || len(cookies) > maxRainCookies {
		return "", errRainSession
	}
	session := rainSavedSession{Version: 1, Cookies: make([]rainStoredCookie, 0, len(cookies))}
	seen := make(map[string]bool, len(cookies))
	headerSize := 0
	now := time.Now()
	for _, raw := range cookies {
		fields, err := rainImportObject(raw, "name", "value", "domain", "path", "expires", "secure", "http_only", "same_site")
		if err != nil {
			return "", errRainSession
		}
		c := rainStoredCookie{SameSite: http.SameSiteDefaultMode}
		for name, target := range map[string]*string{"name": &c.Name, "value": &c.Value, "domain": &c.Domain, "path": &c.Path} {
			if json.Unmarshal(fields[name], target) != nil {
				return "", errRainSession
			}
		}
		c.Domain = strings.ToLower(c.Domain)
		switch c.Domain {
		case "www.yuketang.cn", ".yuketang.cn":
		case "yuketang.cn":
			c.Domain = ".yuketang.cn"
		default:
			return "", errRainSession
		}
		for name, target := range map[string]*bool{"secure": &c.Secure, "http_only": &c.HTTPOnly} {
			if raw, ok := fields[name]; ok && json.Unmarshal(raw, target) != nil {
				return "", errRainSession
			}
		}
		if raw, ok := fields["same_site"]; ok {
			if json.Unmarshal(raw, &c.SameSite) != nil || c.SameSite < http.SameSiteDefaultMode || c.SameSite > http.SameSiteNoneMode {
				return "", errRainSession
			}
		}
		_, hasExpiry := fields["expires"]
		if hasExpiry {
			var value string
			if json.Unmarshal(fields["expires"], &value) != nil || !rainBrowserExpiry.MatchString(value) {
				return "", errRainSession
			}
			// time.Parse currently accepts some out-of-range zone offsets.
			if value[len(value)-1] != 'Z' {
				zone := value[len(value)-6:]
				if zone[1:3] > "23" || zone[4:6] > "59" {
					return "", errRainSession
				}
			}
			c.Expires, err = time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return "", errRainSession
			}
			c.Expires = c.Expires.UTC()
		}
		if c.Path == "" || c.Path[0] != '/' || len(c.Name)+len(c.Value)+len(c.Domain)+len(c.Path) > 32768 || c.cookie().Valid() != nil || seen[c.key()] {
			return "", errRainSession
		}
		seen[c.key()] = true
		headerSize += len(c.Name) + len(c.Value) + 5
		if headerSize > 32768 {
			return "", errRainSession
		}
		if !hasExpiry || c.Expires.After(now) {
			session.Cookies = append(session.Cookies, c)
		}
	}
	encoded, err := encodeRainSession(session)
	if err != nil {
		return "", errRainSession
	}
	jar, err := restoreRainSession(encoded)
	if err != nil {
		return "", errRainSession
	}
	validationURL, _ := url.Parse(rainOrigin + "/api/v3/user/basic-info")
	for _, cookie := range jar.Cookies(validationURL) {
		if cookie.Value != "" {
			return encoded, nil
		}
	}
	return "", errRainSession
}

// Decode exact field names rather than the case-insensitive struct matching in
// encoding/json. Token keys also expose duplicates, including escaped names.
func rainImportObject(data []byte, allowed ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errRainSession
	}
	fields := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, errRainSession
		}
		known := false
		for _, candidate := range allowed {
			if name == candidate {
				known = true
				break
			}
		}
		if _, duplicate := fields[name]; !known || duplicate {
			return nil, errRainSession
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errRainSession
		}
		fields[name] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, errRainSession
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errRainSession
	}
	return fields, nil
}
