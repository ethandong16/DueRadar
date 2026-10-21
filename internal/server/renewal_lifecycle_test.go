package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

func renewalAccount(t *testing.T, s *Server, platform string) core.Account {
	t.Helper()
	for _, account := range s.Engine.Snapshot().Accounts {
		if account.Platform == platform {
			return account
		}
	}
	t.Fatal("account missing")
	return core.Account{}
}

func seedRenewal(t *testing.T, s *Server) (string, core.State) {
	t.Helper()
	directory := t.TempDir()
	var err error
	s.Vault, err = vault.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "original"; c.BarkKey = "before"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Sync(core.PlatformRain, []core.Task{{ID: "previous", Title: "Previous task", Kind: "homework", Status: "pending"}}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	return directory, s.Engine.Snapshot()
}

func TestSessionRainRenewalPersistsAndPreservesBark(t *testing.T) {
	s, _ := testApp(t)
	directory, _ := seedRenewal(t, s)
	tasksRead := false
	s.newRain = func(input string) rainAdapter {
		if input != "original" {
			t.Fatal("wrong input session")
		}
		return renewingZhidao{fakeRainAdapter: fakeRainAdapter{tasks: func(context.Context) ([]core.Task, error) {
			tasksRead = true
			if err := s.Vault.Update(func(c *vault.Config) error { c.BarkKey = "concurrent-update"; return nil }); err != nil {
				return nil, err
			}
			return []core.Task{{ID: "current", Title: "Current task", Kind: "homework", Status: "pending"}}, nil
		}}, export: func() (string, error) {
			if !tasksRead {
				t.Error("session exported before complete task read")
			}
			return "renewed", nil
		}}
	}
	if err := s.syncRain(context.Background()); err != nil {
		t.Fatal(err)
	}
	config := s.Vault.Get()
	if config.RainCookie != "renewed" || config.BarkKey != "concurrent-update" {
		t.Fatal("renewal lost or concurrent Bark update overwritten")
	}
	reopened, err := vault.Open(directory)
	if err != nil || reopened.Get() != config {
		t.Fatalf("renewed session did not survive reopen: %v", err)
	}
	if renewalAccount(t, s, core.PlatformRain).SyncState != "success" {
		t.Fatal("complete persisted sync did not become successful")
	}
	s.Vault = reopened
	s.newRain = func(input string) rainAdapter {
		if input != "renewed" {
			t.Error("next sync did not reuse persisted renewal")
		}
		return fakeRainAdapter{}
	}
	if err := s.syncRain(context.Background()); err != nil || s.Vault.Get().RainCookie != "renewed" {
		t.Fatalf("adapter without optional exporter stopped working: %v", err)
	}
}

func TestSessionRainRenewalFailurePreservesSnapshot(t *testing.T) {
	for _, failure := range []string{"export_error", "empty", "whitespace", "vault_commit", "task_read"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := testApp(t)
			directory, before := seedRenewal(t, s)
			config := s.Vault.Get()
			var restore func()
			defer func() {
				if restore != nil {
					restore()
				}
			}()
			s.newRain = func(string) rainAdapter {
				return renewingZhidao{fakeRainAdapter: fakeRainAdapter{tasks: func(context.Context) ([]core.Task, error) {
					if failure == "task_read" {
						return nil, errors.New("incomplete task list")
					}
					return []core.Task{{ID: "new", Title: "Not committed", Kind: "homework", Status: "pending"}}, nil
				}}, export: func() (string, error) {
					switch failure {
					case "export_error":
						return "", errors.New("SECRET_SESSION must not escape")
					case "empty":
						return "", nil
					case "whitespace":
						return " \t ", nil
					case "vault_commit":
						restore = blockStateDirectory(t, directory)
					case "task_read":
						t.Error("export ran after failed task read")
					}
					return "renewed", nil
				}}
			}
			err := s.syncRain(context.Background())
			if err == nil || strings.Contains(err.Error(), "SECRET_SESSION") {
				t.Fatalf("renewal failure hidden or raw credential diagnostic exposed: %v", err)
			}
			if s.Vault.Get() != config || !reflect.DeepEqual(s.Engine.Snapshot().Tasks, before.Tasks) {
				t.Fatal("failed renewal modified credentials or task snapshot")
			}
			account := renewalAccount(t, s, core.PlatformRain)
			if account.SyncState != "error" || account.ConsecutiveFailures != 1 || !account.LastSuccessAt.Equal(*before.Accounts[0].LastSuccessAt) {
				t.Fatal("failed renewal marked sync successful or changed successful timestamp")
			}
			if restore != nil {
				restore()
				restore = nil
			}
			reopened, openErr := vault.Open(directory)
			if openErr != nil || reopened.Get() != config {
				t.Fatalf("original durable credentials changed: %v", openErr)
			}
		})
	}
}

