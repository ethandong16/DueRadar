package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

type fakeRainAdapter struct {
	validate func(context.Context) error
	tasks    func(context.Context) ([]core.Task, error)
}

func (f fakeRainAdapter) Validate(ctx context.Context) error {
	if f.validate != nil {
		return f.validate(ctx)
	}
	return nil
}
func (f fakeRainAdapter) Tasks(ctx context.Context) ([]core.Task, error) {
	if f.tasks != nil {
		return f.tasks(ctx)
	}
	return nil, nil
}

func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for test barrier")
	}
}
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func loginState(s *Server) string {
	for _, a := range s.Engine.Snapshot().Accounts {
		if a.Platform == core.PlatformRain {
			return a.LoginState
		}
	}
	return ""
}
func rainState(s *Server) string { s.rainMu.Lock(); defer s.rainMu.Unlock(); return s.rainState }

func TestCancelRainLoginWhileCreatingQR(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	entered := make(chan struct{})
	returned := make(chan struct{})
	var waited atomic.Bool
	s.startRain = func(ctx context.Context) (*rainFlow, error) {
		close(entered)
		<-ctx.Done()
		return &rainFlow{wait: func(context.Context) (string, error) { waited.Store(true); return "", nil }}, nil
	}
	var startStatus int
	go func() {
		defer close(returned)
		startStatus = request(h, "POST", "/api/accounts/rain/login", "{}", cookie, "").Code
	}()
	await(t, entered)
	if w := request(h, "POST", "/api/accounts/rain/login/cancel", "{}", cookie, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	await(t, returned)
	if startStatus != 409 || waited.Load() || rainState(s) != "canceled" || loginState(s) != "unbound" {
		t.Fatal("canceled creation revived a login")
	}
}

func TestCancelDoesNotPromoteAnExpiredSavedSession(t *testing.T) {
	s, h := testApp(t)
	admin := loginCookie(t, h)
	if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "fixture-expired"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "expired"}); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	s.startRain = func(context.Context) (*rainFlow, error) {
		return &rainFlow{wait: func(ctx context.Context) (string, error) { close(waiting); <-ctx.Done(); return "", ctx.Err() }}, nil
	}
	s.newRain = func(string) rainAdapter {
		return fakeRainAdapter{validate: func(context.Context) error { return platform.ErrSessionExpired }}
	}
	if w := request(h, "POST", "/api/accounts/rain/login", "{}", admin, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	await(t, waiting)
	request(h, "POST", "/api/accounts/rain/login/cancel", "{}", admin, "")
	if loginState(s) != "expired" {
		t.Fatal("cancel promoted an unverified expired session")
	}
}

func TestOlderQRCreationCannotReplaceNewerLogin(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstReturned := make(chan struct{})
	newWaiting := make(chan struct{})
	var calls atomic.Int32
	s.startRain = func(ctx context.Context) (*rainFlow, error) {
		if calls.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst // Model a late response even after cancellation.
			return &rainFlow{image: []byte("older"), wait: func(context.Context) (string, error) { t.Error("superseded QR started polling"); return "", nil }}, nil
		}
		return &rainFlow{image: []byte("newer"), wait: func(ctx context.Context) (string, error) { close(newWaiting); <-ctx.Done(); return "", ctx.Err() }}, nil
	}
	var firstStatus int
	go func() {
		defer close(firstReturned)
		firstStatus = request(h, "POST", "/api/accounts/rain/login", "{}", cookie, "").Code
	}()
	await(t, firstEntered)
	if w := request(h, "POST", "/api/accounts/rain/login", "{}", cookie, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	await(t, newWaiting)
	close(releaseFirst)
	await(t, firstReturned)
	if firstStatus != 409 || rainState(s) != "waiting" || loginState(s) != "waiting" {
		t.Fatal("old creation overwrote current login")
	}
	request(h, "POST", "/api/accounts/rain/login/cancel", "{}", cookie, "")
}

