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

const Version = "0.3.3-dev"

type session struct{ expires time.Time }
type attempt struct {
	count int
	since time.Time
}
type Server struct {
	zhidaoMu                sync.Mutex
	zhidaoState             string
	zhidaoPreviousLogin     string
	zhidaoError             string
	zhidaoGeneration        uint64
	zhidaoCancel            context.CancelFunc
	zhidaoSyncCancel        context.CancelFunc
	startZhidao             func(context.Context) (*rainFlow, error)
	newZhidao               func(string) rainAdapter
	zhidaoAutoMu            sync.Mutex
	zhidaoAutoState         string
	zhidaoAutoStage         string
	zhidaoAutoError         string
	zhidaoAutoAt            *time.Time
	zhidaoAutoAttempt       time.Time
	zhidaoInClassMu         sync.Mutex
	zhidaoInClassStatus     zhidaoInClassState
	zhidaoClassroomBaseline map[string]bool
	smsSource               smsSource
	passwordLogin           func(context.Context, string, string, platform.SMSCollector) (string, error)
	slideLogin              func(context.Context, string, string, string, platform.SMSCollector) (string, error)

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
	classroomEpoch    uint64 // Protected by rainMu, including the fields below.
	classrooms        map[string]*classroomWatch
	classroomStatus   classroomStatus
	openClassroom     func(context.Context, rainAdapter, platform.LiveLesson) (classroomChannel, error)
	analysisGate      chan struct{}
	analyzeProblem    func(context.Context, vault.Config, platform.ClassroomProblem) (string, error)
	lifetime          context.Context
	lifetimeCancel    context.CancelFunc
	lifeMu            sync.Mutex
	stopping          bool
	runStarted        bool
	syncQueued        bool
	syncWorker        bool
	jobs              sync.WaitGroup
	startRain         func(context.Context) (*rainFlow, error)
	newRain           func(string) rainAdapter
	sendBark          func(context.Context, string, string, string, string, bark.Options) error
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
		classrooms: make(map[string]*classroomWatch), openClassroom: defaultOpenClassroom, analysisGate: make(chan struct{}, 1), analyzeProblem: defaultAnalyzeProblem,
		startZhidao: defaultZhidaoFlow, newZhidao: func(session string) rainAdapter { return platform.NewZhidao(session) }, startRain: defaultRainFlow, newRain: func(cookie string) rainAdapter { return platform.NewRain(cookie) }, sendBark: func(ctx context.Context, server, key, title, body string, opts bark.Options) error {
			return bark.SendWithOptions(ctx, server, key, title, body, opts)
		},
		smsSource: newSMSSourceFromEnv(), passwordLogin: platform.PasswordLogin, slideLogin: platform.SlideLogin}, nil
}

func (s *Server) Handler(files fs.FS) http.Handler {
	return s.handler(staticHandler(files))
}