func TestSessionRainRenewalRejectsStaleOrCanceledSync(t *testing.T) {
	for _, reason := range []string{"generation", "credential_replaced", "unchanged_export", "canceled"} {
		t.Run(reason, func(t *testing.T) {
			s, _ := testApp(t)
			_, before := seedRenewal(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.newRain = func(string) rainAdapter {
				return renewingZhidao{fakeRainAdapter: fakeRainAdapter{}, export: func() (string, error) {
					if reason == "canceled" {
						cancel()
						return "renewed", nil
					}
					s.rainMu.Lock()
					defer s.rainMu.Unlock()
					if reason == "generation" {
						s.rainGeneration++
					}
					if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "replacement"; return nil }); err != nil {
						return "", err
					}
					if reason == "unchanged_export" {
						return "original", nil
					}
					return "stale-renewal", nil
				}}
			}
			wantErr, wantCookie := errSupersededLogin, "replacement"
			if reason == "canceled" {
				wantErr, wantCookie = context.Canceled, "original"
			}
			if err := s.syncRain(ctx); !errors.Is(err, wantErr) {
				t.Fatalf("stale/canceled sync result: %v", err)
			}
			if s.Vault.Get().RainCookie != wantCookie || !reflect.DeepEqual(s.Engine.Snapshot().Tasks, before.Tasks) {
				t.Fatal("stale/canceled sync changed current credentials or snapshot")
			}
			if renewalAccount(t, s, core.PlatformRain).ConsecutiveFailures != 0 {
				t.Fatal("superseded work contaminated current failure count")
			}
		})
	}
}

func TestSessionQRValidationRenewalSavedForBothProviders(t *testing.T) {
	for _, provider := range []string{"rain", "zhidao"} {
		for _, outcome := range []string{"renewed", "export_error", "empty", "vault_commit"} {
			t.Run(provider+"/"+outcome, func(t *testing.T) {
				s, h := testApp(t)
				admin := loginCookie(t, h)
				directory := t.TempDir()
				var err error
				s.Vault, err = vault.Open(directory)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "previous"; c.ZhidaoSession = "previous"; return nil }); err != nil {
					t.Fatal(err)
				}
				// Hold automatic collection until cleanup so this asserts the login
				// transaction itself, independent of a later adapter correcting it.
				s.syncGate <- struct{}{}
				var validated atomic.Bool
				restores := make(chan func(), 1)
				defer func() {
					select {
					case restore := <-restores:
						restore()
					default:
					}
				}()
				factory := func(input string) rainAdapter {
					current := input
					return renewingZhidao{fakeRainAdapter: fakeRainAdapter{validate: func(context.Context) error {
						current = "after-validation"
						validated.Store(true)
						return s.Vault.Update(func(c *vault.Config) error { c.BarkKey = "concurrent-login-setting"; return nil })
					}}, export: func() (string, error) {
						switch outcome {
						case "export_error":
							return "", errors.New("SECRET_SESSION validation export")
						case "empty":
							return "", nil
						case "vault_commit":
							restores <- blockStateDirectory(t, directory)
						}
						return current, nil
					}}
				}
				flow := func(context.Context) (*rainFlow, error) {
					return &rainFlow{wait: func(context.Context) (string, error) { return "before-validation", nil }}, nil
				}
				state := func() string { s.rainMu.Lock(); defer s.rainMu.Unlock(); return s.rainState }
				s.newRain, s.startRain = factory, flow
				if provider == "zhidao" {
					s.newZhidao, s.startZhidao = factory, flow
					state = func() string { s.zhidaoMu.Lock(); defer s.zhidaoMu.Unlock(); return s.zhidaoState }
				}
				if w := request(h, "POST", "/api/accounts/"+provider+"/login", "{}", admin, ""); w.Code != 200 {
					t.Fatal(w.Code)
				}
				eventually(t, func() bool { current := state(); return current == "logged_in" || current == "failed" })
				want, wantState := "after-validation", "logged_in"
				if outcome != "renewed" {
					want, wantState = "previous", "failed"
				}
				config := s.Vault.Get()
				actual := config.RainCookie
				if provider == "zhidao" {
					actual = config.ZhidaoSession
				}
				if !validated.Load() || state() != wantState || actual != want || config.BarkKey != "concurrent-login-setting" {
					t.Fatal("login did not preserve the independently validated session/failure and concurrent configuration")
				}
				select {
				case restore := <-restores:
					restore()
				default:
				}
				reopened, err := vault.Open(directory)
				if err != nil || reopened.Get() != config {
					t.Fatalf("login credentials were not durably preserved: %v", err)
				}
			})
		}
	}
}

