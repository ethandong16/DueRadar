package bark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrPermanent marks failures that retrying the same request cannot fix, such
// as a rejected device key or malformed server URL. The delivery queue stops
// retrying these immediately instead of spending its retry budget.
var ErrPermanent = errors.New("bark rejected the notification")

// ErrTransient marks network, timeout and server-side failures that may
// succeed on a later attempt.
var ErrTransient = errors.New("bark is temporarily unavailable")

type responseError struct {
	status     int
	code       int
	retryAfter time.Duration
	permanent  bool
}

func (e *responseError) Error() string {
	if e.code != 0 {
		return fmt.Sprintf("Bark 返回 HTTP %d（代码 %d）", e.status, e.code)
	}
	return fmt.Sprintf("Bark 返回 HTTP %d", e.status)
}

func (e *responseError) Unwrap() error {
	if e.permanent {
		return ErrPermanent
	}
	return ErrTransient
}

func (e *responseError) RetryAfter() time.Duration { return e.retryAfter }
func (e *responseError) HTTPStatus() int          { return e.status }

func permanentStatus(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return true
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return 0
}

func Validate(server, key string) error {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("Bark 服务地址必须是无账号、查询参数或片段的 HTTPS 地址")
	}
	if strings.TrimSpace(key) == "" || len(key) > 512 {
		return errors.New("请填写有效的 Bark Key")
	}
	return nil
}

// Options are the optional delivery controls supported by the official Bark
// server. Wake is deliberately loud: critical level bypasses silent mode (when
// the Bark app has critical-alert permission), volume is raised to the maximum,
// and call repeats the alert sound for about 30 seconds.
type Options struct {
	Level  string
	Volume int
	Call   bool
	Sound  string
}

// Wake is the strongest alert Bark offers for a short-lived classroom vote.
func Wake() Options {
	return Options{Level: "critical", Volume: 10, Call: true}
}

func Send(ctx context.Context, server, key, title, body string) error {
	return SendWithOptions(ctx, server, key, title, body, Options{})
}

func SendWithOptions(ctx context.Context, server, key, title, body string, opts Options) error {
	if err := Validate(server, key); err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	payload := map[string]any{"device_key": key, "title": title, "body": body, "group": "DueRadar"}
	if opts.Level != "" {
		payload["level"] = opts.Level
	}
	if opts.Volume > 0 {
		payload["volume"] = opts.Volume
	}
	if opts.Call {
		payload["call"] = 1
	}
	if opts.Sound != "" {
		payload["sound"] = opts.Sound
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(server, "/")+"/push", bytes.NewReader(data))
	if err != nil {
		return errors.New("Bark 请求构造失败")
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: 连接失败，请检查网络与服务地址", ErrTransient)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return &responseError{status: response.StatusCode, permanent: permanentStatus(response.StatusCode), retryAfter: parseRetryAfter(response.Header.Get("Retry-After"))}
	}
	var result struct {
		Code int `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&result); err != nil {
		return fmt.Errorf("%w: Bark 返回无效 JSON", ErrTransient)
	}
	if result.Code != 200 {
		return &responseError{status: response.StatusCode, code: result.Code, permanent: result.Code >= 400 && result.Code < 500}
	}
	return nil
}
