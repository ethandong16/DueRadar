package server

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

const (
	multiSessionLifetime = 15 * 24 * time.Hour
	passwordIterations   = 600000
	maxUsers             = 16
	maxPasswordBytes     = 1024
	loginRetryInterval   = 2 * time.Second
)

type multiUser struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

type multiUserFile struct {
	Version int         `json:"version"`
	Users   []multiUser `json:"users"`
}

type multiSession struct {
	ID        string
	UserID    string
	Expires   time.Time
	CreatedAt time.Time
}

type multiStoredSession struct {
	ID        string    `json:"id,omitempty"`
	Key       string    `json:"key"`
	UserID    string    `json:"user_id"`
	Expires   time.Time `json:"expires"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type multiSessionFile struct {
	Version  int                  `json:"version"`
	Sessions []multiStoredSession `json:"sessions"`
}

type multiAttempt struct {
	Count       int
	Since       time.Time
	NextAllowed time.Time
}

// MultiServer owns the account registry and one ordinary Server worker per
// account. Keeping the existing Server as the worker means platform flows and
// the durable scheduler stay account-local without duplicating their code.
type MultiServer struct {
	dataDir           string
	secureCookies     bool
	allowRegistration bool
	usersPath         string
	sessionsPath      string
	static            http.Handler

	mu                sync.RWMutex
	users             map[string]multiUser
	byName            map[string]string
	tenants           map[string]*Server
	handlers          map[string]http.Handler
	sessions          map[string]multiSession
	attempts          map[string]multiAttempt
	signups           map[string]multiAttempt
	passwordSlots     chan struct{}
	dummyPasswordHash string

	lifetime       context.Context
	lifetimeCancel context.CancelFunc
	runStarted     bool
	jobs           sync.WaitGroup
	stopping       bool
	demo           bool
}

// OpenMulti opens the shared account registry. On a legacy data directory with
// no users.json, DUERADAR_ADMIN_PASSWORD becomes the password of the initial
// "owner" account while the existing root state/vault remain in place.
func OpenMulti(dataDir, bootstrapPassword string, secureCookies, allowRegistration, demo bool) (*MultiServer, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory is empty")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &MultiServer{
		dataDir: dataDir, secureCookies: secureCookies, allowRegistration: allowRegistration, demo: demo,
		usersPath: filepath.Join(dataDir, "users.json"), sessionsPath: filepath.Join(dataDir, "user-sessions.json"),
		users: map[string]multiUser{}, byName: map[string]string{}, tenants: map[string]*Server{}, handlers: map[string]http.Handler{},
		sessions: map[string]multiSession{}, attempts: map[string]multiAttempt{}, signups: map[string]multiAttempt{},
		passwordSlots: make(chan struct{}, 2), lifetime: ctx, lifetimeCancel: cancel,
	}
	var err error
	m.dummyPasswordHash, err = hashPassword("invalid-account-placeholder")
	if err != nil {
		cancel()
		return nil, err
	}
	if err := m.loadUsers(bootstrapPassword); err != nil {
		cancel()
		return nil, err
	}
	if err := m.loadSessions(); err != nil {
		cancel()
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.users) > maxUsers {
		cancel()
		return nil, errors.New("user registry exceeds the supported account limit")
	}
	ids := make([]string, 0, len(m.users))
	for id := range m.users {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		// A published account must never silently recreate missing data or keys.
		if err := m.openTenantLocked(m.users[id], false); err != nil {
			cancel()
			return nil, fmt.Errorf("open account %s: %w", m.users[id].Username, err)
		}
	}
	return m, nil
}

func (m *MultiServer) loadUsers(bootstrapPassword string) error {
	data, err := os.ReadFile(m.usersPath)
	if errors.Is(err, os.ErrNotExist) {
		// A missing registry must never turn an existing multi-user installation
		// into an apparently empty one, regardless of the bootstrap password.
		// An interrupted first registration can leave an orphan directory too;
		// require an operator to inspect it rather than guessing which case it is.
		usersDir := filepath.Join(m.dataDir, "users")
		entries, readErr := os.ReadDir(usersDir)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("inspect account directories: %w", readErr)
		}
		if len(entries) > 0 {
			return errors.New("user registry is missing while account data exists; restore the registry from a matching backup")
		}
		if strings.TrimSpace(bootstrapPassword) == "" {
			for _, name := range []string{"state.json", "master.key", "secrets.enc"} {
				if _, statErr := os.Stat(filepath.Join(m.dataDir, name)); statErr == nil {
					return errors.New("DUERADAR_ADMIN_PASSWORD is required to migrate the existing single-user data")
				} else if !errors.Is(statErr, os.ErrNotExist) {
					return statErr
				}
			}
			// A new installation may start with registration enabled and no
			// account at all. The first successful registration creates its own
			// isolated worker directory.
			return nil
		}
		if len(bootstrapPassword) < 12 || len(bootstrapPassword) > maxPasswordBytes {
			return errors.New("DUERADAR_ADMIN_PASSWORD must contain 12 to 1024 bytes to create the initial owner account")
		}
		// Validate/open the existing files before publishing the owner registry.
		// This leaves an interrupted upgrade retryable without accepting an empty
		// replacement for unreadable legacy data.
		if err := m.openTenantLocked(multiUser{ID: "owner"}, true); err != nil {
			return fmt.Errorf("open legacy owner data: %w", err)
		}
		hash, hashErr := hashPassword(bootstrapPassword)
		if hashErr != nil {
			return hashErr
		}
		owner := multiUser{ID: "owner", Username: "owner", PasswordHash: hash, CreatedAt: time.Now().UTC()}
		m.users[owner.ID], m.byName[owner.Username] = owner, owner.ID
		return m.saveUsersLocked()
	}
	if err != nil {
		return err
	}
	var file multiUserFile
	if err := json.Unmarshal(data, &file); err != nil || file.Version != 1 || len(file.Users) == 0 {
		return errors.New("user registry is invalid")
	}
	for _, user := range file.Users {
		if !validUserID(user.ID) || normalizeUsername(user.Username) != user.Username || user.PasswordHash == "" {
			return errors.New("user registry contains an invalid account")
		}
		if _, exists := m.users[user.ID]; exists || m.byName[user.Username] != "" {
			return errors.New("user registry contains duplicate accounts")
		}
		m.users[user.ID] = user
		m.byName[user.Username] = user.ID
	}
	return nil
}

func (m *MultiServer) loadSessions() error {
	data, err := os.ReadFile(m.sessionsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var file multiSessionFile
	if json.Unmarshal(data, &file) != nil || file.Version != 1 {
		// Sessions are disposable. A damaged session file must not block the
		// service; users can sign in again.
		return nil
	}
	now := time.Now()
	for _, stored := range file.Sessions {
		if len(stored.Key) != 64 || !validUserID(stored.UserID) || !stored.Expires.After(now) {
			continue
		}
		if _, exists := m.users[stored.UserID]; exists {
			id := stored.ID
			if id == "" {
				// Sessions written before IDs existed remain usable; assign an ID so
				// the account page can still list and revoke them.
				id, _ = randomToken(8)
			}
			m.sessions[stored.Key] = multiSession{ID: id, UserID: stored.UserID, Expires: stored.Expires, CreatedAt: stored.CreatedAt}
		}
	}
	return nil
}

func (m *MultiServer) saveUsersLocked() error {
	file := multiUserFile{Version: 1, Users: make([]multiUser, 0, len(m.users))}
	for _, user := range m.users {
		file.Users = append(file.Users, user)
	}
	sort.Slice(file.Users, func(i, j int) bool { return file.Users[i].Username < file.Users[j].Username })
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateAtomic(m.usersPath, data)
}

func (m *MultiServer) saveSessionsLocked() error {
	now := time.Now()
	file := multiSessionFile{Version: 1, Sessions: make([]multiStoredSession, 0, len(m.sessions))}
	for key, session := range m.sessions {
		if session.Expires.After(now) {
			file.Sessions = append(file.Sessions, multiStoredSession{ID: session.ID, Key: key, UserID: session.UserID, Expires: session.Expires, CreatedAt: session.CreatedAt})
		}
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return writePrivateAtomic(m.sessionsPath, data)
}

func writePrivateAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".dueradar-users-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if dir, err := os.Open(filepath.Dir(path)); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	return nil
}

func (m *MultiServer) openTenantLocked(user multiUser, create bool) error {
	if _, exists := m.tenants[user.ID]; exists {
		return nil
	}
	dir := m.dataDir
	if user.ID != "owner" {
		dir = filepath.Join(m.dataDir, "users", user.ID)
	}
	if !create {
		for _, name := range []string{"state.json", "master.key"} {
			if info, err := os.Stat(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("required account file %s: %w", name, err)
			} else if !info.Mode().IsRegular() {
				return fmt.Errorf("required account file %s is not a regular file", name)
			}
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Never generate a replacement key beside existing ciphertext, including
	// when opening a legacy owner directory.
	if _, err := os.Stat(filepath.Join(dir, "secrets.enc")); err == nil {
		if _, keyErr := os.Stat(filepath.Join(dir, "master.key")); keyErr != nil {
			return fmt.Errorf("credential key unavailable: %w", keyErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	engine, err := core.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		return err
	}
	credentials, err := vault.Open(dir)
	if err != nil {
		return err
	}
	internalPassword, err := randomToken(24)
	if err != nil {
		return err
	}
	worker, err := New(engine, credentials, internalPassword, m.demo, m.secureCookies)
	if err != nil {
		return err
	}
	m.tenants[user.ID] = worker
	return nil
}

func (m *MultiServer) tenantHandlerLocked(id string) (http.Handler, bool) {
	if handler, ok := m.handlers[id]; ok {
		return handler, true
	}
	worker, ok := m.tenants[id]
	if !ok {
		return nil, false
	}
	handler := worker.handler(m.static)
	m.handlers[id] = handler
	return handler, true
}

func (m *MultiServer) Handler(files fs.FS) http.Handler {
	m.mu.Lock()
	if m.static == nil {
		m.static = staticHandler(files)
	}
	m.mu.Unlock()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setMultiSecurityHeaders(w)
		applySliderSecurity(w, r.URL.Path)
		if r.URL.Path == "/healthz" {
			jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		if r.URL.Path == "/readyz" {
			m.mu.RLock()
			stopping := m.stopping
			m.mu.RUnlock()
			if stopping {
				jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"status": "stopping"})
				return
			}
			jsonResponse(w, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !multiMutationAllowed(w, r) {
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			m.dispatchAPI(w, r)
			return
		}
		m.mu.RLock()
		static := m.static
		m.mu.RUnlock()
		static.ServeHTTP(w, r)
	})
}

// Browser mutations must carry the same-origin marker sent by app.js. A
// cross-site page cannot set this header without an explicit CORS preflight,
// which this server does not grant. The native desktop helper uses only the
// three capability routes and is checked by rain_assist.go instead.
func multiMutationAllowed(w http.ResponseWriter, r *http.Request) bool {
	if _, native := nativeAssistID(r); native {
		return true
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !strings.EqualFold(u.Host, r.Host) {
			fail(w, http.StatusForbidden, "请求来源不匹配")
			return false
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" || r.Header.Get("X-DueRadar") != "1" {
		fail(w, http.StatusForbidden, "请求来源不匹配")
		return false
	}
	return true
}

func setMultiSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Cache-Control", "no-store")
}

func (m *MultiServer) dispatchAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/register":
		m.register(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/api/login":
		m.login(w, r)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/api/session":
		m.sessionStatus(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/api/logout":
		m.logout(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/api/account/password":
		m.changePassword(w, r)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/api/account/sessions":
		m.sessionList(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/api/account/sessions/revoke":
		m.revokeSession(w, r, false)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/api/account/sessions/revoke-others":
		m.revokeSession(w, r, true)
		return
	case r.URL.Path == "/api/":
		fail(w, http.StatusNotFound, "接口不存在")
		return
	}

	if id, native := nativeAssistID(r); native {
		if userID, ok := m.findAssistOwner(id); ok {
			m.routeTenant(w, r, userID, false)
			return
		}
		// Browser status/cancel requests are intentionally not native and must
		// still use the user's authenticated session below.
	}
	userID, ok := m.authenticatedUser(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "请先登录 DueRadar 账号")
		return
	}
	// Pin every authenticated browser request to the account whose session was
	// read at the beginning of a refresh. A cookie changed in another tab must
	// not turn an old account's pending request into a new account's request.
	if expected := r.Header.Get("X-DueRadar-Account"); expected != "" && expected != userID {
		fail(w, http.StatusConflict, "登录账号已切换，请重新加载")
		return
	}
	m.routeTenant(w, r, userID, true)
}

func nativeAssistID(r *http.Request) (string, bool) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 6 || parts[0] != "api" || parts[1] != "accounts" || parts[2] != "rain" || parts[3] != "assist" || parts[4] == "" {
		return "", false
	}
	if r.Method == http.MethodGet && parts[5] == "helper" || r.Method == http.MethodPost && (parts[5] == "connect" || parts[5] == "session") {
		return parts[4], true
	}
	return "", false
}

func (m *MultiServer) findAssistOwner(id string) (string, bool) {
	m.mu.RLock()
	workers := make(map[string]*Server, len(m.tenants))
	for userID, worker := range m.tenants {
		workers[userID] = worker
	}
	m.mu.RUnlock()
	// Do not hold the registry lock while acquiring a worker's Rain lock.
	for userID, worker := range workers {
		if worker.HandlesRainAssist(id) {
			return userID, true
		}
	}
	return "", false
}

func (m *MultiServer) authenticatedUser(r *http.Request) (string, bool) {
	cookie, err := r.Cookie("dueradar_session")
	if err != nil || cookie.Value == "" {
		return "", false
	}
	key := multiSessionKey(cookie.Value)
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[key]
	if ok && !session.Expires.After(time.Now()) {
		delete(m.sessions, key)
		_ = m.saveSessionsLocked()
		ok = false
	}
	if !ok {
		return "", false
	}
	if _, exists := m.users[session.UserID]; !exists {
		return "", false
	}
	return session.UserID, true
}

func (m *MultiServer) routeTenant(w http.ResponseWriter, r *http.Request, userID string, trusted bool) {
	m.mu.Lock()
	handler, ok := m.tenantHandlerLocked(userID)
	m.mu.Unlock()
	if !ok {
		fail(w, http.StatusInternalServerError, "账号数据不可用")
		return
	}
	if trusted {
		r = r.WithContext(context.WithValue(r.Context(), tenantAuthorizedKey{}, &tenantAuthorization{server: m, userID: userID}))
	}
	handler.ServeHTTP(w, r)
}

type tenantAuthorizedKey struct{}

type tenantAuthorization struct {
	server *MultiServer
	userID string
}

func (a *tenantAuthorization) sessionValid(token string) bool {
	a.server.mu.RLock()
	defer a.server.mu.RUnlock()
	session, ok := a.server.sessions[multiSessionKey(token)]
	return ok && session.UserID == a.userID && session.Expires.After(time.Now())
}

func (m *MultiServer) sessionStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := m.authenticatedUser(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "请先登录 DueRadar 账号")
		return
	}
	m.mu.RLock()
	user := m.users[userID]
	worker := m.tenants[userID]
	m.mu.RUnlock()
	setup := map[string]bool{"platforms_bound": false, "bark_configured": false}
	if worker != nil {
		state := worker.Engine.Snapshot()
		setup["platforms_bound"] = hasBoundPlatform(state)
		setup["bark_configured"] = worker.Vault.Get().BarkKey != ""
	}
	setup["required"] = !setup["platforms_bound"] || !setup["bark_configured"]
	jsonResponse(w, http.StatusOK, map[string]any{"authenticated": true, "user": map[string]string{"id": user.ID, "username": user.Username}, "setup": setup})
}

func hasBoundPlatform(state core.State) bool {
	for _, account := range state.Accounts {
		if account.LoginState == "logged_in" {
			return true
		}
	}
	return false
}

func (m *MultiServer) acquirePasswordSlot(w http.ResponseWriter) bool {
	select {
	case m.passwordSlots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "2")
		fail(w, http.StatusTooManyRequests, "认证服务繁忙，请稍后重试")
		return false
	}
}

func (m *MultiServer) register(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	registrationAllowed := m.allowRegistration || len(m.users) == 0
	m.mu.RUnlock()
	if !registrationAllowed {
		fail(w, http.StatusForbidden, "注册功能已关闭")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	username, err := validateUsername(input.Username)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(input.Password) < 12 || len(input.Password) > maxPasswordBytes {
		fail(w, http.StatusBadRequest, "密码长度需要为 12—1024 字节")
		return
	}
	ip := clientIP(r)
	now := time.Now()
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		fail(w, http.StatusServiceUnavailable, "服务正在停止")
		return
	}
	if !m.allowSignupLocked(ip, now) {
		m.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "注册请求过多，请稍后重试")
		return
	}
	m.mu.Unlock()

	if !m.acquirePasswordSlot(w) {
		return
	}
	id, idErr := randomID()
	hash, hashErr := hashPassword(input.Password)
	<-m.passwordSlots
	if idErr != nil || hashErr != nil {
		fail(w, http.StatusInternalServerError, "账号创建失败")
		return
	}
	user := multiUser{ID: id, Username: username, PasswordHash: hash, CreatedAt: now.UTC()}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		fail(w, http.StatusServiceUnavailable, "服务正在停止")
		return
	}
	if !m.allowRegistration && len(m.users) != 0 {
		m.mu.Unlock()
		fail(w, http.StatusForbidden, "注册功能已关闭")
		return
	}
	if _, exists := m.byName[username]; exists {
		m.mu.Unlock()
		fail(w, http.StatusConflict, "该账号已存在")
		return
	}
	if len(m.users) >= maxUsers {
		m.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "账号数量已达到上限")
		return
	}
	// Initialize the private state and vault before publishing the account in
	// users.json. A crash can leave an unreferenced directory, not a registered
	// account whose missing files would be silently recreated on restart.
	if err := m.openTenantLocked(user, true); err != nil {
		m.mu.Unlock()
		fail(w, http.StatusInternalServerError, "账号数据初始化失败")
		return
	}
	m.users[id], m.byName[username] = user, id
	if err := m.saveUsersLocked(); err != nil {
		delete(m.users, id)
		delete(m.byName, username)
		delete(m.tenants, id)
		m.mu.Unlock()
		fail(w, http.StatusInternalServerError, "账号创建失败，请检查数据目录")
		return
	}
	token, tokenErr := m.createSessionLocked(id, now)
	// A committed account must receive background processing even when the
	// first login session cannot be persisted; its owner can sign in later.
	_ = m.startTenantLocked(id)
	m.mu.Unlock()
	if tokenErr != nil {
		fail(w, http.StatusInternalServerError, "会话创建失败")
		return
	}
	setMultiSessionCookie(w, token, m.secureCookies)
	jsonResponse(w, http.StatusCreated, map[string]any{"authenticated": true, "user": map[string]string{"id": id, "username": username}})
}

func (m *MultiServer) allowSignupLocked(ip string, now time.Time) bool {
	for key, attempt := range m.signups {
		if now.Sub(attempt.Since) >= time.Hour {
			delete(m.signups, key)
		}
	}
	attempt := m.signups[ip]
	if attempt.Count >= 5 {
		return false
	}
	if attempt.Since.IsZero() {
		attempt.Since = now
	}
	attempt.Count++
	m.signups[ip] = attempt
	return true
}

func (m *MultiServer) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.Password) > maxPasswordBytes {
		fail(w, http.StatusUnauthorized, "账号或密码不正确")
		return
	}
	ip := clientIP(r)
	m.mu.Lock()
	allowed := m.allowLoginAttemptLocked(ip, time.Now())
	m.mu.Unlock()
	if !allowed {
		w.Header().Set("Retry-After", "2")
		fail(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后重试")
		return
	}
	username := normalizeUsername(input.Username)
	m.mu.RLock()
	user, exists := m.users[m.byName[username]]
	encoded := m.dummyPasswordHash
	if exists {
		encoded = user.PasswordHash
	}
	m.mu.RUnlock()
	if !m.acquirePasswordSlot(w) {
		return
	}
	valid := verifyPassword(encoded, input.Password) && exists
	<-m.passwordSlots
	if !valid {
		now := time.Now()
		m.mu.Lock()
		limited := !m.recordLoginFailureLocked(ip, now)
		m.mu.Unlock()
		if limited {
			w.Header().Set("Retry-After", "2")
			fail(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后重试")
		} else {
			fail(w, http.StatusUnauthorized, "账号或密码不正确")
		}
		return
	}
	now := time.Now()
	m.mu.Lock()
	delete(m.attempts, ip)
	token, err := m.createSessionLocked(user.ID, now)
	m.mu.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, "会话创建失败")
		return
	}
	setMultiSessionCookie(w, token, m.secureCookies)
	jsonResponse(w, http.StatusOK, map[string]any{"authenticated": true, "user": map[string]string{"id": user.ID, "username": user.Username}})
}

func (m *MultiServer) allowLoginAttemptLocked(ip string, now time.Time) bool {
	for key, attempt := range m.attempts {
		if now.Sub(attempt.Since) >= 15*time.Minute {
			delete(m.attempts, key)
		}
	}
	attempt, exists := m.attempts[ip]
	if !exists || attempt.Count < 10 {
		return true
	}
	if now.Before(attempt.NextAllowed) {
		return false
	}
	// Reserve the next verification slot before releasing the lock, so a
	// concurrent burst from the same address cannot bypass the interval.
	attempt.NextAllowed = now.Add(loginRetryInterval)
	m.attempts[ip] = attempt
	return true
}

func (m *MultiServer) recordLoginFailureLocked(ip string, now time.Time) bool {
	attempt, exists := m.attempts[ip]
	if !exists && len(m.attempts) >= 2048 {
		return false
	}
	if attempt.Since.IsZero() || now.Sub(attempt.Since) >= 15*time.Minute {
		attempt = multiAttempt{Since: now}
	}
	attempt.Count++
	if attempt.Count >= 10 {
		next := now.Add(loginRetryInterval)
		if next.After(attempt.NextAllowed) {
			attempt.NextAllowed = next
		}
	}
	m.attempts[ip] = attempt
	return true
}

func (m *MultiServer) createSessionLocked(userID string, now time.Time) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	id, err := randomToken(8)
	if err != nil {
		return "", err
	}
	for key, session := range m.sessions {
		if !session.Expires.After(now) {
			delete(m.sessions, key)
		}
	}
	if len(m.sessions) >= 512 {
		oldestKey := ""
		var oldest time.Time
		for key, session := range m.sessions {
			if oldestKey == "" || session.Expires.Before(oldest) {
				oldestKey, oldest = key, session.Expires
			}
		}
		delete(m.sessions, oldestKey)
	}
	m.sessions[multiSessionKey(token)] = multiSession{ID: id, UserID: userID, Expires: now.Add(multiSessionLifetime), CreatedAt: now.UTC()}
	if err := m.saveSessionsLocked(); err != nil {
		delete(m.sessions, multiSessionKey(token))
		return "", err
	}
	return token, nil
}

func (m *MultiServer) logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("dueradar_session")
	if err == nil && cookie.Value != "" {
		key := multiSessionKey(cookie.Value)
		m.mu.Lock()
		session, exists := m.sessions[key]
		if expected := r.Header.Get("X-DueRadar-Account"); expected != "" && exists && session.UserID != expected {
			m.mu.Unlock()
			fail(w, http.StatusConflict, "登录账号已切换，请重新加载")
			return
		}
		delete(m.sessions, key)
		persistErr := m.saveSessionsLocked()
		worker := m.tenants[session.UserID]
		m.mu.Unlock()
		if exists && worker != nil {
			worker.CancelRainAssistForSession(cookie.Value)
		}
		if persistErr != nil {
			// Do not report a durable logout when a restart could reload the old
			// token. Clear the local cookie and make the persistence failure visible.
			setMultiSessionCookie(w, "", m.secureCookies)
			fail(w, http.StatusInternalServerError, "退出保存失败，请检查数据目录并重试")
			return
		}
	}
	setMultiSessionCookie(w, "", m.secureCookies)
	jsonResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (m *MultiServer) changePassword(w http.ResponseWriter, r *http.Request) {
	userID, ok := m.authenticatedUser(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "请先登录 DueRadar 账号")
		return
	}
	var input struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.New) < 12 || len(input.New) > maxPasswordBytes {
		fail(w, http.StatusBadRequest, "新密码长度需要为 12—1024 字节")
		return
	}
	if input.New == input.Current {
		fail(w, http.StatusBadRequest, "新密码不能与当前密码相同")
		return
	}
	ip := clientIP(r)
	m.mu.Lock()
	allowed := m.allowLoginAttemptLocked(ip, time.Now())
	m.mu.Unlock()
	if !allowed {
		w.Header().Set("Retry-After", "2")
		fail(w, http.StatusTooManyRequests, "密码尝试过于频繁，请稍后重试")
		return
	}
	m.mu.RLock()
	user, exists := m.users[userID]
	m.mu.RUnlock()
	if !exists {
		fail(w, http.StatusUnauthorized, "账号不存在，请重新登录")
		return
	}
	if !m.acquirePasswordSlot(w) {
		return
	}
	verified := user.PasswordHash
	valid := verifyPassword(verified, input.Current)
	<-m.passwordSlots
	if !valid {
		m.mu.Lock()
		limited := !m.recordLoginFailureLocked(ip, time.Now())
		m.mu.Unlock()
		if limited {
			w.Header().Set("Retry-After", "2")
			fail(w, http.StatusTooManyRequests, "密码尝试过于频繁，请稍后重试")
		} else {
			fail(w, http.StatusForbidden, "当前密码不正确")
		}
		return
	}
	if !m.acquirePasswordSlot(w) {
		return
	}
	hash, hashErr := hashPassword(input.New)
	<-m.passwordSlots
	if hashErr != nil {
		fail(w, http.StatusInternalServerError, "密码修改失败")
		return
	}
	now := time.Now()
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		fail(w, http.StatusServiceUnavailable, "服务正在停止")
		return
	}
	user, exists = m.users[userID]
	if !exists {
		m.mu.Unlock()
		fail(w, http.StatusUnauthorized, "账号不存在，请重新登录")
		return
	}
	if user.PasswordHash != verified {
		m.mu.Unlock()
		fail(w, http.StatusConflict, "密码已在其他会话修改，请重新登录")
		return
	}
	previous := user.PasswordHash
	user.PasswordHash = hash
	m.users[userID] = user
	if err := m.saveUsersLocked(); err != nil {
		user.PasswordHash = previous
		m.users[userID] = user
		m.mu.Unlock()
		fail(w, http.StatusInternalServerError, "密码保存失败，请检查数据目录")
		return
	}
	delete(m.attempts, ip)
	// A password change invalidates every other login. Issue a fresh session for
	// the browser that made the change so the user is not signed out here.
	for key, session := range m.sessions {
		if session.UserID == userID {
			delete(m.sessions, key)
		}
	}
	token, sessionErr := m.createSessionLocked(userID, now)
	m.mu.Unlock()
	if sessionErr != nil {
		fail(w, http.StatusInternalServerError, "密码已修改，请重新登录")
		return
	}
	setMultiSessionCookie(w, token, m.secureCookies)
	jsonResponse(w, http.StatusOK, map[string]string{"message": "密码已修改，其他设备已退出登录。"})
}

func (m *MultiServer) sessionList(w http.ResponseWriter, r *http.Request) {
	userID, ok := m.authenticatedUser(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "请先登录 DueRadar 账号")
		return
	}
	current := ""
	if cookie, err := r.Cookie("dueradar_session"); err == nil {
		current = multiSessionKey(cookie.Value)
	}
	now := time.Now()
	type view struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
		Current   bool      `json:"current"`
	}
	m.mu.Lock()
	views := []view{}
	for key, session := range m.sessions {
		if !session.Expires.After(now) {
			delete(m.sessions, key)
			continue
		}
		if session.UserID != userID {
			continue
		}
		views = append(views, view{ID: session.ID, CreatedAt: session.CreatedAt, ExpiresAt: session.Expires, Current: key == current})
	}
	m.mu.Unlock()
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].ExpiresAt.After(views[j].ExpiresAt)
		}
		return views[i].CreatedAt.After(views[j].CreatedAt)
	})
	jsonResponse(w, http.StatusOK, views)
}

// revokeSession removes one session ID, or every other session when all is
// true. The browser making the request keeps its own login.
func (m *MultiServer) revokeSession(w http.ResponseWriter, r *http.Request, all bool) {
	userID, ok := m.authenticatedUser(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "请先登录 DueRadar 账号")
		return
	}
	var input struct {
		ID string `json:"id"`
	}
	if !all && !decode(w, r, &input) {
		return
	}
	current := ""
	if cookie, err := r.Cookie("dueradar_session"); err == nil {
		current = multiSessionKey(cookie.Value)
	}
	m.mu.Lock()
	removed := 0
	for key, session := range m.sessions {
		if session.UserID != userID || key == current {
			continue
		}
		if all || session.ID == input.ID {
			delete(m.sessions, key)
			removed++
		}
	}
	persistErr := m.saveSessionsLocked()
	m.mu.Unlock()
	if persistErr != nil {
		fail(w, http.StatusInternalServerError, "会话保存失败，请检查数据目录")
		return
	}
	if !all && removed == 0 {
		fail(w, http.StatusNotFound, "会话不存在或已退出")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"removed": removed})
}

func setMultiSessionCookie(w http.ResponseWriter, token string, secure bool) {
	maxAge := int(multiSessionLifetime / time.Second)
	if token == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: "dueradar_session", Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func (m *MultiServer) Run(ctx context.Context) {
	m.mu.Lock()
	if m.runStarted || m.stopping {
		m.mu.Unlock()
		return
	}
	m.runStarted = true
	for id := range m.tenants {
		_ = m.startTenantLocked(id)
	}
	m.mu.Unlock()
	stop := context.AfterFunc(ctx, m.stopBackground)
	defer stop()
	<-m.lifetime.Done()
	m.jobs.Wait()
}

// All additions happen while holding m.mu and before stopping is set. Once
// stopBackground releases that lock, Shutdown may safely wait for all workers.
func (m *MultiServer) startTenantLocked(userID string) error {
	worker := m.tenants[userID]
	if worker == nil {
		return errors.New("account worker is unavailable")
	}
	if m.stopping {
		return errors.New("service is stopping")
	}
	if !m.runStarted {
		return nil
	}
	m.jobs.Add(1)
	go func() { defer m.jobs.Done(); worker.Run(m.lifetime) }()
	return nil
}

func (m *MultiServer) stopBackground() {
	m.mu.Lock()
	if !m.stopping {
		m.stopping = true
		m.lifetimeCancel()
	}
	workers := make([]*Server, 0, len(m.tenants))
	for _, worker := range m.tenants {
		workers = append(workers, worker)
	}
	m.mu.Unlock()
	for _, worker := range workers {
		worker.stopBackground()
	}
}

func (m *MultiServer) Shutdown(ctx context.Context) error {
	m.stopBackground()
	m.mu.RLock()
	workers := make([]*Server, 0, len(m.tenants))
	for _, worker := range m.tenants {
		workers = append(workers, worker)
	}
	m.mu.RUnlock()
	done := make(chan struct{})
	go func() {
		m.jobs.Wait()
		// A worker may own jobs even if Run never started or stopped before
		// its goroutine was scheduled. Wait for those jobs as well.
		for _, worker := range workers {
			_ = worker.Shutdown(context.Background())
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *MultiServer) UserCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users)
}

func normalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func validateUsername(value string) (string, error) {
	value = normalizeUsername(value)
	if !utf8.ValidString(value) {
		return "", errors.New("账号格式无效")
	}
	runes := []rune(value)
	if len(runes) < 3 || len(runes) > 32 {
		return "", errors.New("账号长度需要为 3—32 个字符")
	}
	for _, r := range runes {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			continue
		}
		return "", errors.New("账号只能包含字母、数字、下划线、短横线或点号")
	}
	return value, nil
}

func validUserID(value string) bool {
	if value == "owner" {
		return true
	}
	if len(value) < 8 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'f') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func randomID() (string, error) {
	return randomToken(16)
}

func randomToken(bytesLength int) (string, error) {
	value := make([]byte, bytesLength)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func multiSessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordIterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(digest)), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	var iterations int
	if _, err := fmt.Sscanf(parts[1], "%d", &iterations); err != nil || iterations < 100000 || iterations > 1000000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(salt) < 16 || len(want) != 32 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	return err == nil && hmac.Equal(got, want)
}
