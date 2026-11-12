package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

const Version = "0.4.0-dev"

// The state file is read with a 64 MiB limit. Warn well before that ceiling so
// an operator can review retention or split the instance instead of discovering
// the limit only when the next start fails.
const stateSizeWarning = 16 << 20

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
	reminderTestAt    time.Time
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
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.lifetime.Err() != nil {
			jsonResponse(w, 503, map[string]string{"status": "stopping"})
			return
		}
		jsonResponse(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("GET /api/session", s.protected(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]bool{"authenticated": true})
	}))
	mux.HandleFunc("POST /api/logout", s.protected(s.logout))
	mux.HandleFunc("GET /api/tasks", s.protected(func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, s.Engine.Snapshot().Tasks) }))
	mux.HandleFunc("PUT /api/tasks/dismissal", s.protected(s.taskDismissal))
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
	mux.HandleFunc("POST /api/reminders/test", s.protected(s.testReminder))
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
	mux.HandleFunc("GET /api/export", s.protected(s.exportData))
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
func (s *Server) taskDismissal(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Platform  string `json:"platform"`
		ID        string `json:"id"`
		Dismissed bool   `json:"dismissed"`
	}
	if !decode(w, r, &input) {
		return
	}
	// Dismissal is a local reminder decision, never a claim that the platform
	// accepted the work. Demo state is ephemeral but may still be dismissed.
	if err := s.Engine.SetTaskDismissed(input.Platform, input.ID, input.Dismissed, time.Now()); err != nil {
		fail(w, 400, err.Error())
		return
	}
	jsonResponse(w, 200, map[string]bool{"ok": true})
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
func leadLabel(minutes int) string {
	switch {
	case minutes >= 1440 && minutes%1440 == 0:
		return fmt.Sprintf("%d 天", minutes/1440)
	case minutes >= 60 && minutes%60 == 0:
		return fmt.Sprintf("%d 小时", minutes/60)
	default:
		return fmt.Sprintf("%d 分钟", minutes)
	}
}

func leadList(offsets []int) string {
	parts := make([]string, 0, len(offsets))
	for _, n := range offsets {
		if n > 0 {
			parts = append(parts, leadLabel(n))
		}
	}
	if len(parts) == 0 {
		return "未设置提前量"
	}
	return strings.Join(parts, "、")
}

// testReminder sends one sample notification using the saved rule so the user
// can verify both the Bark channel and what a real reminder will look like. It
// never creates tasks or events. Quiet hours are reported rather than bypassed,
// except for the same short-notice exceptions real deliveries use.
func (s *Server) testReminder(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "演示模式不发送真实通知")
		return
	}
	var input struct {
		Kind string `json:"kind"`
	}
	if !decode(w, r, &input) {
		return
	}
	settings := s.Engine.Snapshot().Settings
	var offsets []int
	var title, item, verb string
	urgentLimit := 30
	switch input.Kind {
	case "", "deadline":
		offsets, item, verb = settings.DeadlineMinutes, "第三章作业", "截止"
		title = "任务即将截止 · 示例任务"
	case "video_deadline":
		offsets, item, verb = settings.VideoMinutes, "第 3 章视频", "截止"
		title = "视频任务即将截止 · 示例任务"
	case "exam_start":
		offsets, item, verb = settings.ExamStartMinutes, "期中考试", "开始"
		urgentLimit = 60
		title = "考试即将开始 · 示例考试"
	default:
		fail(w, 400, "未知的提醒类型")
		return
	}
	urgent := false
	first := 0
	for _, n := range offsets {
		if n <= 0 {
			continue
		}
		if first == 0 {
			first = n
		}
		if n <= urgentLimit {
			urgent = true
		}
	}
	if first == 0 {
		fail(w, 400, "该提醒类型尚未设置提前量，请先在提醒规则中填写")
		return
	}
	now := time.Now()
	if core.IsQuiet(settings, now) && !urgent {
		jsonResponse(w, 200, map[string]any{
			"sent":    false,
			"quiet":   true,
			"message": fmt.Sprintf("当前处于静默时段（%s–%s），不会发送测试通知。按当前规则，这类提醒会在静默结束后补发；考试开始前 60 分钟、截止前 30 分钟内的紧急提醒仍会即时推送。", settings.QuietStart, settings.QuietEnd),
		})
		return
	}
	body := fmt.Sprintf("示例课程 · %s\n将在 %s后%s。\n提醒规则：%s前。\n这是测试提醒，不会生成任务或提醒记录。", item, leadLabel(first), verb, leadList(offsets))
	s.notifyMu.Lock()
	if !s.reminderTestAt.IsZero() && now.Sub(s.reminderTestAt) < 10*time.Second {
		s.notifyMu.Unlock()
		fail(w, 429, "测试通知发送过于频繁，请稍后再试")
		return
	}
	s.reminderTestAt = now
	c := s.Vault.Get()
	err := s.sendBark(r.Context(), c.BarkURL, c.BarkKey, title, body, bark.Options{})
	s.mu.Lock()
	s.lastTestAt = &now
	s.lastTestError = ""
	if err != nil {
		s.lastTestError = "Bark 测试通知失败，请检查配置及网络"
	}
	s.mu.Unlock()
	s.notifyMu.Unlock()
	if err != nil {
		fail(w, 502, "测试提醒发送失败，请检查 Bark 配置及网络")
		return
	}
	jsonResponse(w, 200, map[string]any{
		"sent":    true,
		"quiet":   false,
		"message": fmt.Sprintf("已发送测试提醒。正式提醒会在%s前按当前规则推送。", leadList(offsets)),
	})
}