// handler accepts a shared static handler so a multi-user deployment does not
// recompress and retain another copy of the web assets for every account.
func (s *Server) handler(static http.Handler) http.Handler {
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
	mux.HandleFunc("GET /api/analysis", s.protected(s.getAnalysis))
	mux.HandleFunc("PUT /api/analysis", s.protected(s.setAnalysis))
	mux.HandleFunc("POST /api/bark/test", s.protected(s.testBark))
	mux.HandleFunc("POST /api/in-class/test", s.protected(s.testInClass))
	mux.HandleFunc("POST /api/accounts/rain/login", s.protected(s.startRainLogin))
	mux.HandleFunc("POST /api/accounts/zhidao/login", s.protected(s.startZhidaoLogin))
	mux.HandleFunc("GET /api/accounts/zhidao/login", s.protected(s.zhidaoLoginStatus))
	mux.HandleFunc("POST /api/accounts/zhidao/login/cancel", s.protected(s.cancelZhidaoLogin))
	mux.HandleFunc("GET /api/zhidao", s.protected(s.getZhidaoAutoLogin))
	mux.HandleFunc("PUT /api/zhidao/auto-login", s.protected(s.setZhidaoAutoLogin))
	mux.HandleFunc("POST /api/zhidao/auto-login/test", s.protected(s.testZhidaoAutoLogin))
	mux.HandleFunc("POST /api/zhidao/slider", s.protected(s.slideZhidaoLogin))
	mux.HandleFunc("GET /api/accounts/rain/login", s.protected(s.rainLoginStatus))
	mux.HandleFunc("POST /api/accounts/rain/login/cancel", s.protected(s.cancelRainLogin))
	mux.HandleFunc("POST /api/accounts/rain/assist", s.protected(s.startRainAssist))
	mux.HandleFunc("GET /api/accounts/rain/assist/{id}", s.protected(s.rainAssistStatus))
	mux.HandleFunc("POST /api/accounts/rain/assist/{id}/cancel", s.protected(s.cancelRainAssist))
	mux.HandleFunc("POST /api/accounts/rain/assist/{id}/connect", s.connectRainAssist)
	mux.HandleFunc("GET /api/accounts/rain/assist/{id}/helper", s.rainAssistHelperStatus)
	mux.HandleFunc("POST /api/accounts/rain/assist/{id}/session", s.uploadRainAssist)
	mux.HandleFunc("GET /api/events", s.protected(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, trimEvents(s.Engine.Snapshot().Events, 200))
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
	mux.Handle("/", static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		applySliderSecurity(w, r.URL.Path)
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

// trimEvents bounds the API payload without dropping reminders that are still
// scheduled to send. Terminal history is ordered newest-first behind them; an
// older pending event must not disappear merely because newer deliveries exist.
func trimEvents(events []core.Event, limit int) []core.Event {
	active := make([]core.Event, 0, len(events))
	history := make([]core.Event, 0, len(events))
	for _, event := range events {
		if event.Status == "pending" || event.Status == "sending" {
			active = append(active, event)
		} else {
			history = append(history, event)
		}
	}
	sort.SliceStable(active, func(i, j int) bool { return active[i].DueAt.Before(active[j].DueAt) })
	sort.SliceStable(history, func(i, j int) bool { return history[i].CreatedAt.After(history[j].CreatedAt) })
	if len(active) >= limit {
		return active[:limit]
	}
	trimmed := append(active, history...)
	if len(trimmed) > limit {
		trimmed = trimmed[:limit]
	}
	return trimmed
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
	hash := sha256.Sum256([]byte(input.Password))
	if subtle.ConstantTimeCompare(hash[:], s.password[:]) != 1 {
		// Count failures only. Checking the limiter before verifying the password
		// would let anyone who can reach /api/login lock the administrator out by
		// burning the shared reverse-proxy address's budget with wrong guesses.
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
		fail(w, 401, "管理密码不正确")
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
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
		// MultiServer authenticates the account once at its outer boundary and
		// passes an authorized context into this account-local worker. The legacy
		// single-user Server still uses its own persisted session map in tests and
		// when constructed directly.
		if _, ok := r.Context().Value(tenantAuthorizedKey{}).(*tenantAuthorization); ok {
			next(w, r)
			return
		}
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
	// Serialize the in-class enabled flag with a live poll's final commit.
	s.rainMu.Lock()
	previous := s.Engine.Snapshot().Settings
	err := s.Engine.UpdateSettings(settings)
	if err == nil && (previous.RainFallbackEnabled != settings.RainFallbackEnabled || previous.RainAnalysisEnabled != settings.RainAnalysisEnabled) {
		s.invalidateClassroomsLocked()
	}
	s.rainMu.Unlock()
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	// Echo what was stored, not the request: UpdateSettings normalizes values
	// (for example in_class_poll_seconds:0 becomes the default) and a client
	// comparing this response with a later GET must see the same settings.
	jsonResponse(w, 200, s.Engine.Snapshot().Settings)
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
	err := s.sendBark(r.Context(), c.BarkURL, c.BarkKey, "DueRadar 测试通知", "提醒服务已连接，学习任务将按你的规则通知。", bark.Options{})
	now := time.Now()
	s.mu.Lock()
	s.lastTestAt = &now
	s.lastTestError = ""
	if err != nil {
		// Transport implementations may include request URLs, response bodies,
		// or other provider details. Keep those out of both the API response and
		// the persisted-in-process status shown by GET /api/bark.
		s.lastTestError = "Bark 测试通知失败，请检查配置及网络"
	}
	s.mu.Unlock()
	if err != nil {
		fail(w, 502, "Bark 测试通知失败，请检查配置及网络")
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
	zhidaoPoll := s.zhidaoInClassSnapshot()
	settings := s.Engine.Snapshot().Settings
	s.rainMu.Lock()
	classroom := s.classroomStatus
	classroom.Connected = 0
	for _, watch := range s.classrooms {
		if watch.channel != nil && s.classroomCurrentLocked(watch) {
			classroom.Connected++
		}
	}
	s.rainMu.Unlock()
	var lastCheck any
	if !inClass.LastCheckAt.IsZero() {
		lastCheck = inClass.LastCheckAt
	}
	var lastQuiz any
	if !inClass.LastQuizAt.IsZero() {
		lastQuiz = inClass.LastQuizAt
	}
	var zhidaoLastCheck, zhidaoLastPoll any
	if !zhidaoPoll.LastCheckAt.IsZero() {
		zhidaoLastCheck = zhidaoPoll.LastCheckAt
	}
	if !zhidaoPoll.LastPollAt.IsZero() {
		zhidaoLastPoll = zhidaoPoll.LastPollAt
	}
	jsonResponse(w, 200, map[string]any{"version": Version, "demo": s.Demo, "uptime_seconds": int(time.Since(s.started).Seconds()), "sync_interval_seconds": 900, "scheduler_interval_seconds": 15, "platforms_ready": 2, "go_version": runtime.Version(), "message": "普通雨课堂和知到校内课支持扫码绑定；共享课待实测", "notification_error": deliveryError, "notification_last_attempt_at": deliveryAt, "notification_failure_count": health.UnresolvedFailures, "notification_last_success_at": health.LastSuccessAt,
		"classroom": classroom, "rain_fallback_enabled": settings.RainFallbackEnabled, "rain_analysis_enabled": settings.RainAnalysisEnabled,
		"in_class_enabled": settings.InClassEnabled, "in_class_poll_seconds": settings.InClassPollSeconds, "in_class_last_check_at": lastCheck, "in_class_lessons": inClass.Lessons, "in_class_last_quiz_at": lastQuiz, "in_class_last_quiz": inClass.LastQuiz, "in_class_detected": inClass.Detected, "in_class_error": inClass.Error,
		"zhidao_in_class_enabled": settings.ZhidaoInClassEnabled, "zhidao_in_class_last_check_at": zhidaoLastCheck, "zhidao_in_class_lessons": zhidaoPoll.Lessons, "zhidao_in_class_last_poll_at": zhidaoLastPoll, "zhidao_in_class_last_poll": zhidaoPoll.LastPoll, "zhidao_in_class_detected": zhidaoPoll.Detected, "zhidao_in_class_error": zhidaoPoll.Error})
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
	ctx, roundCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer roundCancel()
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
		_ = s.failRainSync(ctx, generation, cookie, errors.Is(err, platform.ErrSessionExpired), "登录验证失败")
		return err
	}
	if err := s.commitRainSync(ctx, generation, func() error {
		if s.Vault.Get().RainCookie != cookie {
			return errSupersededLogin
		}
		return s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "logged_in"})
	}); err != nil {
		return err
	}
	tasks, err := adapter.Tasks(ctx)
	if err != nil {
		_ = s.failRainSync(ctx, generation, cookie, errors.Is(err, platform.ErrSessionExpired), "任务采集失败")
		return err
	}
	updated, err := sessionCredentials(adapter, cookie)
	if err != nil {
		_ = s.failRainSync(ctx, generation, cookie, false, "会话续期读取失败")
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
		_ = s.failRainSync(ctx, generation, cookie, false, "同步结果保存失败")
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
	s.deliverWithGuard(ctx, nil)
}

// deliverWithGuard lets a caller fence an immediate delivery against a changed
// provider generation. The guard is called under notifyMu, never during I/O.
func (s *Server) deliverWithGuard(ctx context.Context, valid func() bool) {
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
		if ctx.Err() != nil || valid != nil && !valid() {
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
		if ctx.Err() != nil || !s.Engine.ClaimStillValidClaim(event, time.Now()) || valid != nil && !valid() {
			s.deliveryResult = &deliveryOutcome{unsent: &event}
			if err := s.persistDelivery(); err != nil {
				s.setDeliveryError("通知结果保存失败，发送已暂停；请检查数据目录")
				return
			}
			continue
		}
		opts := bark.Options{}
		if event.Category == "poll" && s.Engine.Settings().PollWakeEnabled {
			opts = bark.Wake()
		}
		err = s.sendBark(ctx, c.BarkURL, c.BarkKey, event.Title, event.Body, opts)
		s.deliveryResult = &deliveryOutcome{claim: event, sent: err == nil, at: time.Now()}
		if persistErr := s.persistDelivery(); persistErr != nil {
			s.setDeliveryError("通知结果保存失败，发送已暂停；请检查数据目录")
			return
		}
	}
}
func (s *Server) Run(ctx context.Context) {
	s.lifeMu.Lock()
	if s.runStarted || s.stopping {
		s.lifeMu.Unlock()
		return
	}
	s.runStarted = true
	s.lifeMu.Unlock()
	stop := context.AfterFunc(ctx, s.stopBackground)
	defer stop()
	s.startJob(func(ctx context.Context) {
		syncOnce := func() {
			// Bound terminal history even when no platform is currently bound.
			_ = s.Engine.Maintain(time.Now())
			s.queueSync()
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
	s.startJob(s.runZhidaoInClass)
	s.startJob(s.runClassrooms)
	s.startJob(func(ctx context.Context) {
		// Give the startup sync a moment to validate or rotate the stored
		// session before the renewal check decides it is near expiry.
		startup := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			startup.Stop()
			return
		case <-startup.C:
		}
		s.maybeAutoLoginZhidao()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.maybeAutoLoginZhidao()
			}
		}
	})
	<-s.lifetime.Done()
	// The lifetime cancellation stops the ticker loops, but a platform request
	// or final durable delivery may still be unwinding. Keep the worker alive
	// until every owned job has completed its last persistence operation.
	s.jobs.Wait()
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