func TestRebindCancelsOldSyncAndQueuesMandatoryNewSync(t *testing.T) {
	for _, oldFails := range []bool{false, true} {
		t.Run(fmt.Sprint(oldFails), func(t *testing.T) {
			s, h := testApp(t)
			admin := loginCookie(t, h)
			if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "fixture-old"; return nil }); err != nil {
				t.Fatal(err)
			}
			oldStarted := make(chan struct{})
			oldCanceled := make(chan struct{})
			releaseOld := make(chan struct{})
			newCollected := make(chan struct{})
			end := time.Now().Add(24 * time.Hour)
			s.newRain = func(cookie string) rainAdapter {
				if cookie == "fixture-old" {
					return fakeRainAdapter{tasks: func(ctx context.Context) ([]core.Task, error) {
						close(oldStarted)
						<-ctx.Done()
						close(oldCanceled)
						<-releaseOld
						if oldFails {
							return nil, platform.ErrSessionExpired
						}
						return []core.Task{{ID: "old", Title: "Old account", Kind: "homework", Status: "pending", EndAt: &end}}, nil
					}}
				}
				return fakeRainAdapter{tasks: func(context.Context) ([]core.Task, error) {
					close(newCollected)
					return []core.Task{{ID: "new", Title: "New account", Kind: "homework", Status: "pending", EndAt: &end}}, nil
				}}
			}
			s.startRain = func(context.Context) (*rainFlow, error) {
				return &rainFlow{wait: func(context.Context) (string, error) { return "fixture-new", nil }}, nil
			}
			if !s.queueSync() {
				t.Fatal("sync rejected")
			}
			await(t, oldStarted)
			if w := request(h, "POST", "/api/accounts/rain/login", "{}", admin, ""); w.Code != 200 {
				t.Fatal(w.Code)
			}
			await(t, oldCanceled)
			eventually(t, func() bool { return s.Vault.Get().RainCookie == "fixture-new" && rainState(s) == "logged_in" })
			close(releaseOld)
			await(t, newCollected)
			eventually(t, func() bool { state := s.Engine.Snapshot(); return len(state.Tasks) == 1 && state.Tasks[0].ID == "new" })
			for _, a := range s.Engine.Snapshot().Accounts {
				if a.Platform == core.PlatformRain && (a.ConsecutiveFailures != 0 || a.LoginState != "logged_in" || a.NeedsRevalidation) {
					t.Fatalf("old sync contaminated new account: %+v", a)
				}
			}
		})
	}
}

func TestShutdownCancelsAndWaitsForOwnedJobs(t *testing.T) {
	s, _ := testApp(t)
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	if !s.startJob(func(ctx context.Context) { close(started); <-ctx.Done(); close(canceled); <-release }) {
		t.Fatal("job rejected")
	}
	await(t, started)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		finished <- s.Shutdown(ctx)
	}()
	await(t, canceled)
	select {
	case err := <-finished:
		t.Fatalf("shutdown returned before job cleanup: %v", err)
	default:
	}
	if s.startJob(func(context.Context) { t.Error("job started after shutdown") }) {
		t.Fatal("new job accepted")
	}
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestShutdownDeadlineIsBounded(t *testing.T) {
	s, _ := testApp(t)
	release := make(chan struct{})
	s.startJob(func(context.Context) { <-release })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wanted bounded shutdown, got %v", err)
	}
	close(release)
}

func TestShutdownCancelsRainWaitAndRestoresAccount(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	waiting := make(chan struct{})
	s.startRain = func(context.Context) (*rainFlow, error) {
		return &rainFlow{wait: func(ctx context.Context) (string, error) { close(waiting); <-ctx.Done(); return "", ctx.Err() }}, nil
	}
	if w := request(h, "POST", "/api/accounts/rain/login", "{}", cookie, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	await(t, waiting)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if rainState(s) != "canceled" || loginState(s) != "unbound" {
		t.Fatal("shutdown left active QR login state")
	}
}

func TestSyncTimeoutCountsAsOneFailure(t *testing.T) {
	s, _ := testApp(t)
	if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "fixture"; return nil }); err != nil {
		t.Fatal(err)
	}
	s.newRain = func(string) rainAdapter {
		return fakeRainAdapter{tasks: func(ctx context.Context) ([]core.Task, error) { <-ctx.Done(); return nil, ctx.Err() }}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Sync(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("sync deadline did not propagate")
	}
	for _, a := range s.Engine.Snapshot().Accounts {
		if a.Platform == core.PlatformRain && a.ConsecutiveFailures != 1 {
			t.Fatal("normal sync timeout was not recorded as a failed round")
		}
	}
}

