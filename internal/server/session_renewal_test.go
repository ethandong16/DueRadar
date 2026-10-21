package server

import (
	"context"
	"errors"
	"testing"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

type renewingZhidao struct {
	fakeRainAdapter
	export func() (string, error)
}

func (a renewingZhidao) Credentials() (string, error) { return a.export() }

func TestZhidaoRenewalPreservesConcurrentBarkSettings(t *testing.T) {
	s, _ := testApp(t)
	directory := t.TempDir()
	credentials, err := vault.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	s.Vault = credentials
	if err := s.Vault.Update(func(c *vault.Config) error { c.ZhidaoSession = "original"; return nil }); err != nil {
		t.Fatal(err)
	}
	s.newZhidao = func(string) rainAdapter {
		return renewingZhidao{fakeRainAdapter: fakeRainAdapter{tasks: func(context.Context) ([]core.Task, error) {
			if err := s.Vault.Update(func(c *vault.Config) error { c.BarkKey = "concurrently-saved"; return nil }); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}}, export: func() (string, error) { return "renewed", nil }}
	}
	if err := s.syncZhidao(context.Background()); err != nil {
		t.Fatal(err)
	}
	config := s.Vault.Get()
	if config.ZhidaoSession != "renewed" || config.BarkKey != "concurrently-saved" {
		t.Fatal("renewal overwrote unrelated settings or did not save")
	}
	reopened, err := vault.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Get() != config {
		t.Fatal("renewed credentials did not survive reopening")
	}
	s.Vault = reopened
	// A second adapter receives the persisted new session instead of the old one.
	s.newZhidao = func(serialized string) rainAdapter {
		if serialized != "renewed" {
			t.Fatal("next sync did not reuse renewed credentials")
		}
		return fakeRainAdapter{}
	}
	if err := s.syncZhidao(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSupersededZhidaoRenewalCannotOverwriteNewLogin(t *testing.T) {
	s, _ := testApp(t)
	if err := s.Vault.Update(func(c *vault.Config) error { c.ZhidaoSession = "original"; return nil }); err != nil {
		t.Fatal(err)
	}
	s.newZhidao = func(string) rainAdapter {
		return renewingZhidao{fakeRainAdapter: fakeRainAdapter{}, export: func() (string, error) {
			s.zhidaoMu.Lock()
			s.zhidaoGeneration++
			err := s.Vault.Update(func(c *vault.Config) error { c.ZhidaoSession = "replacement"; return nil })
			s.zhidaoMu.Unlock()
			return "stale-renewal", err
		}}
	}
	if err := s.syncZhidao(context.Background()); !errors.Is(err, errSupersededLogin) {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Vault.Get().ZhidaoSession != "replacement" {
		t.Fatal("stale renewal overwrote replacement login")
	}
}
