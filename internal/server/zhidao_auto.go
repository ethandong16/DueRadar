package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

const (
	zhidaoAutoLoginLead   = time.Hour
	zhidaoAutoLoginRetry  = 30 * time.Minute
	zhidaoAutoLoginWindow = 150 * time.Second
)

var (
	errAutoLoginNotConfigured = errors.New("自动登录尚未配置")
	errAutoLoginBusy          = errors.New("自动登录正在进行")
)

type zhidaoAutoView struct {
	Enabled          bool       `json:"enabled"`
	Configured       bool       `json:"configured"`
	Phone            string     `json:"phone"`
	SMSConfigured    bool       `json:"sms_configured"`
	CaptchaID        string     `json:"captcha_id"`
	SessionExpiresAt *time.Time `json:"session_expires_at"`
	State            string     `json:"state"`
	Stage            string     `json:"stage"`
	Error            string     `json:"error"`
	LastRunAt        *time.Time `json:"last_run_at"`
}

func (s *Server) autoLoginConfigured(c vault.Config) bool {
	return c.ZhidaoPhone != "" && c.ZhidaoPassword != "" && s.smsSource != nil && s.passwordLogin != nil
}

func maskPhone(phone string) string {
	if len(phone) < 8 {
		return phone
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

func (s *Server) zhidaoAutoView() zhidaoAutoView {
	c := s.Vault.Get()
	s.zhidaoAutoMu.Lock()
	view := zhidaoAutoView{
		State:     s.zhidaoAutoState,
		Stage:     s.zhidaoAutoStage,
		Error:     s.zhidaoAutoError,
		LastRunAt: s.zhidaoAutoAt,
	}
	s.zhidaoAutoMu.Unlock()
	view.Enabled = s.Engine.Snapshot().Settings.ZhidaoAutoLogin
	view.Configured = c.ZhidaoPhone != "" && c.ZhidaoPassword != ""
	view.Phone = maskPhone(c.ZhidaoPhone)
	view.SMSConfigured = s.smsSource != nil
	view.CaptchaID = platform.ZhidaoCaptchaID
	if expiry, ok := platform.ZhidaoSessionExpiry(c.ZhidaoSession); ok {
		view.SessionExpiresAt = &expiry
	}
	return view
}

func (s *Server) getZhidaoAutoLogin(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, 200, s.zhidaoAutoView())
}

func (s *Server) setZhidaoAutoLogin(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "演示模式不保存账号凭据")
		return
	}
	var input struct {
		Enabled  *bool  `json:"enabled"`
		Phone    string `json:"phone"`
		Password string `json:"password"`
		Clear    bool   `json:"clear"`
	}
	if !decode(w, r, &input) {
		return
	}
	phone := strings.ReplaceAll(strings.TrimSpace(input.Phone), " ", "")
	phone = strings.TrimPrefix(phone, "+86")
	if phone != "" && !validPhone(phone) {
		fail(w, 400, "请填写 11 位中国大陆手机号")
		return
	}
	if len(input.Password) > 128 {
		fail(w, 400, "密码长度超出限制")
		return
	}
	c := s.Vault.Get()
	nextPhone, nextPassword := c.ZhidaoPhone, c.ZhidaoPassword
	if input.Clear {
		nextPhone, nextPassword = "", ""
	} else {
		if phone != "" {
			nextPhone = phone
		}
		if input.Password != "" {
			nextPassword = input.Password
		}
	}
	settings := s.Engine.Snapshot().Settings
	enabled := settings.ZhidaoAutoLogin
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	if input.Clear {
		enabled = false
	}
	if enabled && s.smsSource == nil {
		fail(w, 400, "短信模块尚未配置，无法启用自动登录")
		return
	}
	if enabled && (nextPhone == "" || nextPassword == "") {
		fail(w, 400, "启用自动登录前请填写手机号和密码")
		return
	}
	if err := s.Vault.Update(func(config *vault.Config) error {
		if input.Clear {
			config.ZhidaoPhone, config.ZhidaoPassword = "", ""
			return nil
		}
		if phone != "" {
			config.ZhidaoPhone = phone
		}
		if input.Password != "" {
			config.ZhidaoPassword = input.Password
		}
		return nil
	}); err != nil {
		fail(w, 500, "凭据保存失败，请检查数据目录")
		return
	}
	settings.ZhidaoAutoLogin = enabled
	if err := s.Engine.UpdateSettings(settings); err != nil {
		fail(w, 500, "自动登录设置保存失败")
		return
	}
	jsonResponse(w, 200, s.zhidaoAutoView())
}