func TestConcurrentBlankBarkSavesPreserveLatestRotation(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	if w := request(h, "PUT", "/api/bark", `{"server":"https://api.day.app","key":"rotation-0"}`, cookie, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				w := request(h, "PUT", "/api/bark", `{"server":"https://api.day.app","key":""}`, cookie, "")
				if w.Code != 200 {
					t.Error(w.Code)
				}
			}
		}()
	}
	for i := 1; i <= 20; i++ {
		w := request(h, "PUT", "/api/bark", fmt.Sprintf(`{"server":"https://api.day.app","key":"rotation-%d"}`, i), cookie, "")
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	wg.Wait()
	if s.Vault.Get().BarkKey != "rotation-20" {
		t.Fatal("blank save restored an obsolete key")
	}
}

func deliveryApp(t *testing.T) (*Server, string) {
	t.Helper()
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	e, err := core.Open(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(filepath.Join(base, "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(e, v, "test-password-long-enough", false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	settings := e.Snapshot().Settings
	settings.QuietEnabled = false
	if err = e.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err = v.Update(func(c *vault.Config) error { c.BarkURL = "https://api.day.app"; c.BarkKey = "fixture-key"; return nil }); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	end := now.Add(time.Hour)
	if err = e.Sync(core.PlatformRain, []core.Task{{ID: "one", Title: "First", Kind: "homework", Status: "pending", EndAt: &end}, {ID: "two", Title: "Second", Kind: "homework", Status: "pending", EndAt: &end}}, now); err != nil {
		t.Fatal(err)
	}
	return s, stateDir
}

func blockStateDirectory(t *testing.T, dir string) func() {
	t.Helper()
	backup := dir + "-saved"
	if err := os.Rename(dir, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("fixture block"), 0600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, dir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeliveredOutcomeRetriesPersistenceBeforeAnotherPush(t *testing.T) {
	s, dir := deliveryApp(t)
	sends := 0
	var restore func()
	s.sendBark = func(context.Context, string, string, string, string) error {
		sends++
		if sends == 1 {
			restore = blockStateDirectory(t, dir)
		}
		return nil
	}
	s.Deliver(context.Background())
	if sends != 1 || s.deliveryResult == nil {
		t.Fatal("batch continued after outcome persistence failed")
	}
	w := httptest.NewRecorder()
	s.status(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if !strings.Contains(w.Body.String(), "通知结果保存失败") || strings.Contains(w.Body.String(), dir) {
		t.Fatal("storage failure not safely surfaced")
	}
	s.Deliver(context.Background())
	if sends != 1 {
		t.Fatal("successful push repeated during storage outage")
	}
	restore()
	s.Deliver(context.Background())
	if sends != 2 || s.deliveryResult != nil {
		t.Fatal("persisted outcome was resent or next unclaimed event was not delivered")
	}
	sent := 0
	for _, event := range s.Engine.Snapshot().Events {
		if event.Status == "sent" {
			sent++
		}
	}
	if sent != 2 {
		t.Fatal("successful outcome missing from durable state")
	}
}

func TestClaimPersistenceFailureDoesNotSend(t *testing.T) {
	s, dir := deliveryApp(t)
	sends := 0
	s.sendBark = func(context.Context, string, string, string, string) error { sends++; return nil }
	restore := blockStateDirectory(t, dir)
	s.Deliver(context.Background())
	if sends != 0 {
		t.Fatal("push sent without durable claim")
	}
	s.mu.Lock()
	message := s.deliveryError
	s.mu.Unlock()
	if message == "" {
		t.Fatal("claim persistence failure hidden")
	}
	restore()
	s.Deliver(context.Background())
	if sends != 2 {
		t.Fatal("delivery did not recover")
	}
}

func TestCompletionDuringPushStillRecordsActualDelivery(t *testing.T) {
	s, _ := deliveryApp(t)
	sends := 0
	s.sendBark = func(context.Context, string, string, string, string) error {
		sends++
		tasks := s.Engine.Snapshot().Tasks
		for i := range tasks {
			tasks[i].Status = "completed"
		}
		return s.Engine.Sync(core.PlatformRain, tasks, time.Now())
	}
	s.Deliver(context.Background())
	if sends != 1 || s.deliveryResult != nil {
		t.Fatal("completion interfered with delivery outcome")
	}
	sent := 0
	for _, event := range s.Engine.Snapshot().Events {
		if event.Status == "sent" {
			sent++
		}
	}
	if sent != 1 {
		t.Fatal("accepted push not recorded after concurrent completion")
	}
}