func TestSessionRainKeepsRenewedCredentialsIfTaskStateWriteFails(t *testing.T) {
	s, stateDir := deliveryApp(t)
	if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "original"; return nil }); err != nil {
		t.Fatal(err)
	}
	before := s.Engine.Snapshot()
	var restore func()
	defer func() {
		if restore != nil {
			restore()
		}
	}()
	s.newRain = func(string) rainAdapter {
		return renewingZhidao{fakeRainAdapter: fakeRainAdapter{}, export: func() (string, error) {
			restore = blockStateDirectory(t, stateDir)
			return "renewed", nil
		}}
	}
	if err := s.syncRain(context.Background()); err == nil {
		t.Fatal("task state write failure hidden")
	}
	if s.Vault.Get().RainCookie != "renewed" {
		t.Fatal("valid renewed credentials were lost after state write failure")
	}
	if !reflect.DeepEqual(s.Engine.Snapshot().Tasks, before.Tasks) || !renewalAccount(t, s, core.PlatformRain).LastSuccessAt.Equal(*before.Accounts[0].LastSuccessAt) {
		t.Fatal("failed task state write changed the successful snapshot")
	}
	restore()
	restore = nil
	reopened, err := vault.Open(filepath.Join(filepath.Dir(stateDir), "credentials"))
	if err != nil || reopened.Get().RainCookie != "renewed" {
		t.Fatalf("renewal did not survive reopen: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "state.json")); err != nil {
		t.Fatal(err)
	}
}

func TestSessionZhidaoUnchangedExportRejectsReplacedCredentials(t *testing.T) {
	s, _ := testApp(t)
	if err := s.Vault.Update(func(c *vault.Config) error { c.ZhidaoSession = "original"; return nil }); err != nil {
		t.Fatal(err)
	}
	s.newZhidao = func(string) rainAdapter {
		return renewingZhidao{fakeRainAdapter: fakeRainAdapter{tasks: func(context.Context) ([]core.Task, error) {
			return []core.Task{{ID: "stale", Title: "Previous account", Kind: "homework", Status: "pending"}}, nil
		}}, export: func() (string, error) {
			err := s.Vault.Update(func(c *vault.Config) error { c.ZhidaoSession = "replacement"; return nil })
			return "original", err
		}}
	}
	if err := s.syncZhidao(context.Background()); !errors.Is(err, errSupersededLogin) {
		t.Fatalf("unchanged export bypassed stored-credential comparison: %v", err)
	}
	if s.Vault.Get().ZhidaoSession != "replacement" || len(s.Engine.Snapshot().Tasks) != 0 {
		t.Fatal("stale sync changed the replacement session or task snapshot")
	}
	if account := renewalAccount(t, s, core.PlatformZhidaoCampus); account.LastSuccessAt != nil || account.ConsecutiveFailures != 0 {
		t.Fatal("superseded sync affected current account status")
	}
}
