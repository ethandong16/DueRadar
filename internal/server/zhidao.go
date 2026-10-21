package server

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/ethandong16/due-radar/internal/platform"
	"net/http"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

func (s *Server) startZhidaoLogin(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "请在正式实例绑定账号")
		return
	}
	s.zhidaoMu.Lock()
	if s.lifetime.Err() != nil {
		s.zhidaoMu.Unlock()
		fail(w, 503, "服务正在停止")
		return
	}
	if s.zhidaoState != "initializing" && s.zhidaoState != "waiting" {
		s.zhidaoPreviousLogin = "unbound"
		for _, account := range s.Engine.Snapshot().Accounts {
			if account.Platform == core.PlatformZhidaoCampus {
				s.zhidaoPreviousLogin = account.LoginState
				break
			}
		}
	}
	if s.zhidaoCancel != nil {
		s.zhidaoCancel()
	}
	if s.zhidaoSyncCancel != nil {
		s.zhidaoSyncCancel()
		s.zhidaoSyncCancel = nil
	}
	s.zhidaoGeneration++
	generation := s.zhidaoGeneration
	loginCtx, loginCancel := context.WithTimeout(s.lifetime, 200*time.Second)
	s.zhidaoCancel = loginCancel
	s.zhidaoState, s.zhidaoError = "initializing", ""
	if err := s.Engine.SetAccount(core.Account{Platform: core.PlatformZhidaoCampus, LoginState: "waiting"}); err != nil {
		loginCancel()
		s.zhidaoState, s.zhidaoError = "failed", "登录状态保存失败"
		s.zhidaoMu.Unlock()
		fail(w, 500, "登录状态保存失败")
		return
	}
	s.zhidaoMu.Unlock()
	createCtx, createCancel := context.WithTimeout(loginCtx, 20*time.Second)
	stopRequest := context.AfterFunc(r.Context(), createCancel)
	flow, err := s.startZhidao(createCtx)
	stopRequest()
	createCancel()
	s.zhidaoMu.Lock()
	if generation != s.zhidaoGeneration || loginCtx.Err() != nil {
		if generation == s.zhidaoGeneration {
			s.zhidaoState, s.zhidaoError = "canceled", ""
			s.restoreZhidaoAccountLocked()
		}
		s.zhidaoMu.Unlock()
		loginCancel()
		fail(w, 409, "扫码请求已取消，请重新发起")
		return
	}
	if err != nil {
		s.zhidaoState, s.zhidaoError = "failed", "二维码创建失败，请重试"
		_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformZhidaoCampus, LoginState: "failed", LastError: s.zhidaoError})
		s.zhidaoMu.Unlock()
		loginCancel()
		fail(w, 502, "二维码创建失败，请重试")
		return
	}
	s.zhidaoState = "waiting"
	started := s.startJob(func(_ context.Context) {
		defer loginCancel()
		waitCtx, waitCancel := context.WithTimeout(loginCtx, 180*time.Second)
		defer waitCancel()
		cookie, err := flow.wait(waitCtx)
		if err == nil {
			err = s.newZhidao(cookie).Validate(waitCtx)
		}
		s.zhidaoMu.Lock()
		if generation != s.zhidaoGeneration {
			s.zhidaoMu.Unlock()
			return
		}
		if s.lifetime.Err() != nil {
			s.zhidaoState, s.zhidaoError = "canceled", ""
			s.restoreZhidaoAccountLocked()
			s.zhidaoMu.Unlock()
			return
		}
		if err == nil {
			err = waitCtx.Err()
		}
		if err != nil {
			s.zhidaoState, s.zhidaoError = "failed", "扫码登录未完成，请重试"
			_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformZhidaoCampus, LoginState: "failed", LastError: s.zhidaoError})
			s.zhidaoMu.Unlock()
			return
		}
		if err = s.Vault.Update(func(c *vault.Config) error { c.ZhidaoSession = cookie; return nil }); err != nil {
			s.zhidaoState, s.zhidaoError = "failed", "会话保存失败"
			_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformZhidaoCampus, LoginState: "failed", LastError: s.zhidaoError})
			s.zhidaoMu.Unlock()
			return
		}
		if err = s.Engine.SetAccount(core.Account{Platform: core.PlatformZhidaoCampus, LoginState: "logged_in"}); err != nil {
			s.zhidaoState, s.zhidaoError = "failed", "登录状态保存失败"
			s.zhidaoMu.Unlock()
			return
		}
		s.zhidaoState, s.zhidaoError = "logged_in", ""
		s.zhidaoPreviousLogin = "logged_in"
		s.zhidaoMu.Unlock()
		// Wait for the previous canceled sync to unwind; never drop this sync.
		s.queueSync()
	})
	if !started {
		s.zhidaoState, s.zhidaoError = "canceled", ""
		s.restoreZhidaoAccountLocked()
		loginCancel()
	}
	s.zhidaoMu.Unlock()
	if !started {
		fail(w, 503, "服务正在停止")
		return
	}
	jsonResponse(w, 200, map[string]any{"image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(flow.image), "expires_in": 180})
}

func (s *Server) restoreZhidaoAccountLocked() {
	state := s.zhidaoPreviousLogin
	if state == "" || state == "waiting" {
		state = "unbound"
	}
	_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformZhidaoCampus, LoginState: state})
}

