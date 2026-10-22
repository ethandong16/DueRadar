package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

const assistBrowserSession = `{"version":1,"cookies":[{"name":"sessionid","value":"synthetic-secret","domain":"www.yuketang.cn","path":"/","secure":true,"http_only":true,"same_site":2}]}`

type assistCreated struct {
	ID      string    `json:"flow_id"`
	Code    string    `json:"pairing_code"`
	Expires time.Time `json:"expires_at"`
	Seconds int       `json:"expires_in"`
}

func createAssist(t *testing.T, h http.Handler, admin *http.Cookie) assistCreated {
	t.Helper()
	w := request(h, "POST", "/api/accounts/rain/assist", "", admin, "")
	var flow assistCreated
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &flow) != nil || len(flow.ID) != 64 || len(flow.Code) != 64 || flow.Seconds != 300 {
		t.Fatalf("create: status=%d body=%s", w.Code, w.Body.String())
	}
	return flow
}

func helperRequest(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func connectAssist(t *testing.T, h http.Handler, flow assistCreated) string {
	t.Helper()
	w := helperRequest(h, "POST", "/api/accounts/rain/assist/"+flow.ID+"/connect", "", flow.Code)
	var result struct {
		Token   string    `json:"upload_token"`
		State   string    `json:"state"`
		Expires time.Time `json:"expires_at"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Token) != 64 || result.State != "awaiting_user" || !result.Expires.Equal(flow.Expires) {
		t.Fatalf("connect: status=%d body=%s", w.Code, w.Body.String())
	}
	return result.Token
}

func assistState(s *Server, id string) string {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	return s.rainAssists[id].state
}

func TestRainAssistCapabilityIsolationAndOneUse(t *testing.T) {
	s, h := testApp(t)
	admin := loginCookie(t, h)
	flow := createAssist(t, h, admin)
	base := "/api/accounts/rain/assist/" + flow.ID
	for _, path := range []string{base, base + "/cancel"} {
		method := "GET"
		if strings.HasSuffix(path, "/cancel") {
			method = "POST"
		}
		if w := request(h, method, path, "", nil, ""); w.Code != 401 {
			t.Fatal("management capability without admin session", w.Code)
		}
	}
	if w := helperRequest(h, "POST", base+"/connect", "", strings.Repeat("0", 64)); w.Code != 401 {
		t.Fatal("wrong pairing capability accepted", w.Code)
	}
	if w := helperRequest(h, "GET", base+"/helper", "", flow.Code); w.Code != 401 {
		t.Fatal("pairing capability used as upload capability", w.Code)
	}
	token := connectAssist(t, h, flow)
	if w := helperRequest(h, "POST", base+"/connect", "", flow.Code); w.Code != 401 {
		t.Fatal("pairing replay accepted", w.Code)
	}
	s.rainMu.Lock()
	if s.rainAssists[flow.ID].pairing != [32]byte{} || s.rainAssists[flow.ID].upload != sha256.Sum256([]byte(token)) {
		t.Error("capabilities were not consumed/hashed")
	}
	s.rainMu.Unlock()
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		for _, endpoint := range []string{"helper", "session", "connect"} {
			method := "POST"
			if endpoint == "helper" {
				method = "GET"
			}
			r := httptest.NewRequest(method, base+"/"+endpoint, nil)
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set(header, "") // Presence alone is rejected, even same-origin.
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("browser helper header %s/%s: %d", header, endpoint, w.Code)
			}
		}
	}
	for _, w := range []*httptest.ResponseRecorder{
		request(h, "GET", base, "", admin, ""),
		helperRequest(h, "GET", base+"/helper", "", token),
		request(h, "GET", "/api/status", "", admin, ""),
	} {
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), flow.Code) {
			t.Fatal("status credential disclosure/cache/auth failure", w.Code)
		}
	}
	if w := request(h, "POST", base+"/cancel", "", admin, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := helperRequest(h, "GET", base+"/helper", "", token); w.Code != 401 {
		t.Fatal("canceled helper remains authorized", w.Code)
	}
}

func TestRainAssistValidatesBeforePersistingAndPreservesBark(t *testing.T) {
	s, h := testApp(t)
	admin := loginCookie(t, h)
	directory, _ := seedRenewal(t, s)
	entered, release := make(chan struct{}), make(chan struct{})
	var validations atomic.Int32
	s.newRain = func(input string) rainAdapter {
		return renewingZhidao{fakeRainAdapter: fakeRainAdapter{
			validate: func(context.Context) error {
				if validations.Add(1) == 1 {
					if !strings.Contains(input, "synthetic-secret") {
						t.Error("imported browser session not independently validated")
					}
					close(entered)
					<-release
				}
				return nil
			},
			tasks: func(context.Context) ([]core.Task, error) { return nil, errors.New("task read unavailable") },
		}, export: func() (string, error) { return "renewed-validated-session", nil }}
	}
	flow := createAssist(t, h, admin)
	token := connectAssist(t, h, flow)
	base := "/api/accounts/rain/assist/" + flow.ID
	w := helperRequest(h, "POST", base+"/session", assistBrowserSession, token)
	if w.Code != 202 || w.Body.String() != "{\"state\":\"verifying\"}\n" {
		t.Fatalf("async upload response: %d %s", w.Code, w.Body.String())
	}
	await(t, entered)
	if s.Vault.Get().RainCookie != "original" || loginState(s) != "waiting" || assistState(s, flow.ID) != "verifying" {
		t.Fatal("unvalidated credentials replaced current account")
	}
	if w := helperRequest(h, "POST", base+"/session", assistBrowserSession, token); w.Code != 409 {
		t.Fatal("session upload replay accepted", w.Code)
	}
	if err := s.Vault.Update(func(c *vault.Config) error { c.BarkKey = "concurrent-bark"; return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	eventually(t, func() bool { return assistState(s, flow.ID) == "logged_in" })
	config := s.Vault.Get()
	if config.RainCookie != "renewed-validated-session" || config.BarkKey != "concurrent-bark" {
		t.Fatal("renewal or concurrent setting lost")
	}
	reopened, err := vault.Open(directory)
	if err != nil || reopened.Get() != config {
		t.Fatal("validated session was not durable", err)
	}
	if w := helperRequest(h, "GET", base+"/helper", "", token); w.Code != 200 || !strings.Contains(w.Body.String(), "logged_in") {
		t.Fatal("upload capability cannot read terminal outcome", w.Code)
	}
	eventually(t, func() bool { return renewalAccount(t, s, core.PlatformRain).SyncState == "error" })
	if assistState(s, flow.ID) != "logged_in" {
		t.Fatal("task read failure changed independently validated login status")
	}
}

func TestRainAssistInvalidUploadIsTerminalAndConsumed(t *testing.T) {
	for _, body := range []string{"not json", `{"version":1,"cookies":[]}`, strings.Repeat("x", (64<<10)+1), assistBrowserSession + `{}`} {
		t.Run(body[:min(len(body), 12)], func(t *testing.T) {
			s, h := testApp(t)
			admin := loginCookie(t, h)
			var called atomic.Bool
			s.newRain = func(string) rainAdapter { called.Store(true); return fakeRainAdapter{} }
			flow := createAssist(t, h, admin)
			token := connectAssist(t, h, flow)
			base := "/api/accounts/rain/assist/" + flow.ID
			if w := helperRequest(h, "POST", base+"/session", body, token); w.Code != 400 || strings.Contains(w.Body.String(), "synthetic-secret") {
				t.Fatal("invalid payload not rejected generically", w.Code)
			}
			if w := helperRequest(h, "POST", base+"/session", assistBrowserSession, token); w.Code != 409 {
				t.Fatal("invalid payload did not consume upload", w.Code)
			}
			if called.Load() || s.Vault.Get().RainCookie != "" || assistState(s, flow.ID) != "failed" || loginState(s) != "unbound" {
				t.Fatal("invalid payload started provider validation or persisted state")
			}
		})
	}
}

func TestRainAssistStaleValidationCannotPersist(t *testing.T) {
	for _, action := range []string{"cancel", "expire", "new_assist", "new_qr", "logout", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			s, h := testApp(t)
			admin := loginCookie(t, h)
			entered, release := make(chan struct{}), make(chan struct{})
			s.newRain = func(string) rainAdapter {
				return fakeRainAdapter{validate: func(context.Context) error { close(entered); <-release; return nil }}
			}
			flow := createAssist(t, h, admin)
			token := connectAssist(t, h, flow)
			base := "/api/accounts/rain/assist/" + flow.ID
			if w := helperRequest(h, "POST", base+"/session", assistBrowserSession, token); w.Code != 202 {
				t.Fatal(w.Code)
			}
			await(t, entered)
			switch action {
			case "cancel":
				request(h, "POST", base+"/cancel", "", admin, "")
			case "expire":
				s.rainMu.Lock()
				s.rainAssists[flow.ID].expires = time.Now().Add(-time.Second)
				s.rainMu.Unlock()
				if w := helperRequest(h, "GET", base+"/helper", "", token); w.Code != 401 {
					t.Fatal("expired helper remains authorized", w.Code)
				}
			case "new_assist":
				newFlow := createAssist(t, h, admin)
				request(h, "POST", base+"/cancel", "", admin, "")
				request(h, "POST", "/api/accounts/rain/login/cancel", "", admin, "")
				if assistState(s, newFlow.ID) != "awaiting_helper" {
					t.Fatal("old cancel canceled a later assist flow")
				}
			case "new_qr":
				s.startRain = func(context.Context) (*rainFlow, error) {
					return &rainFlow{wait: func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }}, nil
				}
				if w := request(h, "POST", "/api/accounts/rain/login", "", admin, ""); w.Code != 200 {
					t.Fatal(w.Code)
				}
				request(h, "POST", base+"/cancel", "", admin, "")
				if rainState(s) != "waiting" {
					t.Fatal("old assist cancel canceled newer QR")
				}
			case "logout":
				request(h, "POST", "/api/logout", "", admin, "")
			case "shutdown":
				s.stopBackground()
			}
			close(release)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := s.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if s.Vault.Get().RainCookie != "" || assistState(s, flow.ID) == "logged_in" || loginState(s) == "logged_in" {
				t.Fatal("stale verification overwrote canceled/new generation")
			}
		})
	}
}

func TestRainAssistFailuresNeverClaimSuccess(t *testing.T) {
	for _, failure := range []string{"validate", "export", "vault", "state"} {
		t.Run(failure, func(t *testing.T) {
			s, h := testApp(t)
			admin := loginCookie(t, h)
			vaultDir, _ := seedRenewal(t, s)
			stateDir := t.TempDir()
			engine, err := core.Open(filepath.Join(stateDir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			s.Engine = engine
			entered, release := make(chan struct{}), make(chan struct{})
			var once atomic.Bool
			s.newRain = func(input string) rainAdapter {
				if input == "original" { // Restored old account sync stays synthetic.
					return fakeRainAdapter{validate: func(context.Context) error { return errors.New("old session") }}
				}
				return renewingZhidao{fakeRainAdapter: fakeRainAdapter{validate: func(context.Context) error {
					if once.CompareAndSwap(false, true) {
						close(entered)
						<-release
					}
					if failure == "validate" {
						return errors.New("SECRET_PROVIDER_BODY")
					}
					return nil
				}}, export: func() (string, error) {
					if failure == "export" {
						return "", errors.New("SECRET_PROVIDER_BODY")
					}
					return "validated", nil
				}}
			}
			flow := createAssist(t, h, admin)
			token := connectAssist(t, h, flow)
			base := "/api/accounts/rain/assist/" + flow.ID
			if w := helperRequest(h, "POST", base+"/session", assistBrowserSession, token); w.Code != 202 {
				t.Fatal(w.Code)
			}
			await(t, entered)
			var restore func()
			if failure == "vault" {
				restore = blockStateDirectory(t, vaultDir)
			}
			if failure == "state" {
				restore = blockStateDirectory(t, stateDir)
			}
			if restore != nil {
				defer restore()
			}
			close(release)
			eventually(t, func() bool { return assistState(s, flow.ID) == "failed" })
			if failure != "state" && s.Vault.Get().RainCookie != "original" {
				t.Fatal("failed validation/persistence replaced original credentials")
			}
			if failure == "state" && s.Vault.Get().RainCookie != "validated" {
				t.Fatal("valid persisted rotation was discarded after state failure")
			}
			w := helperRequest(h, "GET", base+"/helper", "", token)
			if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET_PROVIDER_BODY") || strings.Contains(w.Body.String(), "synthetic-secret") {
				t.Fatal("failure leaked provider details", w.Code)
			}
		})
	}
}

func TestRainAssistLogoutIsScopedToOwner(t *testing.T) {
	s, h := testApp(t)
	owner, other := loginCookie(t, h), loginCookie(t, h)
	flow := createAssist(t, h, owner)
	request(h, "POST", "/api/logout", "", other, "")
	if assistState(s, flow.ID) != "awaiting_helper" {
		t.Fatal("another admin session logout canceled owner flow")
	}
	request(h, "POST", "/api/logout", "", owner, "")
	if assistState(s, flow.ID) != "canceled" {
		t.Fatal("owner logout retained active flow")
	}
}

func TestRainAssistConcurrentConnectHasOneWinner(t *testing.T) {
	_, h := testApp(t)
	admin := loginCookie(t, h)
	flow := createAssist(t, h, admin)
	var accepted atomic.Int32
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := helperRequest(h, "POST", "/api/accounts/rain/assist/"+flow.ID+"/connect", "", flow.Code)
			if w.Code == 200 {
				accepted.Add(1)
			} else if w.Code != 401 {
				t.Errorf("unexpected connect result %d", w.Code)
			}
		}()
	}
	workers.Wait()
	if accepted.Load() != 1 {
		t.Fatal("pairing capability was not single-use", accepted.Load())
	}
}

func TestRainAssistIdleExpiryAndShutdown(t *testing.T) {
	for _, phase := range []string{"awaiting_helper", "awaiting_user"} {
		for _, cause := range []string{"expiry", "shutdown"} {
			t.Run(phase+"/"+cause, func(t *testing.T) {
				s, h := testApp(t)
				admin := loginCookie(t, h)
				flow := createAssist(t, h, admin)
				token := ""
				if phase == "awaiting_user" {
					token = connectAssist(t, h, flow)
				}
				base := "/api/accounts/rain/assist/" + flow.ID
				if cause == "expiry" {
					s.rainMu.Lock()
					s.rainAssists[flow.ID].expires = time.Now().Add(-time.Second)
					s.rainMu.Unlock()
					if w := request(h, "GET", base, "", admin, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "expired") {
						t.Fatal("idle expiry not visible", w.Code)
					}
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if err := s.Shutdown(ctx); err != nil {
						t.Fatal(err)
					}
					if assistState(s, flow.ID) != "canceled" {
						t.Fatal("shutdown left idle assist active")
					}
				}
				if w := helperRequest(h, "POST", base+"/connect", "", flow.Code); w.Code != 401 {
					t.Fatal("finished pairing still exchangeable")
				}
				if w := helperRequest(h, "GET", base+"/helper", "", token); w.Code != 401 {
					t.Fatal("finished helper still usable")
				}
				if loginState(s) != "unbound" {
					t.Fatal("idle flow did not restore account")
				}
			})
		}
	}
}

func TestRainAssistReplacesPendingQRWithoutOldWriteback(t *testing.T) {
	s, h := testApp(t)
	admin := loginCookie(t, h)
	entered, release := make(chan struct{}), make(chan struct{})
	s.startRain = func(context.Context) (*rainFlow, error) {
		return &rainFlow{wait: func(context.Context) (string, error) { close(entered); <-release; return "old-qr-session", nil }}, nil
	}
	s.newRain = func(string) rainAdapter { return fakeRainAdapter{} }
	if w := request(h, "POST", "/api/accounts/rain/login", "", admin, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	await(t, entered)
	flow := createAssist(t, h, admin)
	close(release)
	// Drain both generations; a late QR result remains stale even if its fake
	// provider ignores cancellation and reports success.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Vault.Get().RainCookie != "" || assistState(s, flow.ID) == "logged_in" {
		t.Fatal("old QR replaced assisted login")
	}
}

type stalledAssistBody struct{ entered, release chan struct{} }

func (b *stalledAssistBody) Read([]byte) (int, error) {
	close(b.entered)
	<-b.release
	return 0, io.EOF
}

func TestRainAssistSlowBodyDoesNotBlockCancellation(t *testing.T) {
	for _, endpoint := range []string{"connect", "session"} {
		t.Run(endpoint, func(t *testing.T) {
			s, h := testApp(t)
			admin := loginCookie(t, h)
			flow := createAssist(t, h, admin)
			token := flow.Code
			if endpoint == "session" {
				token = connectAssist(t, h, flow)
			}
			base := "/api/accounts/rain/assist/" + flow.ID
			body := &stalledAssistBody{make(chan struct{}), make(chan struct{})}
			r := httptest.NewRequest("POST", base+"/"+endpoint, nil)
			r.Body = io.NopCloser(body)
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			done := make(chan struct{})
			go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), r) }()
			await(t, body.entered)
			canceled := make(chan struct{})
			go func() { defer close(canceled); request(h, "POST", base+"/cancel", "", admin, "") }()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				close(body.release)
				t.Fatal("request body held provider lock against cancel")
			}
			close(body.release)
			await(t, done)
			if assistState(s, flow.ID) != "canceled" || s.Vault.Get().RainCookie != "" {
				t.Fatal("late body revived canceled flow")
			}
		})
	}
}

func TestRainAssistInitialStateWriteFailureAndInvalidContentType(t *testing.T) {
	t.Run("initial_state", func(t *testing.T) {
		s, h := testApp(t)
		admin := loginCookie(t, h)
		dir := t.TempDir()
		engine, err := core.Open(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		s.Engine = engine
		restore := blockStateDirectory(t, dir)
		defer restore()
		w := request(h, "POST", "/api/accounts/rain/assist", "", admin, "")
		if w.Code != 500 || strings.Contains(w.Body.String(), "pairing_code") {
			t.Fatal("initial storage failure issued credentials", w.Code)
		}
		s.rainMu.Lock()
		defer s.rainMu.Unlock()
		if s.rainAssistActive != nil || s.rainState != "failed" {
			t.Fatal("initial persistence failure retained active flow")
		}
	})
	t.Run("content_type", func(t *testing.T) {
		s, h := testApp(t)
		admin := loginCookie(t, h)
		flow := createAssist(t, h, admin)
		token := connectAssist(t, h, flow)
		path := "/api/accounts/rain/assist/" + flow.ID + "/session"
		r := httptest.NewRequest("POST", path, strings.NewReader(assistBrowserSession))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || assistState(s, flow.ID) != "failed" {
			t.Fatal("non JSON upload accepted", w.Code)
		}
		if replay := helperRequest(h, "POST", path, assistBrowserSession, token); replay.Code != 409 {
			t.Fatal("invalid content type did not consume upload")
		}
	})
}
