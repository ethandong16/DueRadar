package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

type analysisTransport func(*http.Request) (*http.Response, error)

func (f analysisTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func analysisSSE(text, reason string, stop bool) string {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","content":[],"model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text),
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":2}}`, reason),
	}
	if stop {
		events = append(events, `{"type":"message_stop"}`)
	}
	var b strings.Builder
	for _, event := range events {
		var envelope struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(event), &envelope)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", envelope.Type, event)
	}
	return b.String()
}

func analysisResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAnalysisConfigurationAuthRetentionClearAndSecrecy(t *testing.T) {
	s, h := testApp(t)
	if response := request(h, "GET", "/api/analysis", "", nil, ""); response.Code != 401 {
		t.Fatal("analysis configuration is public")
	}
	cookie := loginCookie(t, h)
	var modelCalls atomic.Int32
	s.analyzeProblem = func(context.Context, vault.Config, platform.ClassroomProblem) (string, error) {
		modelCalls.Add(1)
		return "", nil
	}
	const key = "fixture-private-analysis-key"
	body := `{"key":"` + key + `","model":"claude-opus-5-5"}`
	if response := request(h, "PUT", "/api/analysis", body, cookie, "https://attacker.invalid"); response.Code != 403 {
		t.Fatal("cross-site model configuration accepted")
	}
	response := request(h, "PUT", "/api/analysis", body, cookie, "")
	if response.Code != 200 || strings.Contains(response.Body.String(), key) || s.Vault.Get().AnalysisAPIKey != key {
		t.Fatal("configuration failed or disclosed key")
	}
	if response := request(h, "PUT", "/api/analysis", `{"key":"","model":"claude-sonnet-5-5"}`, cookie, ""); response.Code != 200 || s.Vault.Get().AnalysisAPIKey != key {
		t.Fatal("blank key did not retain tenant credential")
	}
	for _, path := range []string{"/api/analysis", "/api/settings", "/api/status", "/api/events"} {
		response := request(h, "GET", path, "", cookie, "")
		if strings.Contains(response.Body.String(), key) {
			t.Fatalf("key leaked in %s", path)
		}
	}
	state, _ := json.Marshal(s.Engine.Snapshot())
	if bytes.Contains(state, []byte(key)) {
		t.Fatal("key leaked into task state")
	}
	if response := request(h, "PUT", "/api/analysis", `{"clear_key":true,"model":"claude-sonnet-5-5"}`, cookie, ""); response.Code != 200 || s.Vault.Get().AnalysisAPIKey != "" {
		t.Fatal("key removal failed")
	}
	if modelCalls.Load() != 0 {
		t.Fatal("saving a key called the paid API")
	}
}

func TestAnalysisConfigurationValidationAndDemo(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	for _, body := range []string{
		`{"key":"abc","model":"https://attacker.invalid"}`,
		`{"key":"bad\nkey","model":"claude-opus-5-5"}`,
		`{"key":"abc","clear_key":true}`,
		`{"base_url":"https://attacker.invalid"}`,
		`{"model":"CLAUDE-opus-5-5"}`,
	} {
		if response := request(h, "PUT", "/api/analysis", body, cookie, ""); response.Code != 400 {
			t.Fatalf("invalid config accepted: %s", body)
		}
	}
	if s.Vault.Get().AnalysisAPIKey != "" {
		t.Fatal("invalid request changed key")
	}
	s.Demo = true
	if response := request(h, "PUT", "/api/analysis", `{"key":"fixture"}`, cookie, ""); response.Code != 409 {
		t.Fatal("demo saved model credentials")
	}
}

func TestAnalysisConfigurationTenantIsolation(t *testing.T) {
	m, h := multiTestOpen(t, t.TempDir(), "", true, false)
	a := multiTestRegister(t, h, "alpha", multiTestPasswordA)
	b := multiTestRegister(t, h, "bravo", multiTestPasswordB)
	body := `{"key":"fixture-alpha-key","model":"claude-sonnet-5-5"}`
	multiTestStatus(t, multiTestRequest(h, "PUT", "/api/analysis", body, a.cookie), 200)
	if multiTestWorker(t, m, a.id).Vault.Get().AnalysisAPIKey != "fixture-alpha-key" || multiTestWorker(t, m, b.id).Vault.Get().AnalysisAPIKey != "" {
		t.Fatal("tenant model keys crossed accounts")
	}
	response := multiTestRequest(h, "GET", "/api/analysis", "", b.cookie)
	if !strings.Contains(response.Body.String(), `"configured":false`) || strings.Contains(response.Body.String(), "fixture-alpha") {
		t.Fatal("another account sees analysis configuration")
	}
}

