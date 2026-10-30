package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethandong16/due-radar/internal/bark"
	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

const Version = "0.3.2-dev"

type session struct{ expires time.Time }
type attempt struct {
	count int
	since time.Time
}
type Server struct {
	zhidaoMu            sync.Mutex
	zhidaoState         string
	zhidaoPreviousLogin string
	zhidaoError         string
	zhidaoGeneration    uint64
	zhidaoCancel        context.CancelFunc
	zhidaoSyncCancel    context.CancelFunc
	startZhidao         func(context.Context) (*rainFlow, error)
	newZhidao           func(string) rainAdapter

	Engine            *core.Engine
	Vault             *vault.Vault
	Demo              bool
	SecureCookies     bool
	password          [32]byte
	started           time.Time
	mu                sync.Mutex
	sessions          map[string]session
	sessionsPath      string
	attempts          map[string]attempt
	syncGate          chan struct{}
	notifyMu          sync.Mutex
	lastTestAt        *time.Time
	lastTestError     string
	rainMu            sync.Mutex
	rainState         string
	rainPreviousLogin string
	rainError         string
	rainGeneration    uint64
	rainCancel        context.CancelFunc
	rainSyncCancel    context.CancelFunc
	rainAssists       map[string]*rainAssist
	rainAssistActive  *rainAssist
	inClassMu         sync.Mutex
	inClassStatus     inClassStatus
	lifetime          context.Context
	lifetimeCancel    context.CancelFunc
	lifeMu            sync.Mutex
	stopping          bool
	jobs              sync.WaitGroup
	startRain         func(context.Context) (*rainFlow, error)
	newRain           func(string) rainAdapter
	sendBark          func(context.Context, string, string, string, string) error
	deliveryError     string // Active queue/result persistence error; transport health is derived from durable events.
	deliveryAt        *time.Time
	deliveryResult    *deliveryOutcome
}

