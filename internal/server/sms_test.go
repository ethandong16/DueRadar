package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeSMSSource struct {
	pages [][]smsMessage
	calls int
	err   error
}

func (f *fakeSMSSource) Recent(context.Context) ([]smsMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	index := f.calls
	if index >= len(f.pages) {
		index = len(f.pages) - 1
	}
	f.calls++
	return f.pages[index], nil
}

func TestSMSCodeExtraction(t *testing.T) {
	cases := []struct {
		text string
		want string
		ok   bool
	}{
		{"【智慧树】您正在登录，验证码：246810，5分钟内有效", "246810", true},
		{"验证码 1234", "1234", true},
		{"【知到】动态码为 98765 请勿泄露", "98765", true},
		{"6789 是您的登录验证码，请勿转发", "6789", true},
		{"您的快递已到驿站，取件码 123456", "", false},
		{"账单 2026 年 11 月", "", false},
		{"验证码错误", "", false},
	}
	for _, tc := range cases {
		got, ok := smsCode(tc.text)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("smsCode(%q) = %q, %v; want %q, %v", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSMSCollectorIgnoresBacklogAndUnrelatedMessages(t *testing.T) {
	now := time.Now()
	source := &fakeSMSSource{pages: [][]smsMessage{
		{{ID: 10, Sender: "1069", Text: "【智慧树】登录验证码：111111", ImportedAtUTC: now.Add(-time.Minute).Format(time.RFC3339)}},
		{
			{ID: 11, Sender: "1069", Text: "您的快递取件码 222222", ImportedAtUTC: now.Format(time.RFC3339)},
			{ID: 10, Sender: "1069", Text: "【智慧树】登录验证码：111111", ImportedAtUTC: now.Add(-time.Minute).Format(time.RFC3339)},
		},
		{
			{ID: 12, Sender: "1069", Text: "【智慧树】您正在登录，验证码：246810，5分钟内有效", ImportedAtUTC: now.Add(2 * time.Second).Format(time.RFC3339)},
			{ID: 11, Sender: "1069", Text: "您的快递取件码 222222", ImportedAtUTC: now.Format(time.RFC3339)},
			{ID: 10, Sender: "1069", Text: "【智慧树】登录验证码：111111", ImportedAtUTC: now.Add(-time.Minute).Format(time.RFC3339)},
		},
	}}
	collector := &smsCollector{source: source, keywords: []string{"验证码", "智慧树", "知到"}, poll: time.Millisecond, wait: time.Second}
	baseline, err := collector.Baseline(context.Background())
	if err != nil || baseline != 10 {
		t.Fatalf("baseline = %d, %v", baseline, err)
	}
	code, err := collector.WaitForCode(context.Background(), baseline, now.Add(-15*time.Second))
	if err != nil || code != "246810" {
		t.Fatalf("code = %q, %v", code, err)
	}
}

func TestSMSCollectorRejectsStaleImport(t *testing.T) {
	now := time.Now()
	source := &fakeSMSSource{pages: [][]smsMessage{{
		{ID: 20, Sender: "1069", Text: "【智慧树】验证码：333333", ImportedAtUTC: now.Add(-10 * time.Minute).Format(time.RFC3339)},
	}}}
	collector := &smsCollector{source: source, keywords: []string{"智慧树"}, poll: time.Millisecond, wait: 5 * time.Millisecond}
	if _, err := collector.WaitForCode(context.Background(), 10, now); err == nil {
		t.Fatal("stale message accepted")
	}
}

func TestHTTPSSourceRecent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sms/recent" || r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []smsMessage{{ID: 7, Sender: "1069", Text: "验证码 1234"}}})
	}))
	defer server.Close()
	source := &httpSMSSource{base: server.URL, token: "token", client: server.Client()}
	messages, err := source.Recent(context.Background())
	if err != nil || len(messages) != 1 || messages[0].ID != 7 {
		t.Fatalf("messages = %+v, %v", messages, err)
	}
	if _, err := (&httpSMSSource{base: server.URL, token: "wrong", client: server.Client()}).Recent(context.Background()); err == nil {
		t.Fatal("unauthorized source accepted")
	}
}

func TestNewSMSSourceRequiresBothEnvValues(t *testing.T) {
	t.Setenv("DUERADAR_SMS_API_URL", "http://127.0.0.1:1")
	t.Setenv("DUERADAR_SMS_API_TOKEN", "")
	if newSMSSourceFromEnv() != nil {
		t.Fatal("missing token must disable the SMS source")
	}
	t.Setenv("DUERADAR_SMS_API_TOKEN", "token")
	if newSMSSourceFromEnv() == nil {
		t.Fatal("configured SMS source was not created")
	}
	if !strings.Contains(strings.Join(newSMSCollector(nil).keywords, ","), "智慧树") {
		t.Fatal("default keywords missing")
	}
}
