package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

func (s *Server) startRainLogin(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "请在正式实例绑定账号")
		return
	}
	s.rainMu.Lock()
	if s.lifetime.Err() != nil {
		s.rainMu.Unlock()
		fail(w, 503, "服务正在停止")
		return
	}
	if s.rainState != "initializing" && s.rainState != "waiting" {
		s.rainPreviousLogin = "unbound"
		for _, account := range s.Engine.Snapshot().Accounts {
			if account.Platform == core.PlatformRain {
				s.rainPreviousLogin = account.LoginState
				break
			}
		}
	}
	if s.rainCancel != nil {
		s.rainCancel()
	}
	if s.rainSyncCancel != nil {
		s.rainSyncCancel()
		s.rainSyncCancel = nil
	}
	s.rainGeneration++
	generation := s.rainGeneration
	loginCtx, loginCancel := context.WithTimeout(s.lifetime, 200*time.Second)
	s.rainCancel = loginCancel
	s.rainState, s.rainError = "initializing", ""
	if err := s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "waiting"}); err != nil {
		loginCancel()
		s.rainState, s.rainError = "failed", "登录状态保存失败"
		s.rainMu.Unlock()
		fail(w, 500, "登录状态保存失败")
		return
	}
	s.rainMu.Unlock()
	createCtx, createCancel := context.WithTimeout(loginCtx, 20*time.Second)
	stopRequest := context.AfterFunc(r.Context(), createCancel)
	flow, err := s.startRain(createCtx)
	stopRequest()
	createCancel()
	s.rainMu.Lock()
	if generation != s.rainGeneration || loginCtx.Err() != nil {
		if generation == s.rainGeneration {
			s.rainState, s.rainError = "canceled", ""
			s.restoreRainAccountLocked()
		}
		s.rainMu.Unlock()
		loginCancel()
		fail(w, 409, "扫码请求已取消，请重新发起")
		return
	}
	if err != nil {
		s.rainState, s.rainError = "failed", "二维码创建失败，请重试"
		_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "failed", LastError: s.rainError})
		s.rainMu.Unlock()
		loginCancel()
		fail(w, 502, "二维码创建失败，请重试")
		return
	}
	s.rainState = "waiting"
	started := s.startJob(func(_ context.Context) {
		defer loginCancel()
		waitCtx, waitCancel := context.WithTimeout(loginCtx, 180*time.Second)
		defer waitCancel()
		cookie, err := flow.wait(waitCtx)
		if err == nil {
			adapter := s.newRain(cookie)
			if err = adapter.Validate(waitCtx); err == nil {
				cookie, err = sessionCredentials(adapter, cookie)
			}
		}
		s.rainMu.Lock()
		if generation != s.rainGeneration {
			s.rainMu.Unlock()
			return
		}
		if s.lifetime.Err() != nil {
			s.rainState, s.rainError = "canceled", ""
			s.restoreRainAccountLocked()
			s.rainMu.Unlock()
			return
		}
		if err == nil {
			err = waitCtx.Err()
		}
		if err != nil {
			s.rainState, s.rainError = "failed", "扫码登录未完成，请重试"
			_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "failed", LastError: s.rainError})
			s.rainMu.Unlock()
			return
		}
		if err = s.Vault.Update(func(c *vault.Config) error { c.RainCookie = cookie; return nil }); err != nil {
			s.rainState, s.rainError = "failed", "会话保存失败"
			_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "failed", LastError: s.rainError})
			s.rainMu.Unlock()
			return
		}
		if err = s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "logged_in"}); err != nil {
			s.rainState, s.rainError = "failed", "登录状态保存失败"
			s.rainMu.Unlock()
			return
		}
		s.rainState, s.rainError = "logged_in", ""
		s.rainPreviousLogin = "logged_in"
		s.rainMu.Unlock()
		// Wait for the previous canceled sync to unwind; never drop this sync.
		s.queueSync()
	})
	if !started {
		s.rainState, s.rainError = "canceled", ""
		s.restoreRainAccountLocked()
		loginCancel()
	}
	s.rainMu.Unlock()
	if !started {
		fail(w, 503, "服务正在停止")
		return
	}
	jsonResponse(w, 200, map[string]any{"image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(flow.image), "expires_in": 180})
}

func (s *Server) restoreRainAccountLocked() {
	state := s.rainPreviousLogin
	if state == "" || state == "waiting" {
		state = "unbound"
	}
	_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: state})
}

func (s *Server) rainLoginStatus(w http.ResponseWriter, r *http.Request) {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	jsonResponse(w, 200, map[string]string{"state": s.rainState, "error": s.rainError})
}

func (s *Server) cancelRainLogin(w http.ResponseWriter, r *http.Request) {
	s.rainMu.Lock()
	canceled := s.rainState == "initializing" || s.rainState == "waiting"
	if canceled {
		s.rainGeneration++
		if s.rainCancel != nil {
			s.rainCancel()
			s.rainCancel = nil
		}
		if s.rainSyncCancel != nil {
			s.rainSyncCancel()
			s.rainSyncCancel = nil
		}
		s.rainState, s.rainError = "canceled", ""
		s.restoreRainAccountLocked()
	}
	s.rainMu.Unlock()
	if canceled {
		s.queueSync()
	}
	jsonResponse(w, 200, map[string]bool{"ok": true})
}