func New(engine *core.Engine, credentials *vault.Vault, password string, demo, secure bool) (*Server, error) {
	if len(password) < 12 {
		return nil, errors.New("DUERADAR_ADMIN_PASSWORD must contain at least 12 characters")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{Engine: engine, Vault: credentials, Demo: demo, SecureCookies: secure, password: sha256.Sum256([]byte(password)), started: time.Now(), sessions: map[string]session{}, attempts: map[string]attempt{}, syncGate: make(chan struct{}, 1), lifetime: ctx, lifetimeCancel: cancel,
		startZhidao: defaultZhidaoFlow, newZhidao: func(session string) rainAdapter { return platform.NewZhidao(session) }, startRain: defaultRainFlow, newRain: func(cookie string) rainAdapter { return platform.NewRain(cookie) }, sendBark: bark.Send}, nil
}

func (s *Server) Handler(files fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("GET /api/session", s.protected(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]bool{"authenticated": true})
	}))
	mux.HandleFunc("POST /api/logout", s.protected(s.logout))
	mux.HandleFunc("GET /api/tasks", s.protected(func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, s.Engine.Snapshot().Tasks) }))
	mux.HandleFunc("GET /api/accounts", s.protected(s.accounts))
	mux.HandleFunc("GET /api/settings", s.protected(func(w http.ResponseWriter, r *http.Request) {
		settings := s.Engine.Snapshot().Settings
		jsonResponse(w, 200, settings)
	}))
	mux.HandleFunc("PUT /api/settings", s.protected(s.settings))
	mux.HandleFunc("GET /api/bark", s.protected(s.getBark))
	mux.HandleFunc("PUT /api/bark", s.protected(s.setBark))
	mux.HandleFunc("POST /api/bark/test", s.protected(s.testBark))
	mux.HandleFunc("POST /api/in-class/test", s.protected(s.testInClass))
	mux.HandleFunc("POST /api/accounts/rain/login", s.protected(s.startRainLogin))
	mux.HandleFunc("POST /api/accounts/zhidao/login", s.protected(s.startZhidaoLogin))
	mux.HandleFunc("GET /api/accounts/zhidao/login", s.protected(s.zhidaoLoginStatus))
	mux.HandleFunc("POST /api/accounts/zhidao/login/cancel", s.protected(s.cancelZhidaoLogin))
	mux.HandleFunc("GET /api/accounts/rain/login", s.protected(s.rainLoginStatus))
	mux.HandleFunc("POST /api/accounts/rain/login/cancel", s.protected(s.cancelRainLogin))
	mux.HandleFunc("POST /api/accounts/rain/assist", s.protected(s.startRainAssist))
	mux.HandleFunc("GET /api/accounts/rain/assist/{id}", s.protected(s.rainAssistStatus))
	mux.HandleFunc("POST /api/accounts/rain/assist/{id}/cancel", s.protected(s.cancelRainAssist))
	mux.HandleFunc("POST /api/accounts/rain/assist/{id}/connect", s.connectRainAssist)
	mux.HandleFunc("GET /api/accounts/rain/assist/{id}/helper", s.rainAssistHelperStatus)
	mux.HandleFunc("POST /api/accounts/rain/assist/{id}/session", s.uploadRainAssist)
	mux.HandleFunc("GET /api/events", s.protected(func(w http.ResponseWriter, r *http.Request) {
		events := s.Engine.Snapshot().Events
		sort.SliceStable(events, func(i, j int) bool { return events[i].CreatedAt.After(events[j].CreatedAt) })
		if len(events) > 200 {
			events = events[:200]
		}
		jsonResponse(w, 200, events)
	}))
	mux.HandleFunc("GET /api/status", s.protected(s.status))
	mux.HandleFunc("POST /api/sync", s.protected(func(w http.ResponseWriter, r *http.Request) {
		if !s.Demo && s.Vault.Get().RainCookie == "" && s.Vault.Get().ZhidaoSession == "" {
			fail(w, 409, "请先绑定平台账号")
			return
		}
		if !s.queueSync() {
			fail(w, 503, "服务正在停止")
			return
		}
		jsonResponse(w, 202, map[string]string{"message": "同步已开始，请稍后查看平台结果"})
	}))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "接口不存在") })
	mux.Handle("/", staticHandler(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Method != "HEAD" {
			// Reject cross-site browser mutations, including login CSRF. CLI calls
			// without Origin are allowed; authenticated cookies remain mandatory.
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
					fail(w, 403, "请求来源不匹配")
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, "请求来源不匹配")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		fail(w, 415, "请使用 application/json")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "JSON 格式或字段无效")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "请求包含多余或过大的数据")
		return false
	}
	return true
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	s.mu.Lock()
	for k, a := range s.attempts {
		if now.Sub(a.since) > 15*time.Minute {
			delete(s.attempts, k)
		}
	}
	a := s.attempts[ip]
	if a.count >= 10 || len(s.attempts) >= 2048 {
		s.mu.Unlock()
		fail(w, 429, "尝试次数过多，请 15 分钟后重试")
		return
	}
	if a.since.IsZero() {
		a.since = now
	}
	a.count++
	s.attempts[ip] = a
	s.mu.Unlock()
	hash := sha256.Sum256([]byte(input.Password))
	if subtle.ConstantTimeCompare(hash[:], s.password[:]) != 1 {
		fail(w, 401, "管理密码不正确")
		return
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		fail(w, 500, "会话创建失败")
		return
	}
	token := hex.EncodeToString(tokenBytes)
	s.mu.Lock()
	delete(s.attempts, ip)
	for k, v := range s.sessions {
		if !v.expires.After(now) {
			delete(s.sessions, k)
		}
	}
	// Long-lived sessions accumulate; evict the oldest instead of locking the
	// administrator out once the cap is reached.
	if len(s.sessions) >= 128 {
		oldestKey := ""
		var oldestExpiry time.Time
		for key, value := range s.sessions {
			if oldestKey == "" || value.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, value.expires
			}
		}
		delete(s.sessions, oldestKey)
	}
	s.sessions[sessionKey(token)] = session{now.Add(sessionLifetime)}
	s.persistSessionsLocked()
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "dueradar_session", Value: token, Path: "/", HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime / time.Second)})
	jsonResponse(w, 200, map[string]bool{"authenticated": true})
}