// exportData gives the user a portable copy of their own task and reminder
// history. Vault contents (platform cookies, Bark key, API key) are deliberately
// excluded: exports leave the device and must never carry credentials.
func (s *Server) exportData(w http.ResponseWriter, r *http.Request) {
	snapshot := s.Engine.Snapshot()
	now := time.Now().UTC()
	w.Header().Set("Content-Disposition", `attachment; filename="dueradar-export-`+now.Format("20060102-150405")+`.json"`)
	jsonResponse(w, 200, map[string]any{
		"version":     Version,
		"exported_at": now,
		"settings":    snapshot.Settings,
		"tasks":       snapshot.Tasks,
		"events":      snapshot.Events,
		"accounts":    snapshot.Accounts,
	})
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
	stateSize := s.Engine.StateSize()
	sizeWarning := ""
	if stateSize >= stateSizeWarning {
		sizeWarning = "状态文件已超过 16 MiB，请检查任务与事件保留情况或拆分实例"
	}
	jsonResponse(w, 200, map[string]any{"version": Version, "demo": s.Demo, "uptime_seconds": int(time.Since(s.started).Seconds()), "sync_interval_seconds": 900, "scheduler_interval_seconds": 15, "platforms_ready": 2, "go_version": runtime.Version(), "message": "普通雨课堂和知到校内课支持扫码绑定；共享课待实测", "notification_error": deliveryError, "notification_last_attempt_at": deliveryAt, "notification_failure_count": health.UnresolvedFailures, "notification_last_success_at": health.LastSuccessAt, "backup_error": s.Engine.BackupError(), "state_size_bytes": stateSize, "state_size_warning": sizeWarning,
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
		var retryAfter time.Duration
		var delayed interface{ RetryAfter() time.Duration }
		if err != nil && errors.As(err, &delayed) {
			retryAfter = delayed.RetryAfter()
		}
		s.deliveryResult = &deliveryOutcome{claim: event, sent: err == nil, permanent: errors.Is(err, bark.ErrPermanent), retryAfter: retryAfter, at: time.Now()}
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
		lastSyncUnix := time.Now().Unix()
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		// A laptop suspend, a long GC pause, or a manual clock change can leave
		// the ticker waiting while the snapshot goes stale. Reconcile as soon as
		// wall time and the last sync disagree by more than one interval.
		watch := time.NewTicker(time.Minute)
		defer watch.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncOnce()
				lastSyncUnix = time.Now().Unix()
			case <-watch.C:
				now := time.Now().Unix()
				if delta := now - lastSyncUnix; delta > 20*60 || delta < -60 {
					syncOnce()
					lastSyncUnix = now
				}
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
