package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

// A lost registry must not allow a new installation or bootstrap owner to be
// created over data whose account mapping could still be recovered from backup.
func TestMultiMissingRegistryFailsClosed(t *testing.T) {
	dir := t.TempDir()
	m, handler := multiTestOpen(t, dir, "", true, false)
	user := multiTestRegister(t, handler, "existing-user", multiTestPasswordA)
	multiTestShutdown(t, m)
	registry := filepath.Join(dir, "users.json")
	original, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(registry); err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"", multiTestPasswordA} {
		if reopened, err := OpenMulti(dir, password, true, true, false); err == nil {
			_ = reopened.Shutdown(context.Background())
			t.Fatal("missing user registry was silently replaced")
		}
		if _, err := os.Stat(registry); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed startup wrote a registry: %v", err)
		}
	}
	if err := os.WriteFile(registry, original, 0600); err != nil {
		t.Fatal(err)
	}
	_, restoredHandler := multiTestOpen(t, dir, "", true, false)
	if restored := multiTestLogin(t, restoredHandler, user.username, multiTestPasswordA); restored.id != user.id {
		t.Fatal("restored registry did not reopen the original account")
	}
}

func TestMultiLoginRetryReservation(t *testing.T) {
	m := &MultiServer{attempts: map[string]multiAttempt{}}
	now := time.Now()
	for i := 0; i < 10; i++ {
		if !m.allowLoginAttemptLocked("same-ip", now) || !m.recordLoginFailureLocked("same-ip", now) {
			t.Fatalf("failure %d was unexpectedly rejected", i+1)
		}
	}
	if m.allowLoginAttemptLocked("same-ip", now) {
		t.Fatal("the burst limit did not defer the next password verification")
	}
	next := now.Add(loginRetryInterval)
	if !m.allowLoginAttemptLocked("same-ip", next) {
		t.Fatal("retry remained locked after its interval")
	}
	if m.allowLoginAttemptLocked("same-ip", next) {
		t.Fatal("parallel retry was not reserved")
	}
	if !m.recordLoginFailureLocked("same-ip", next) || m.allowLoginAttemptLocked("same-ip", next) {
		t.Fatal("another failed password did not renew the retry interval")
	}
	if !m.allowLoginAttemptLocked("another-ip", now) {
		t.Fatal("the retry interval affected another address")
	}
	if !m.allowLoginAttemptLocked("same-ip", now.Add(15*time.Minute)) {
		t.Fatal("the failure budget did not expire")
	}
}

// A pre-rebind request that reaches Bark after the replacement must not mark
// a newly created event with the same platform-local ID as sent or failed.
func TestRebindFencesInFlightDelivery(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "accepted"}[sent], func(t *testing.T) {
			s, _ := deliveryApp(t)
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseSend := func() { once.Do(func() { close(release) }) }
			defer releaseSend()
			s.sendBark = func(context.Context, string, string, string, string) error {
				close(started)
				<-release
				if !sent {
					return errors.New("synthetic transport failure")
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { defer close(done); s.Deliver(ctx) }()
			await(t, started)
			oldTasks := s.Engine.Snapshot().Tasks
			if err := s.Engine.ResetPlatformForRebind(core.PlatformRain); err != nil {
				t.Fatal(err)
			}
			for i := range oldTasks {
				oldTasks[i].Title = "Replacement account task"
			}
			if err := s.Engine.Sync(core.PlatformRain, oldTasks, time.Now()); err != nil {
				t.Fatal(err)
			}
			cancel()
			releaseSend()
			await(t, done)
			if s.deliveryResult != nil {
				t.Fatal("superseded outcome was treated as a storage failure")
			}
			for _, event := range s.Engine.Snapshot().Events {
				if event.Status == "sent" || event.Attempts != 0 {
					t.Fatalf("old transport altered new account event: %+v", event)
				}
			}
		})
	}
}

// The live poll may renew a cookie while an older full-sync request is still
// in flight. An expired result from the old credential cannot expire the new
// one, and a follow-up sync must use the renewed credential.
func TestRainLiveRenewalQueuesSyncAndFencesOldFailure(t *testing.T) {
	s, _ := testApp(t)
	if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "fixture-old"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Sync(core.PlatformRain, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	oldStarted, releaseOld, newCollected := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce, collectedOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOld) }) }
	defer release()
	s.newRain = func(cookie string) rainAdapter {
		if cookie == "fixture-renewed" {
			return fakeRainAdapter{tasks: func(context.Context) ([]core.Task, error) {
				collectedOnce.Do(func() { close(newCollected) })
				return []core.Task{{ID: "new", Title: "Fresh task", Kind: "homework", Status: "pending"}}, nil
			}}
		}
		return renewingZhidao{fakeRainAdapter: fakeRainAdapter{
			validate: func(context.Context) error { close(oldStarted); <-releaseOld; return platform.ErrSessionExpired },
		}, export: func() (string, error) { return "fixture-renewed", nil }}
	}
	if !s.queueSync() {
		t.Fatal("sync was not queued")
	}
	await(t, oldStarted)
	// Use a separate live adapter to model a credential rotation from polling.
	s.rainMu.Lock()
	if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "fixture-renewed"; return nil }); err != nil {
		s.rainMu.Unlock()
		t.Fatal(err)
	}
	if s.rainSyncCancel == nil || !s.queueSync() {
		s.rainMu.Unlock()
		t.Fatal("follow-up sync was not queued")
	}
	s.rainMu.Unlock()
	release()
	await(t, newCollected)
	eventually(t, func() bool {
		state := s.Engine.Snapshot()
		for _, task := range state.Tasks {
			if task.ID == "new" {
				return true
			}
		}
		return false
	})
	account := renewalAccount(t, s, core.PlatformRain)
	if account.LoginState != "logged_in" || account.NeedsRevalidation || account.ConsecutiveFailures != 0 {
		t.Fatalf("old credential contaminated renewed account: %+v", account)
	}
}
