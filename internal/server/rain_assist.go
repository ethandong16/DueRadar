package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

const rainAssistLifetime = 5 * time.Minute

// All fields, including terminal flows, are protected by rainMu. Secrets are
// returned only once; their hashes are retained solely for capability checks.
type rainAssist struct {
	id                     string
	state, message         string
	expires                time.Time
	generation             uint64
	owner, pairing, upload [32]byte
	connected, uploaded    bool
	ctx                    context.Context
	cancel                 context.CancelFunc
}

func (f *rainAssist) pending() bool {
	return f.state == "awaiting_helper" || f.state == "awaiting_user" || f.state == "verifying"
}

func (f *rainAssist) status() map[string]any {
	return map[string]any{"state": f.state, "error": f.message, "expires_at": f.expires}
}

func rainCapability() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func emptyAssistBody(w http.ResponseWriter, r *http.Request) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(data) != 0 {
		fail(w, 400, "此请求无需正文")
		return false
	}
	return true
}

// Helper capabilities are native-client credentials, never browser credentials.
func nativeAssistRequest(w http.ResponseWriter, r *http.Request) bool {
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		if _, present := r.Header[header]; present {
			fail(w, 403, "请使用桌面登录助手")
			return false
		}
	}
	return true
}

func matchingAssistCapability(r *http.Request, want [32]byte) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, secret, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || len(secret) != 64 {
		return false
	}
	got := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func (s *Server) startRainAssist(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "请在正式实例绑定账号")
		return
	}
	if !emptyAssistBody(w, r) {
		return
	}
	id, err := rainCapability()
	if err != nil {
		fail(w, 500, "配对创建失败")
		return
	}
	code, err := rainCapability()
	if err != nil {
		fail(w, 500, "配对创建失败")
		return
	}
	admin, err := r.Cookie("dueradar_session")
	if err != nil {
		fail(w, 401, "请先登录管理页面")
		return
	}
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	if s.lifetime.Err() != nil {
		fail(w, 503, "服务正在停止")
		return
	}
	// Close the protected-handler/logout race before associating the flow.
	s.mu.Lock()
	owner, exists := s.sessions[admin.Value]
	s.mu.Unlock()
	if !exists || !owner.expires.After(time.Now()) {
		fail(w, 401, "管理会话已过期，请重新登录")
		return
	}
	now := time.Now()
	for key, flow := range s.rainAssists {
		if !flow.expires.After(now) {
			s.expireRainAssistLocked(flow, now)
			delete(s.rainAssists, key)
		}
	}
	if len(s.rainAssists) >= 128 {
		fail(w, 429, "配对次数过多，请稍后重试")
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
	s.supersedeRainAssistLocked()
	if s.rainSyncCancel != nil {
		s.rainSyncCancel()
		s.rainSyncCancel = nil
	}
	s.rainGeneration++
	expires := now.Add(rainAssistLifetime)
	ctx, cancel := context.WithDeadline(s.lifetime, expires)
	flow := &rainAssist{id: id, state: "awaiting_helper", expires: expires, generation: s.rainGeneration,
		owner: sha256.Sum256([]byte(admin.Value)), pairing: sha256.Sum256([]byte(code)), ctx: ctx, cancel: cancel}
	if s.rainAssists == nil {
		s.rainAssists = make(map[string]*rainAssist)
	}
	s.rainAssists[id] = flow
	s.rainAssistActive, s.rainCancel = flow, cancel
	s.rainState, s.rainError = "waiting", ""
	if err := s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "waiting"}); err != nil {
		s.finishRainAssistLocked(flow, "failed", "登录状态保存失败")
		fail(w, 500, "登录状态保存失败")
		return
	}
	if !s.startJob(func(context.Context) {
		<-ctx.Done()
		s.rainMu.Lock()
		defer s.rainMu.Unlock()
		if s.rainAssistActive != flow || !flow.pending() {
			return
		}
		state := "canceled"
		if !flow.expires.After(time.Now()) {
			state = "expired"
		}
		s.finishRainAssistLocked(flow, state, "")
	}) {
		s.finishRainAssistLocked(flow, "canceled", "")
		fail(w, 503, "服务正在停止")
		return
	}
	jsonResponse(w, 200, map[string]any{"flow_id": id, "pairing_code": code, "expires_at": expires, "expires_in": 300})
}