func validPhone(phone string) bool {
	if len(phone) != 11 || phone[0] != '1' {
		return false
	}
	for _, c := range phone {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (s *Server) testZhidaoAutoLogin(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "演示模式不登录平台账号")
		return
	}
	if err := s.triggerZhidaoAutoLogin(true); err != nil {
		if errors.Is(err, errAutoLoginBusy) {
			fail(w, 409, "自动登录正在进行，请稍候")
			return
		}
		fail(w, 400, "请先保存手机号、密码，并配置短信模块")
		return
	}
	jsonResponse(w, 202, map[string]string{"message": "正在登录知到，请留意短信验证码"})
}

// maybeAutoLoginZhidao runs on the maintenance ticker. It only renews a
// session whose jt-cas expiry is within the lead window; an unknown expiry is
// acted on only after a sync has already marked the account expired.
func (s *Server) maybeAutoLoginZhidao() {
	if s.Demo || !s.Engine.Snapshot().Settings.ZhidaoAutoLogin {
		return
	}
	c := s.Vault.Get()
	if !s.autoLoginConfigured(c) {
		return
	}
	expiry, ok := platform.ZhidaoSessionExpiry(c.ZhidaoSession)
	if ok {
		if time.Until(expiry) > zhidaoAutoLoginLead {
			return
		}
	} else {
		expired := false
		for _, account := range s.Engine.Snapshot().Accounts {
			if account.Platform == core.PlatformZhidaoCampus && account.LoginState == "expired" {
				expired = true
				break
			}
		}
		if !expired {
			return
		}
	}
	_ = s.triggerZhidaoAutoLogin(false)
}

// renewExpiredZhidao reacts to an explicit platform 401. That is stronger
// evidence than the JWT clock, which may still look valid when a session is
// revoked server-side. The trigger remains throttled and opt-in.
func (s *Server) renewExpiredZhidao() {
	if s.Demo || !s.Engine.Snapshot().Settings.ZhidaoAutoLogin {
		return
	}
	_ = s.triggerZhidaoAutoLogin(false)
}

func (s *Server) triggerZhidaoAutoLogin(manual bool) error {
	s.zhidaoAutoMu.Lock()
	defer s.zhidaoAutoMu.Unlock()
	if s.zhidaoAutoState == "running" {
		return errAutoLoginBusy
	}
	if !manual && time.Since(s.zhidaoAutoAttempt) < zhidaoAutoLoginRetry {
		return nil
	}
	c := s.Vault.Get()
	if !s.autoLoginConfigured(c) {
		return errAutoLoginNotConfigured
	}
	// Do not race a QR binding or a manual rebind. The generation captured here
	// is checked again before the new session is written.
	s.zhidaoMu.Lock()
	busy := s.zhidaoState == "initializing" || s.zhidaoState == "waiting"
	generation := s.zhidaoGeneration
	s.zhidaoMu.Unlock()
	if busy {
		return errAutoLoginBusy
	}
	oldSession := c.ZhidaoSession
	s.zhidaoAutoAttempt = time.Now()
	s.zhidaoAutoState, s.zhidaoAutoStage, s.zhidaoAutoError = "running", "starting", ""
	started := s.startJob(func(ctx context.Context) {
		s.runZhidaoAutoLogin(ctx, generation, oldSession, c.ZhidaoPhone, c.ZhidaoPassword)
	})
	if !started {
		s.zhidaoAutoState, s.zhidaoAutoStage = "failed", ""
		s.zhidaoAutoError = "服务正在停止"
		return errAutoLoginBusy
	}
	return nil
}

type stagedCollector struct {
	inner  platform.SMSCollector
	onWait func()
}

func (c *stagedCollector) Baseline(ctx context.Context) (int64, error) {
	return c.inner.Baseline(ctx)
}

func (c *stagedCollector) WaitForCode(ctx context.Context, baseline int64, since time.Time) (string, error) {
	if c.onWait != nil {
		c.onWait()
	}
	return c.inner.WaitForCode(ctx, baseline, since)
}

func (s *Server) setAutoStage(stage string) {
	s.zhidaoAutoMu.Lock()
	if s.zhidaoAutoState == "running" {
		s.zhidaoAutoStage = stage
	}
	s.zhidaoAutoMu.Unlock()
}

func (s *Server) finishZhidaoAuto(state, message string) {
	now := time.Now()
	s.zhidaoAutoMu.Lock()
	s.zhidaoAutoState, s.zhidaoAutoStage, s.zhidaoAutoError = state, "", message
	s.zhidaoAutoAt = &now
	s.zhidaoAutoMu.Unlock()
}