func (s *Server) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("dueradar_session")
		if err != nil {
			fail(w, 401, "请先登录管理页面")
			return
		}
		s.mu.Lock()
		key := sessionKey(c.Value)
		v, ok := s.sessions[key]
		if ok && !v.expires.After(time.Now()) {
			delete(s.sessions, key)
			ok = false
		}
		s.mu.Unlock()
		if !ok {
			fail(w, 401, "管理会话已过期，请重新登录")
			return
		}
		next(w, r)
	}
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("dueradar_session"); err == nil {
		s.mu.Lock()
		delete(s.sessions, sessionKey(c.Value))
		s.persistSessionsLocked()
		s.mu.Unlock()
		s.cancelRainAssistOwner(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "dueradar_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode})
	jsonResponse(w, 200, map[string]bool{"ok": true})
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	type view struct {
		core.Account
		Name    string `json:"name"`
		Ready   bool   `json:"ready"`
		Message string `json:"message"`
	}
	out := []view{}
	for _, capability := range platform.Capabilities() {
		for _, a := range s.Engine.Snapshot().Accounts {
			if a.Platform == capability.ID {
				if s.Demo {
					a.LoginState = "demo"
					a.SyncState = "demo"
				}
				out = append(out, view{a, capability.Name, capability.Ready, capability.Message})
			}
		}
	}
	jsonResponse(w, 200, out)
}
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	var settings core.Settings
	if !decode(w, r, &settings) {
		return
	}
	if err := s.Engine.UpdateSettings(settings); err != nil {
		fail(w, 400, err.Error())
		return
	}
	jsonResponse(w, 200, settings)
}
func (s *Server) getBark(w http.ResponseWriter, r *http.Request) {
	c := s.Vault.Get()
	if c.BarkURL == "" {
		c.BarkURL = "https://api.day.app"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jsonResponse(w, 200, map[string]any{"server": c.BarkURL, "configured": c.BarkKey != "", "last_test_at": s.lastTestAt, "last_error": s.lastTestError})
}
func (s *Server) setBark(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "演示模式不保存通知凭据")
		return
	}
	var input struct {
		Server string `json:"server"`
		Key    string `json:"key"`
	}
	if !decode(w, r, &input) {
		return
	}
	var validationErr error
	if err := s.Vault.Update(func(c *vault.Config) error {
		key := input.Key
		if key == "" {
			if input.Server != c.BarkURL && c.BarkKey != "" {
				validationErr = errors.New("修改 Bark 服务地址时请重新填写 Key")
				return validationErr
			}
			key = c.BarkKey
		}
		if validationErr = bark.Validate(input.Server, key); validationErr != nil {
			return validationErr
		}
		c.BarkURL, c.BarkKey = input.Server, key
		return nil
	}); err != nil {
		if validationErr != nil {
			fail(w, 400, validationErr.Error())
			return
		}
		fail(w, 500, "通知配置保存失败")
		return
	}
	s.getBark(w, r)
}
func (s *Server) testBark(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "演示模式不发送真实通知")
		return
	}
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	c := s.Vault.Get()
	err := s.sendBark(r.Context(), c.BarkURL, c.BarkKey, "DueRadar 测试通知", "提醒服务已连接，学习任务将按你的规则通知。")
	now := time.Now()
	s.mu.Lock()
	s.lastTestAt = &now
	s.lastTestError = ""
	if err != nil {
		s.lastTestError = err.Error()
	}
	s.mu.Unlock()
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	jsonResponse(w, 200, map[string]string{"message": "Bark 已接受测试通知"})
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	deliveryError, deliveryAt := s.deliveryError, s.deliveryAt
	s.mu.Unlock()
	health := s.Engine.DeliveryHealth(time.Now())
	if deliveryError == "" && health.UnresolvedFailures > 0 {
		deliveryError = "有通知发送失败，请查看发送记录并检查 Bark 配置及网络"
	}
	inClass := s.inClassSnapshot()
	settings := s.Engine.Snapshot().Settings
	var lastCheck any
	if !inClass.LastCheckAt.IsZero() {
		lastCheck = inClass.LastCheckAt
	}
	var lastQuiz any
	if !inClass.LastQuizAt.IsZero() {
		lastQuiz = inClass.LastQuizAt
	}
	jsonResponse(w, 200, map[string]any{"version": Version, "demo": s.Demo, "uptime_seconds": int(time.Since(s.started).Seconds()), "sync_interval_seconds": 900, "scheduler_interval_seconds": 15, "platforms_ready": 2, "go_version": runtime.Version(), "message": "普通雨课堂和知到校内课支持扫码绑定；共享课待实测", "notification_error": deliveryError, "notification_last_attempt_at": deliveryAt, "notification_failure_count": health.UnresolvedFailures, "notification_last_success_at": health.LastSuccessAt,
		"in_class_enabled": settings.InClassEnabled, "in_class_poll_seconds": settings.InClassPollSeconds, "in_class_last_check_at": lastCheck, "in_class_lessons": inClass.Lessons, "in_class_last_quiz_at": lastQuiz, "in_class_last_quiz": inClass.LastQuiz, "in_class_detected": inClass.Detected, "in_class_error": inClass.Error})
}

