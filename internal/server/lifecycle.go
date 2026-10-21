package server

import (
	"context"
	"errors"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
)

type rainAdapter interface {
	Validate(context.Context) error
	Tasks(context.Context) ([]core.Task, error)
}

type rainFlow struct {
	image []byte
	wait  func(context.Context) (string, error)
}

func defaultRainFlow(ctx context.Context) (*rainFlow, error) {
	flow, err := platform.StartRainLogin(ctx)
	if err != nil {
		return nil, err
	}
	return &rainFlow{image: flow.Image, wait: flow.Wait}, nil
}

func (s *Server) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.lifetime, cancel)
	if s.lifetime.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func (s *Server) startJob(run func(context.Context)) bool {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.stopping || s.lifetime.Err() != nil {
		return false
	}
	s.jobs.Add(1)
	go func() { defer s.jobs.Done(); run(s.lifetime) }()
	return true
}

func (s *Server) queueSync() bool {
	return s.startJob(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		_ = s.Sync(ctx)
	})
}

func (s *Server) stopBackground() {
	s.lifeMu.Lock()
	s.stopping = true
	s.lifetimeCancel()
	s.lifeMu.Unlock()
}

// Shutdown stops accepting jobs, cancels all service-owned network operations,
// and waits for their final persistence operations, bounded by the caller.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopBackground()
	done := make(chan struct{})
	go func() { s.jobs.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errSupersededLogin = errors.New("登录会话已更新")

func (s *Server) commitRainSync(ctx context.Context, generation uint64, commit func() error) error {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if generation != s.rainGeneration {
		return errSupersededLogin
	}
	return commit()
}

func (s *Server) failRainSync(ctx context.Context, generation uint64, expired bool, message string) error {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	if s.lifetime.Err() != nil || generation != s.rainGeneration {
		return errSupersededLogin
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// A normal sync deadline is one failed polling round. Rebind and shutdown
	// cancellation must not mutate the current account's failure count.
	return s.Engine.Fail(core.PlatformRain, expired, message, time.Now())
}

type deliveryOutcome struct {
	id   string
	sent bool
	at   time.Time
}

// A failed disk commit is retried before sending anything else. Within this
// process, a successful remote push is not repeated after a local record failure.
func (s *Server) persistDelivery() error {
	outcome := s.deliveryResult
	if outcome == nil {
		return nil
	}
	var err error
	if outcome.sent {
		err = s.Engine.MarkSent(outcome.id, outcome.at)
	} else {
		err = s.Engine.MarkFailed(outcome.id, "通知发送失败", outcome.at)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.deliveryAt = &outcome.at
	s.deliveryError = ""
	s.mu.Unlock()
	s.deliveryResult = nil
	return nil
}

func (s *Server) setDeliveryError(message string) {
	s.mu.Lock()
	s.deliveryError = message
	s.mu.Unlock()
}

func (s *Server) commitZhidaoSync(ctx context.Context, generation uint64, commit func() error) error {
	s.zhidaoMu.Lock()
	defer s.zhidaoMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if generation != s.zhidaoGeneration {
		return errSupersededLogin
	}
	return commit()
}

func (s *Server) failZhidaoSync(ctx context.Context, generation uint64, expired bool, message string) error {
	s.zhidaoMu.Lock()
	defer s.zhidaoMu.Unlock()
	if s.lifetime.Err() != nil || generation != s.zhidaoGeneration {
		return errSupersededLogin
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// A normal sync deadline is one failed polling round. Rebind and shutdown
	// cancellation must not mutate the current account's failure count.
	return s.Engine.Fail(core.PlatformZhidaoCampus, expired, message, time.Now())
}

func defaultZhidaoFlow(ctx context.Context) (*rainFlow, error) {
	flow, err := platform.StartZhidaoLogin(ctx)
	if err != nil {
		return nil, err
	}
	return &rainFlow{image: flow.Image, wait: flow.Wait}, nil
}