func (s *Server) zhidaoLoginStatus(w http.ResponseWriter, r *http.Request) {
	s.zhidaoMu.Lock()
	defer s.zhidaoMu.Unlock()
	jsonResponse(w, 200, map[string]string{"state": s.zhidaoState, "error": s.zhidaoError})
}

func (s *Server) cancelZhidaoLogin(w http.ResponseWriter, r *http.Request) {
	s.zhidaoMu.Lock()
	canceled := s.zhidaoState == "initializing" || s.zhidaoState == "waiting"
	if canceled {
		s.zhidaoGeneration++
		if s.zhidaoCancel != nil {
			s.zhidaoCancel()
			s.zhidaoCancel = nil
		}
		if s.zhidaoSyncCancel != nil {
			s.zhidaoSyncCancel()
			s.zhidaoSyncCancel = nil
		}
		s.zhidaoState, s.zhidaoError = "canceled", ""
		s.restoreZhidaoAccountLocked()
	}
	s.zhidaoMu.Unlock()
	if canceled {
		s.queueSync()
	}
	jsonResponse(w, 200, map[string]bool{"ok": true})
}

func (s *Server) syncZhidao(ctx context.Context) error {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	s.zhidaoMu.Lock()
	if s.zhidaoState == "initializing" || s.zhidaoState == "waiting" {
		s.zhidaoMu.Unlock()
		return nil
	}
	generation := s.zhidaoGeneration
	s.zhidaoSyncCancel = cancel
	cookie := s.Vault.Get().ZhidaoSession
	s.zhidaoMu.Unlock()
	defer func() {
		s.zhidaoMu.Lock()
		if generation == s.zhidaoGeneration {
			s.zhidaoSyncCancel = nil
		}
		s.zhidaoMu.Unlock()
	}()
	if cookie == "" {
		return nil
	}
	adapter := s.newZhidao(cookie)
	if err := adapter.Validate(ctx); err != nil {
		_ = s.failZhidaoSync(ctx, generation, errors.Is(err, platform.ErrSessionExpired), "登录验证失败")
		return err
	}
	if err := s.commitZhidaoSync(ctx, generation, func() error {
		return s.Engine.SetAccount(core.Account{Platform: core.PlatformZhidaoCampus, LoginState: "logged_in"})
	}); err != nil {
		return err
	}
	tasks, err := adapter.Tasks(ctx)
	if err != nil {
		_ = s.failZhidaoSync(ctx, generation, errors.Is(err, platform.ErrSessionExpired), "任务采集失败")
		return err
	}
	// Persist renewed cookies only after a complete successful read. The same
	// generation guard prevents a previous account from replacing a new login.
	if exporter, ok := adapter.(interface{ Credentials() (string, error) }); ok {
		updated, err := exporter.Credentials()
		if err != nil || updated == "" {
			_ = s.failZhidaoSync(ctx, generation, false, "会话续期读取失败")
			return errors.New("知到会话续期读取失败")
		}
		if updated != cookie {
			if err := s.commitZhidaoSync(ctx, generation, func() error {
				return s.Vault.Update(func(c *vault.Config) error {
					if c.ZhidaoSession != cookie {
						return errSupersededLogin
					}
					c.ZhidaoSession = updated
					return nil
				})
			}); err != nil {
				_ = s.failZhidaoSync(ctx, generation, false, "会话续期保存失败")
				return err
			}
		}
	}
	return s.commitZhidaoSync(ctx, generation, func() error { return s.Engine.Sync(core.PlatformZhidaoCampus, tasks, time.Now()) })
}
