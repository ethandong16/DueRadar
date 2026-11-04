package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

const (
	// zhidaoSlideWindow stays under Cloudflare's 100s origin timeout so the
	// browser always receives a JSON result instead of a proxy error page.
	zhidaoSlideWindow = 80 * time.Second
	// zhidaoSlideWriteGrace covers the write after the login finally settles.
	zhidaoSlideWriteGrace = 15 * time.Second
)

// sliderCSP is scoped to the one page that mounts the 网易易盾 widget, so the
// dashboard keeps its strict same-origin policy. The widget loads its config
// over JSONP from the 163/163yun dun hosts, its runtime from the 126.net CDN
// (both cstaticdun and cstaticdun1), and probes the client from
// *.nstool.netease.com, so those hosts have to be reachable here.
const sliderCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' 'unsafe-eval' 'wasm-unsafe-eval' https://*.126.net http://*.126.net https://*.163.com http://*.163.com https://*.163yun.com http://*.163yun.com https://*.netease.com http://*.netease.com; " +
	"worker-src 'self' blob: https://*.126.net; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: https://*.126.net http://*.126.net https://*.127.net http://*.127.net https://*.163.com https://*.163yun.com https://*.netease.com; " +
	"connect-src 'self' https://*.126.net http://*.126.net https://*.127.net http://*.127.net https://*.163.com http://*.163.com https://*.163yun.com http://*.163yun.com https://*.netease.com http://*.netease.com https://*.zhihuishu.com; " +
	"font-src 'self' data: https://*.126.net; " +
	"frame-src https://*.126.net https://*.163.com https://*.163yun.com; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// applySliderSecurity relaxes the dashboard CSP for the captcha page only.
func applySliderSecurity(w http.ResponseWriter, path string) {
	if path == "/slider.html" {
		w.Header().Set("Content-Security-Policy", sliderCSP)
	}
}

// slideZhidaoLogin finishes a password login with a captcha token the student
// solved in a browser. The phone and password come from the vault, so the page
// never displays or transmits them.
func (s *Server) slideZhidaoLogin(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "演示模式不登录平台账号")
		return
	}
	// The platform round trip regularly outlives the server's 30s write
	// deadline; without this the login can succeed while the browser still
	// shows a transport error.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(zhidaoSlideWindow + zhidaoSlideWriteGrace)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("zhidao slide login: cannot extend write deadline: %v", err)
	}
	started := time.Now()
	var input struct {
		Validate string `json:"validate"`
	}
	if !decode(w, r, &input) {
		return
	}
	validate := strings.TrimSpace(input.Validate)
	if validate == "" || len(validate) > 4096 {
		fail(w, 400, "滑块验证结果无效，请重新滑动")
		return
	}
	c := s.Vault.Get()
	if c.ZhidaoPhone == "" || c.ZhidaoPassword == "" {
		fail(w, 400, "请先保存知到手机号和密码")
		return
	}
	s.zhidaoAutoMu.Lock()
	busy := s.zhidaoAutoState == "running"
	if !busy {
		s.zhidaoAutoState, s.zhidaoAutoStage, s.zhidaoAutoError = "running", "logging_in", ""
	}
	s.zhidaoAutoMu.Unlock()
	if busy {
		fail(w, 409, "已有登录正在进行，请稍候")
		return
	}
	defer func() {
		s.zhidaoAutoMu.Lock()
		if s.zhidaoAutoState == "running" {
			s.zhidaoAutoState = ""
		}
		s.zhidaoAutoMu.Unlock()
	}()
	s.zhidaoMu.Lock()
	busy = s.zhidaoState == "initializing" || s.zhidaoState == "waiting"
	generation := s.zhidaoGeneration
	s.zhidaoMu.Unlock()
	if busy {
		fail(w, 409, "正在扫码绑定，请先完成或取消")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), zhidaoSlideWindow)
	defer cancel()
	var collector platform.SMSCollector
	if s.smsSource != nil {
		collector = &stagedCollector{inner: newSMSCollector(s.smsSource), onWait: func() { s.setAutoStage("waiting_sms") }}
	}
	updated, err := s.slideLogin(ctx, c.ZhidaoPhone, c.ZhidaoPassword, validate, collector)
	if err != nil {
		log.Printf("zhidao slide login failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		s.zhidaoAutoMu.Lock()
		s.zhidaoAutoState, s.zhidaoAutoError = "failed", zhidaoSlideErrorMessage(err)
		now := time.Now()
		s.zhidaoAutoAt = &now
		s.zhidaoAutoMu.Unlock()
		status := 502
		if errors.Is(err, platform.ErrPasswordRejected) || errors.Is(err, platform.ErrCaptchaRejected) {
			status = 401
		}
		if status == 502 {
			// Cloudflare replaces origin 5xx bodies with its own error page, so a
			// transport-level failure is reported inside a 200 envelope instead.
			jsonResponse(w, 200, map[string]any{"ok": false, "error": zhidaoSlideErrorMessage(err)})
			return
		}
		fail(w, status, zhidaoSlideErrorMessage(err))
		return
	}
	if strings.TrimSpace(updated) == "" {
		jsonResponse(w, 200, map[string]any{"ok": false, "error": "知到登录未返回有效会话"})
		return
	}
	if oldNID := zhidaoIdentity(c.ZhidaoSession); oldNID != "" {
		if newNID := zhidaoIdentity(updated); newNID != "" && newNID != oldNID {
			fail(w, 409, "知到登录的账号与当前绑定不一致")
			return
		}
	}
	s.zhidaoMu.Lock()
	if generation != s.zhidaoGeneration || s.zhidaoState == "initializing" || s.zhidaoState == "waiting" {
		s.zhidaoMu.Unlock()
		fail(w, 409, "已有新的登录，本次滑块登录已取消")
		return
	}
	err = s.Vault.Update(func(cfg *vault.Config) error {
		cfg.ZhidaoSession = updated
		return nil
	})
	if err != nil {
		s.zhidaoMu.Unlock()
		fail(w, 500, "知到会话保存失败，请检查数据目录")
		return
	}
	s.zhidaoGeneration++
	_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformZhidaoCampus, LoginState: "logged_in"})
	s.zhidaoMu.Unlock()
	s.zhidaoAutoMu.Lock()
	now := time.Now()
	s.zhidaoAutoState, s.zhidaoAutoStage, s.zhidaoAutoError, s.zhidaoAutoAt = "success", "", "", &now
	s.zhidaoAutoMu.Unlock()
	s.queueSync()
	log.Printf("zhidao slide login succeeded in %s", time.Since(started).Round(time.Millisecond))
	jsonResponse(w, 200, map[string]any{"ok": true, "message": "知到登录成功，可以关闭此页"})
}

func zhidaoSlideErrorMessage(err error) string {
	switch {
	case errors.Is(err, platform.ErrPasswordRejected):
		return "知到手机号或密码错误，请检查已保存的凭据"
	case errors.Is(err, platform.ErrCaptchaRejected):
		return "知到未接受本次滑块验证，请重新滑动"
	default:
		return zhidaoAutoErrorMessage(err)
	}
}