// Superseding a flow does not restore the old account between generations.
func (s *Server) supersedeRainAssistLocked() {
	if flow := s.rainAssistActive; flow != nil {
		flow.state, flow.message = "canceled", ""
		flow.pairing, flow.upload = [32]byte{}, [32]byte{}
		flow.cancel()
		s.rainAssistActive = nil
	}
}

// finishRainAssistLocked restores the previous account on failure/cancellation.
// A separate sync must revalidate it before any notification is eligible again.
func (s *Server) finishRainAssistLocked(flow *rainAssist, state, message string) {
	flow.state, flow.message = state, message
	flow.pairing = [32]byte{}
	flow.cancel()
	if state == "canceled" || state == "expired" {
		flow.upload = [32]byte{}
	}
	if s.rainAssistActive != flow || flow.generation != s.rainGeneration {
		return
	}
	s.rainAssistActive, s.rainCancel = nil, nil
	s.rainGeneration++
	s.rainState, s.rainError = state, message
	previous := s.rainPreviousLogin
	if previous == "" || previous == "waiting" {
		previous = "unbound"
	}
	if err := s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: previous}); err != nil {
		flow.message, s.rainError = "登录状态恢复失败", "登录状态恢复失败"
	}
	s.queueSync()
}

func (s *Server) expireRainAssistLocked(flow *rainAssist, now time.Time) {
	if flow != nil && flow.pending() && !flow.expires.After(now) {
		s.finishRainAssistLocked(flow, "expired", "")
	}
}

func (s *Server) rainAssistStatus(w http.ResponseWriter, r *http.Request) {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	flow := s.rainAssists[r.PathValue("id")]
	if flow == nil {
		fail(w, 404, "配对不存在")
		return
	}
	s.expireRainAssistLocked(flow, time.Now())
	jsonResponse(w, 200, flow.status())
}

func (s *Server) cancelRainAssist(w http.ResponseWriter, r *http.Request) {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	flow := s.rainAssists[r.PathValue("id")]
	if flow == nil {
		fail(w, 404, "配对不存在")
		return
	}
	s.expireRainAssistLocked(flow, time.Now())
	if s.rainAssistActive == flow && flow.pending() {
		s.finishRainAssistLocked(flow, "canceled", "")
	}
	jsonResponse(w, 200, map[string]bool{"ok": true})
}

func (s *Server) cancelRainAssistOwner(token string) {
	owner := sha256.Sum256([]byte(token))
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	if flow := s.rainAssistActive; flow != nil && flow.owner == owner {
		s.finishRainAssistLocked(flow, "canceled", "")
	}
}

func (s *Server) connectRainAssist(w http.ResponseWriter, r *http.Request) {
	if !nativeAssistRequest(w, r) {
		return
	}
	s.rainMu.Lock()
	flow := s.rainAssists[r.PathValue("id")]
	s.expireRainAssistLocked(flow, time.Now())
	if flow == nil || s.lifetime.Err() != nil || flow.state != "awaiting_helper" || flow.connected || !matchingAssistCapability(r, flow.pairing) {
		s.rainMu.Unlock()
		fail(w, 401, "配对凭证无效或已失效")
		return
	}
	s.rainMu.Unlock()
	if !emptyAssistBody(w, r) {
		return
	}
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	s.expireRainAssistLocked(flow, time.Now())
	if s.lifetime.Err() != nil || flow.state != "awaiting_helper" || flow.connected || !matchingAssistCapability(r, flow.pairing) {
		fail(w, 401, "配对凭证无效或已失效")
		return
	}
	secret, err := rainCapability()
	if err != nil {
		fail(w, 500, "配对创建失败")
		return
	}
	flow.pairing = [32]byte{}
	flow.upload = sha256.Sum256([]byte(secret))
	flow.connected, flow.state = true, "awaiting_user"
	jsonResponse(w, 200, map[string]any{"upload_token": secret, "expires_at": flow.expires, "state": flow.state})
}

