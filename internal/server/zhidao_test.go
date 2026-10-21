package server

import (
	"context"
	"errors"
	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
	"testing"
	"time"
)

func TestRainFailureDoesNotBlockZhidaoSync(t *testing.T) {
	s, _ := testApp(t)
	s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "rain"; c.ZhidaoSession = "zhidao"; return nil })
	s.newRain = func(string) rainAdapter {
		return fakeRainAdapter{validate: func(context.Context) error { return errors.New("offline") }}
	}
	s.newZhidao = func(string) rainAdapter {
		return fakeRainAdapter{tasks: func(context.Context) ([]core.Task, error) {
			return []core.Task{{ID: "zhs-test", Title: "test", Course: "course", Kind: "homework", Status: "pending"}}, nil
		}}
	}
	if s.Sync(context.Background()) == nil {
		t.Fatal("rain failure hidden")
	}
	for _, a := range s.Engine.Snapshot().Accounts {
		if a.Platform == core.PlatformZhidaoCampus && a.SyncState != "success" {
			t.Fatalf("Zhidao was blocked: %s", a.SyncState)
		}
	}
}
func TestCancelZhidaoDuringQRCreation(t *testing.T) {
	s, h := testApp(t)
	admin := loginCookie(t, h)
	entered, done := make(chan struct{}), make(chan struct{})
	s.startZhidao = func(ctx context.Context) (*rainFlow, error) { close(entered); <-ctx.Done(); return nil, ctx.Err() }
	go func() {
		defer close(done)
		w := request(h, "POST", "/api/accounts/zhidao/login", "{}", admin, "")
		if w.Code != 409 {
			t.Errorf("status %d", w.Code)
		}
	}()
	await(t, entered)
	w := request(h, "POST", "/api/accounts/zhidao/login/cancel", "{}", admin, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	await(t, done)
	s.zhidaoMu.Lock()
	defer s.zhidaoMu.Unlock()
	if s.zhidaoState != "canceled" {
		t.Fatal("canceled QR was revived")
	}
}

func TestRainDeadlineDoesNotStarveZhidaoSync(t *testing.T) {
	s, _ := testApp(t)
	if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "rain"; c.ZhidaoSession = "zhidao"; return nil }); err != nil {
		t.Fatal(err)
	}
	rainStarted := make(chan struct{})
	zhidaoRead := make(chan struct{})
	s.newRain = func(string) rainAdapter {
		return fakeRainAdapter{validate: func(ctx context.Context) error { close(rainStarted); <-ctx.Done(); return ctx.Err() }}
	}
	s.newZhidao = func(string) rainAdapter {
		return fakeRainAdapter{tasks: func(ctx context.Context) ([]core.Task, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			close(zhidaoRead)
			return []core.Task{{ID: "zhs-during-rain-timeout", Title: "test", Kind: "homework", Status: "pending"}}, nil
		}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- s.Sync(ctx) }()
	await(t, rainStarted)
	select {
	case <-zhidaoRead:
	case <-ctx.Done():
		t.Fatal("Zhidao did not run before Rain exhausted the shared deadline")
	}
	if err := <-finished; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Rain deadline was hidden: %v", err)
	}
	state := s.Engine.Snapshot()
	if len(state.Tasks) != 1 || state.Tasks[0].ID != "zhs-during-rain-timeout" {
		t.Fatal("successful peer snapshot was not saved")
	}
	for _, account := range state.Accounts {
		if account.Platform == core.PlatformRain && account.ConsecutiveFailures != 1 {
			t.Fatal("Rain timeout was not counted")
		}
		if account.Platform == core.PlatformZhidaoCampus && (account.SyncState != "success" || account.ConsecutiveFailures != 0) {
			t.Fatal("Rain timeout contaminated Zhidao state")
		}
	}
}

func TestRoundCancellationReachesBothPlatforms(t *testing.T) {
	s, _ := testApp(t)
	if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "rain"; c.ZhidaoSession = "zhidao"; return nil }); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 2)
	finishedPlatform := make(chan struct{}, 2)
	adapter := func(string) rainAdapter {
		return fakeRainAdapter{validate: func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			finishedPlatform <- struct{}{}
			return ctx.Err()
		}}
	}
	s.newRain, s.newZhidao = adapter, adapter
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- s.Sync(ctx) }()
	await(t, started)
	await(t, started)
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("round did not stop after caller cancellation")
	}
	if len(finishedPlatform) != 2 {
		t.Fatal("round returned before both platform operations stopped")
	}
	for _, account := range s.Engine.Snapshot().Accounts {
		if account.ConsecutiveFailures != 0 {
			t.Fatal("caller cancellation was counted as a platform failure")
		}
	}
}