func (s *Server) runZhidaoAutoLogin(ctx context.Context, generation uint64, oldSession, phone, password string) {
	ctx, cancel := context.WithTimeout(ctx, zhidaoAutoLoginWindow)
	defer cancel()
	collector := &stagedCollector{inner: newSMSCollector(s.smsSource), onWait: func() { s.setAutoStage("waiting_sms") }}
	s.setAutoStage("logging_in")
	updated, err := s.passwordLogin(ctx, phone, password, collector)
	if err != nil {
		s.finishZhidaoAuto("failed", zhidaoAutoErrorMessage(err))
		return
	}
	if strings.TrimSpace(updated) == "" {
		s.finishZhidaoAuto("failed", "知到登录未返回有效会话")
		return
	}
	// A password login must never replace one student's binding with another's.
	if oldNID := zhidaoIdentity(oldSession); oldNID != "" {
		if newNID := zhidaoIdentity(updated); newNID != "" && newNID != oldNID {
			s.finishZhidaoAuto("failed", "知到登录的账号与当前绑定不一致")
			return
		}
	}
	s.zhidaoMu.Lock()
	if generation != s.zhidaoGeneration || s.zhidaoState == "initializing" || s.zhidaoState == "waiting" {
		s.zhidaoMu.Unlock()
		s.finishZhidaoAuto("failed", "已有新的登录，本次自动登录已取消")
		return
	}
	if s.lifetime.Err() != nil {
		s.zhidaoMu.Unlock()
		s.finishZhidaoAuto("failed", "服务正在停止")
		return
	}
	if s.Vault.Get().ZhidaoSession != oldSession {
		s.zhidaoMu.Unlock()
		s.finishZhidaoAuto("failed", "已有新的登录，本次自动登录已取消")
		return
	}
	err = s.Vault.Update(func(c *vault.Config) error {
		if c.ZhidaoSession != oldSession {
			return errSupersededLogin
		}
		c.ZhidaoSession = updated
		return nil
	})
	if err != nil {
		s.zhidaoMu.Unlock()
		if errors.Is(err, errSupersededLogin) {
			s.finishZhidaoAuto("failed", "已有新的登录，本次自动登录已取消")
		} else {
			s.finishZhidaoAuto("failed", "知到会话保存失败，请检查数据目录")
		}
		return
	}
	// Invalidate any sync that still holds the previous session before it can
	// persist rotated credentials or tasks over this login.
	s.zhidaoGeneration++
	_ = s.Engine.SetAccount(core.Account{Platform: core.PlatformZhidaoCampus, LoginState: "logged_in"})
	s.zhidaoMu.Unlock()
	s.finishZhidaoAuto("success", "")
	s.queueSync()
}

func zhidaoIdentity(serialized string) string {
	var session platform.ZhidaoSession
	if json.Unmarshal([]byte(serialized), &session) != nil {
		return ""
	}
	return session.UserNID
}

func zhidaoAutoErrorMessage(err error) string {
	switch {
	case errors.Is(err, platform.ErrPasswordRejected):
		return "知到手机号或密码错误"
	case errors.Is(err, platform.ErrCaptchaRequired):
		return "知到要求图形验证码，请在「滑块续期」中手动完成一次"
	case errors.Is(err, platform.ErrSlideRequired):
		return "知到要求滑块验证，请在「滑块续期」中手动完成一次"
	case errors.Is(err, platform.ErrAccountLocked):
		return "知到账号暂时被限制登录"
	case errors.Is(err, platform.ErrSimplePassword):
		return "知到要求先修改密码"
	case errors.Is(err, platform.ErrNeedBinding):
		return "知到账号需要额外身份验证，无法自动登录"
	case errors.Is(err, platform.ErrSMSUnavailable):
		return "短信模块不可用，请检查部署配置与网络"
	case errors.Is(err, platform.ErrSMSTimeout):
		return "未在 90 秒内收到知到短信验证码"
	case errors.Is(err, platform.ErrSMSCodeRejected):
		return "知到未接受短信验证码"
	case errors.Is(err, platform.ErrLoginIncomplete):
		return "知到登录未完成，请改用扫码绑定"
	case errors.Is(err, errSupersededLogin):
		return "已有新的登录，本次自动登录已取消"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "自动登录超时或已取消"
	default:
		return "知到自动登录失败，请稍后重试或改用扫码绑定"
	}
}