// Sync and Deliver have separate clocks; slow polling does not postpone known deadlines.
func (s *Server) Sync(ctx context.Context) error {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	select {
	case s.syncGate <- struct{}{}:
		defer func() { <-s.syncGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Demo {
		return s.seedDemo(time.Now())
	}
	// Start both platforms together so a slow platform cannot consume the
	// caller's entire deadline before its peer gets a chance to synchronize.
	// The round gate bounds this to two concurrent platform operations.
	var waiting sync.WaitGroup
	var results [2]error
	for i, synchronize := range []func(context.Context) error{s.syncRain, s.syncZhidao} {
		waiting.Add(1)
		go func(index int, synchronize func(context.Context) error) {
			defer waiting.Done()
			platformCtx, platformCancel := context.WithTimeout(ctx, 5*time.Minute)
			defer platformCancel()
			results[index] = synchronize(platformCtx)
		}(i, synchronize)
	}
	waiting.Wait()
	return errors.Join(results[:]...)
}
func (s *Server) syncRain(ctx context.Context) error {
	ctx, cancel := s.operationContext(ctx)
	defer cancel()
	s.rainMu.Lock()
	if s.rainState == "initializing" || s.rainState == "waiting" {
		s.rainMu.Unlock()
		return nil
	}
	generation := s.rainGeneration
	s.rainSyncCancel = cancel
	cookie := s.Vault.Get().RainCookie
	s.rainMu.Unlock()
	defer func() {
		s.rainMu.Lock()
		if generation == s.rainGeneration {
			s.rainSyncCancel = nil
		}
		s.rainMu.Unlock()
	}()
	if cookie == "" {
		return nil
	}
	adapter := s.newRain(cookie)
	if err := adapter.Validate(ctx); err != nil {
		_ = s.failRainSync(ctx, generation, errors.Is(err, platform.ErrSessionExpired), "登录验证失败")
		return err
	}
	if err := s.commitRainSync(ctx, generation, func() error {
		return s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "logged_in"})
	}); err != nil {
		return err
	}
	tasks, err := adapter.Tasks(ctx)
	if err != nil {
		_ = s.failRainSync(ctx, generation, errors.Is(err, platform.ErrSessionExpired), "任务采集失败")
		return err
	}
	updated, err := sessionCredentials(adapter, cookie)
	if err != nil {
		_ = s.failRainSync(ctx, generation, false, "会话续期读取失败")
		return err
	}
	// Keep the generation lock across both writes. Even an unchanged export
	// must still belong to the stored session before its tasks can be accepted.
	err = s.commitRainSync(ctx, generation, func() error {
		if s.Vault.Get().RainCookie != cookie {
			return errSupersededLogin
		}
		if updated != cookie {
			if err := s.Vault.Update(func(c *vault.Config) error {
				if c.RainCookie != cookie {
					return errSupersededLogin
				}
				c.RainCookie = updated
				return nil
			}); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Preserve valid renewed credentials if the task-state disk write fails;
		// restoring the old session could discard a platform-issued rotation.
		return s.Engine.Sync(core.PlatformRain, tasks, time.Now())
	})
	if err != nil && !errors.Is(err, errSupersededLogin) {
		_ = s.failRainSync(ctx, generation, false, "同步结果保存失败")
	}
	return err
}

// Export is optional for adapters without renewable sessions. Provider errors
// may contain cookies or signed URLs and must never escape to API callers.
func sessionCredentials(adapter rainAdapter, original string) (string, error) {
	exporter, ok := adapter.(interface{ Credentials() (string, error) })
	if !ok {
		return original, nil
	}
	updated, err := exporter.Credentials()
	if err != nil || strings.TrimSpace(updated) == "" {
		return "", errors.New("平台会话续期读取失败")
	}
	return updated, nil
}

func (s *Server) Deliver(ctx context.Context) {
	if s.Demo {
		return
	}
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.deliveryResult != nil {
		if err := s.persistDelivery(); err != nil {
			s.setDeliveryError("通知结果保存失败，发送已暂停；请检查数据目录")
			return
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		c := s.Vault.Get()
		if c.BarkKey == "" {
			return
		}
		events, err := s.Engine.PendingOneWithError(time.Now())
		if err != nil {
			s.setDeliveryError("通知队列保存失败，发送已暂停；请检查数据目录")
			return
		}
		s.setDeliveryError("")
		if len(events) == 0 {
			return
		}
		event := events[0]
		if ctx.Err() != nil || !s.Engine.ClaimStillValid(event.ID, time.Now()) {
			s.deliveryResult = &deliveryOutcome{unsent: &event}
			if err := s.persistDelivery(); err != nil {
				s.setDeliveryError("通知结果保存失败，发送已暂停；请检查数据目录")
				return
			}
			continue
		}
		err = s.sendBark(ctx, c.BarkURL, c.BarkKey, event.Title, event.Body)
		s.deliveryResult = &deliveryOutcome{id: event.ID, sent: err == nil, at: time.Now()}
		if persistErr := s.persistDelivery(); persistErr != nil {
			s.setDeliveryError("通知结果保存失败，发送已暂停；请检查数据目录")
			return
		}
	}
}
func (s *Server) Run(ctx context.Context) {
	stop := context.AfterFunc(ctx, s.stopBackground)
	defer stop()
	s.startJob(func(ctx context.Context) {
		syncOnce := func() {
			syncCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			_ = s.Sync(syncCtx)
		}
		syncOnce()
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncOnce()
			}
		}
	})
	s.startJob(func(ctx context.Context) {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Deliver(ctx)
			}
		}
	})
	s.startJob(s.runInClass)
	<-s.lifetime.Done()
}