func TestAnalysisSDKStreamCompletionAndInput(t *testing.T) {
	for _, tc := range []struct {
		name, text, reason string
		stop, want         bool
	}{
		{"success", "A: fixture", "end_turn", true, true},
		{"stop sequence", "B", "stop_sequence", true, true},
		{"truncated", "A", "max_tokens", true, false},
		{"refused", "not allowed", "refusal", true, false},
		{"missing message stop", "A", "end_turn", false, false},
		{"empty", "  ", "end_turn", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/messages" || r.Method != "POST" || r.Header.Get("Cookie") != "" {
					t.Error("wrong model endpoint or leaked platform cookie")
				}
				var body struct {
					Model     string
					Stream    bool
					MaxTokens int64 `json:"max_tokens"`
					Thinking  struct{ Type string }
					Tools     []any
					Messages  []any
					System    []any
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != defaultAnalysisModel || !body.Stream || body.Thinking.Type != "adaptive" || body.MaxTokens != 2048 || len(body.Tools) != 0 || len(body.System) == 0 {
					t.Errorf("incorrect SDK request shape: %+v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, analysisSSE(tc.text, tc.reason, tc.stop))
			}))
			t.Cleanup(srv.Close)
			client := anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithAPIKey("fixture-key"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
			answer, err := analyzeWithClient(context.Background(), client, vault.Config{}, serverClassroomProblem())
			if (err == nil) != tc.want || calls.Load() != 1 {
				t.Fatalf("answer=%q err=%v calls=%d", answer, err, calls.Load())
			}
			if tc.want && answer != tc.text {
				t.Fatal("SDK response text was not accumulated")
			}
		})
	}
}

func TestAnalysisDefaultClientIgnoresHostCredentialsHeadersAndOrigin(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fixture-host-key")
	t.Setenv("ANTHROPIC_BASE_URL", "https://attacker.invalid")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Api-Key: fixture-other-host-key\nX-Host-Secret: fixture-secret")
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	var calls atomic.Int32
	http.DefaultTransport = analysisTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Scheme != "https" || r.URL.Host != "api.anthropic.com" || r.URL.Path != "/v1/messages" {
			t.Error("model destination changed by host environment")
		}
		if r.Header.Get("Authorization") != "Bearer fixture-tenant-key" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Host-Secret") != "" || r.Header.Get("Cookie") != "" {
			t.Error("model credentials/headers not isolated to tenant")
		}
		return analysisResponse(analysisSSE("A", "end_turn", true), 200), nil
	})
	if answer, err := defaultAnalyzeProblem(context.Background(), vault.Config{AnalysisAPIKey: "fixture-tenant-key"}, serverClassroomProblem()); err != nil || answer != "A" {
		t.Fatalf("default SDK client: %q, %v", answer, err)
	}
	if _, err := defaultAnalyzeProblem(context.Background(), vault.Config{}, serverClassroomProblem()); err == nil || calls.Load() != 1 {
		t.Fatal("missing tenant key used host credentials")
	}
}

func TestAnalysisDoesNotRetryFailuresOrFollowRedirects(t *testing.T) {
	for _, status := range []int{429, 500, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			original := http.DefaultTransport
			defer func() { http.DefaultTransport = original }()
			var calls atomic.Int32
			http.DefaultTransport = analysisTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				response := analysisResponse(`{"error":{"type":"api_error","message":"fixture private key"}}`, status)
				response.Header.Set("Location", "https://attacker.invalid")
				return response, nil
			})
			_, err := defaultAnalyzeProblem(context.Background(), vault.Config{AnalysisAPIKey: "fixture-key"}, serverClassroomProblem())
			if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "fixture private key") {
				t.Fatalf("failure retries/leak: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestAnalysisImageBoundaries(t *testing.T) {
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	var calls atomic.Int32
	body, status := "\x89PNG\r\n\x1a\nfixture", 200
	http.DefaultTransport = analysisTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("image request leaked credentials")
		}
		response := analysisResponse(body, status)
		response.Header.Set("Location", "https://attacker.invalid")
		return response, nil
	})
	for _, url := range []string{"http://qn-sfe.yuketang.cn/a.png", "https://127.0.0.1/a.png", "https://qn-sfe.yuketang.cn.attacker.invalid/a.png", "https://qn-sfe.yuketang.cn:443/a.png", "https://user@qn-sfe.yuketang.cn/a.png", "https://qn-sfe.yuketang.cn/a.png#fragment"} {
		if _, _, err := analysisImage(context.Background(), url); err == nil {
			t.Fatalf("unsafe image URL accepted: %s", url)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid image address performed I/O")
	}
	if data, media, err := analysisImage(context.Background(), "https://qn-sfe.yuketang.cn/a.png"); err != nil || media != "image/png" || len(data) == 0 {
		t.Fatal("valid image rejected")
	}
	for _, mode := range []string{"html", "oversize", "redirect"} {
		switch mode {
		case "html":
			body = "<html>fixture</html>"
		case "oversize":
			body = strings.Repeat("x", (4<<20)+1)
		case "redirect":
			status = 307
		}
		before := calls.Load()
		if _, _, err := analysisImage(context.Background(), "https://qn-sfe.yuketang.cn/a.png"); err == nil || calls.Load() != before+1 {
			t.Fatalf("image %s boundary failed", mode)
		}
	}
}

func TestAnalysisContextCancellation(t *testing.T) {
	client := anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithAPIKey("fixture-key"), option.WithMaxRetries(0), option.WithHTTPClient(&http.Client{Transport: analysisTransport(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := analyzeWithClient(ctx, client, vault.Config{}, serverClassroomProblem()); err == nil || errors.Is(ctx.Err(), nil) {
		t.Fatal("model job ignored context cancellation")
	}
}