func (s *Server) helperRainAssistLocked(w http.ResponseWriter, r *http.Request) *rainAssist {
	flow := s.rainAssists[r.PathValue("id")]
	s.expireRainAssistLocked(flow, time.Now())
	if flow == nil || s.lifetime.Err() != nil || !flow.expires.After(time.Now()) || flow.state == "canceled" || flow.state == "expired" || !flow.connected || !matchingAssistCapability(r, flow.upload) {
		fail(w, 401, "配对凭证无效或已失效")
		return nil
	}
	return flow
}

func (s *Server) rainAssistHelperStatus(w http.ResponseWriter, r *http.Request) {
	if !nativeAssistRequest(w, r) {
		return
	}
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	if flow := s.helperRainAssistLocked(w, r); flow != nil {
		jsonResponse(w, 200, flow.status())
	}
}

func (s *Server) uploadRainAssist(w http.ResponseWriter, r *http.Request) {
	if !nativeAssistRequest(w, r) {
		return
	}
	s.rainMu.Lock()
	flow := s.helperRainAssistLocked(w, r)
	if flow == nil {
		s.rainMu.Unlock()
		return
	}
	if flow.uploaded || flow.state != "awaiting_user" {
		s.rainMu.Unlock()
		fail(w, 409, "此配对已提交会话，请重新发起配对")
		return
	}
	// Consume before parsing, including invalid/oversized uploads. Never hold the
	// provider lock while reading a potentially slow request body.
	flow.uploaded, flow.state = true, "verifying"
	s.rainMu.Unlock()
	media, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	data, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	cookie, importErr := platform.ImportRainBrowserSession(data)
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	s.expireRainAssistLocked(flow, time.Now())
	if s.lifetime.Err() != nil || s.rainAssistActive != flow || flow.generation != s.rainGeneration || flow.ctx.Err() != nil {
		fail(w, 409, "配对已结束，请重新发起")
		return
	}
	if mediaErr != nil || media != "application/json" || readErr != nil || importErr != nil {
		s.finishRainAssistLocked(flow, "failed", "浏览器会话格式无效，请重新配对")
		fail(w, 400, "浏览器会话格式无效，请重新配对")
		return
	}
	if !s.startJob(func(context.Context) { s.validateRainAssist(flow, cookie) }) {
		s.finishRainAssistLocked(flow, "canceled", "")
		fail(w, 503, "服务正在停止")
		return
	}
	jsonResponse(w, 202, map[string]string{"state": "verifying"})
}

func (s *Server) validateRainAssist(flow *rainAssist, cookie string) {
	ctx, cancel := context.WithTimeout(flow.ctx, 30*time.Second)
	defer cancel()
	adapter := s.newRain(cookie)
	err := adapter.Validate(ctx)
	if err == nil {
		cookie, err = sessionCredentials(adapter, cookie)
	}
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	s.expireRainAssistLocked(flow, time.Now())
	if s.rainAssistActive != flow || flow.generation != s.rainGeneration || s.lifetime.Err() != nil || flow.ctx.Err() != nil {
		return
	}
	if err != nil || ctx.Err() != nil {
		s.finishRainAssistLocked(flow, "failed", "官方会话验证未完成，请重新配对")
		return
	}
	if err := s.Vault.Update(func(c *vault.Config) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.RainCookie = cookie
		return nil
	}); err != nil {
		s.finishRainAssistLocked(flow, "failed", "会话保存失败，请重试")
		return
	}
	if err := s.Engine.SetAccount(core.Account{Platform: core.PlatformRain, LoginState: "logged_in"}); err != nil {
		// Validated credentials have already been persisted. Preserve them rather
		// than restoring an obsolete session; never claim login-state persistence.
		flow.state, flow.message = "failed", "登录状态保存失败，请检查存储后重新同步"
		s.rainState, s.rainError = "failed", flow.message
		s.rainAssistActive, s.rainCancel = nil, nil
		flow.cancel()
		return
	}
	flow.state, flow.message = "logged_in", ""
	flow.pairing = [32]byte{}
	s.rainState, s.rainError, s.rainPreviousLogin = "logged_in", "", "logged_in"
	s.rainAssistActive, s.rainCancel = nil, nil
	flow.cancel()
	s.queueSync()
}