func (s *Server) seedDemo(now time.Time) error {
	state := s.Engine.Snapshot()
	if len(state.Tasks) > 0 {
		for _, p := range []string{core.PlatformRain, core.PlatformZhidaoShared, core.PlatformZhidaoCampus} {
			tasks := []core.Task{}
			for _, t := range state.Tasks {
				if t.Platform == p {
					tasks = append(tasks, t)
				}
			}
			if err := s.Engine.Sync(p, tasks, now); err != nil {
				return err
			}
		}
		return nil
	}
	end := now.Add(26 * time.Hour)
	examStart := now.Add(50 * time.Hour)
	examEnd := examStart.Add(2 * time.Hour)
	videoEnd := now.Add(5 * 24 * time.Hour)
	progress := 62.5
	items := map[string][]core.Task{
		core.PlatformRain:         {{ID: "demo-homework", Title: "第三章 · 极限与连续", Course: "高等数学 A", Kind: "homework", Status: "pending", EndAt: &end, SourceTimezone: "Asia/Shanghai"}, {ID: "demo-exam", Title: "期中测验", Course: "大学物理", Kind: "exam", Status: "pending", StartAt: &examStart, EndAt: &examEnd, SourceTimezone: "Asia/Shanghai"}},
		core.PlatformZhidaoShared: {{ID: "demo-video", Title: "本周视频学习", Course: "中国传统文化", Kind: "video", Status: "pending", EndAt: &videoEnd, Progress: &progress, SourceTimezone: "Asia/Shanghai"}},
		core.PlatformZhidaoCampus: {{ID: "demo-done", Title: "第二章自测", Course: "程序设计基础", Kind: "chapter", Status: "completed", EndAt: &end}, {ID: "demo-unknown", Title: "课程实践安排", Course: "创新创业基础", Kind: "homework", Status: "unknown"}},
	}
	for _, p := range []string{core.PlatformRain, core.PlatformZhidaoShared, core.PlatformZhidaoCampus} {
		if err := s.Engine.Sync(p, items[p], now); err != nil {
			return err
		}
	}
	return nil
}
