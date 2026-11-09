package server

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

const rainFallbackLead = 10 * time.Second

func defaultOpenClassroom(ctx context.Context, adapter rainAdapter, lesson platform.LiveLesson) (classroomChannel, error) {
	opener, ok := adapter.(interface {
		OpenClassroom(context.Context, platform.LiveLesson) (*platform.LiveClassroom, error)
	})
	if !ok {
		return nil, errors.New("课堂实时接口不受支持")
	}
	return opener.OpenClassroom(ctx, lesson)
}

type classroomChannel interface {
	Run(context.Context, func(platform.ClassroomProblem)) error
	CurrentProblem(context.Context, string) (platform.ClassroomProblem, error)
	SubmitChoice(context.Context, platform.ClassroomProblem, string) (string, error)
	Close()
}

type classroomWatch struct {
	generation, epoch uint64
	cookie            string
	lesson            platform.LiveLesson
	ctx               context.Context
	cancel            context.CancelFunc
	adapter           rainAdapter
	channel           classroomChannel
	renewMu           sync.Mutex // Serializes credential exports from concurrent problem jobs.
	// Mutable watch state, including the queues, is protected by rainMu.
	problems  map[string]platform.ClassroomProblem
	fallbacks map[string]bool
	changed   chan struct{}
}

type classroomStatus struct {
	Connected  int    `json:"connected"`
	LastAction string `json:"last_action"`
	Error      string `json:"error"`
}

// invalidateClassroomsLocked is called before replacing an identity or changing
// assistant settings. A disable/re-enable must never revive an old timer/AI job.
func (s *Server) invalidateClassroomsLocked() {
	s.classroomEpoch++
	for _, watch := range s.classrooms {
		watch.cancel()
	}
	s.classrooms = make(map[string]*classroomWatch)
	s.classroomStatus = classroomStatus{}
}

func (s *Server) classroomCurrentLocked(w *classroomWatch) bool {
	settings := s.Engine.Snapshot().Settings
	return w.ctx.Err() == nil && s.lifetime.Err() == nil && w.generation == s.rainGeneration && w.epoch == s.classroomEpoch &&
		s.rainState != "initializing" && s.rainState != "waiting" && s.Vault.Get().RainCookie == w.cookie &&
		(settings.InClassEnabled || settings.RainFallbackEnabled || settings.RainAnalysisEnabled)
}

func (s *Server) classroomCurrent(w *classroomWatch) bool {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	return s.classroomCurrentLocked(w)
}

// Discovery is separate from both notifications and the 15-minute task sync.
// Once connected, websocket pushes and per-problem timers do not wait for polls.
func (s *Server) runClassrooms(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.checkClassrooms(ctx)
		interval := s.inClassLoopInterval()
		if interval > 20*time.Second {
			interval = 20 * time.Second
		}
		timer.Reset(interval)
	}
}

func (s *Server) checkClassrooms(ctx context.Context) {
	s.rainMu.Lock()
	settings := s.Engine.Snapshot().Settings
	generation, epoch, cookie := s.rainGeneration, s.classroomEpoch, s.Vault.Get().RainCookie
	if s.Demo || ctx.Err() != nil || s.lifetime.Err() != nil || (!settings.InClassEnabled && !settings.RainFallbackEnabled && !settings.RainAnalysisEnabled) || cookie == "" || s.rainState == "waiting" || s.rainState == "initializing" {
		s.invalidateClassroomsLocked()
		s.rainMu.Unlock()
		return
	}
	// Cookie renewal may supersede a connection without changing the identity.
	for id, w := range s.classrooms {
		if !s.classroomCurrentLocked(w) {
			w.cancel()
			delete(s.classrooms, id)
		}
	}
	s.rainMu.Unlock()
	poll, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	adapter := s.newRain(cookie)
	live, ok := adapter.(inClassAdapter)
	if !ok {
		return
	}
	lessons, err := live.LiveLessons(poll)
	updated, exportErr := sessionCredentials(adapter, cookie)
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	if !s.inClassBindingCurrentLocked(poll, generation, cookie) || epoch != s.classroomEpoch {
		return
	}
	if exportErr == nil && updated != cookie {
		exportErr = s.Vault.Update(func(c *vault.Config) error {
			if c.RainCookie != cookie {
				return errSupersededLogin
			}
			c.RainCookie = updated
			return nil
		})
		if exportErr == nil {
			cookie = updated
			if s.rainSyncCancel != nil {
				s.queueSync()
			}
		}
	}
	if exportErr != nil {
		s.classroomStatus.Error = "课堂会话续期保存失败"
		return
	}
	if err != nil {
		s.classroomStatus.Error = "课堂实时入口读取失败"
		return
	}
	s.classroomStatus.Error = ""
	present := map[string]bool{}
	for _, lesson := range lessons {
		if lesson.Role != 5 || lesson.LessonID == "" {
			continue
		}
		present[lesson.LessonID] = true
		if w := s.classrooms[lesson.LessonID]; w != nil {
			if s.classroomCurrentLocked(w) {
				continue
			}
			w.cancel()
			delete(s.classrooms, lesson.LessonID)
		}
		watchCtx, watchCancel := context.WithCancel(ctx)
		w := &classroomWatch{generation: generation, epoch: epoch, cookie: cookie, lesson: lesson, ctx: watchCtx, cancel: watchCancel, problems: map[string]platform.ClassroomProblem{}, fallbacks: map[string]bool{}, changed: make(chan struct{})}
		if s.classrooms == nil {
			s.classrooms = map[string]*classroomWatch{}
		}
		s.classrooms[lesson.LessonID] = w
		if !s.startJob(func(context.Context) { s.watchClassroom(w) }) {
			watchCancel()
			delete(s.classrooms, lesson.LessonID)
		}
	}
	for id, w := range s.classrooms {
		if !present[id] {
			w.cancel()
			delete(s.classrooms, id)
		}
	}
}

func (s *Server) watchClassroom(w *classroomWatch) {
	defer func() {
		w.cancel()
		s.rainMu.Lock()
		defer s.rainMu.Unlock()
		if s.classrooms[w.lesson.LessonID] == w {
			delete(s.classrooms, w.lesson.LessonID)
		}
	}()
	s.rainMu.Lock()
	if !s.classroomCurrentLocked(w) {
		s.rainMu.Unlock()
		return
	}
	cookie := w.cookie
	s.rainMu.Unlock()
	adapter := s.newRain(cookie)
	s.rainMu.Lock()
	w.adapter = adapter
	s.rainMu.Unlock()
	openCtx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	channel, err := s.openClassroom(openCtx, adapter, w.lesson)
	cancel()
	if !s.renewClassroom(w) {
		if channel != nil {
			channel.Close()
		}
		return
	}
	if err != nil {
		s.classroomError(w, "课堂学生实时接口不可用（不会自动签到）")
		return
	}
	s.rainMu.Lock()
	if !s.classroomCurrentLocked(w) {
		s.rainMu.Unlock()
		channel.Close()
		return
	}
	w.channel = channel
	s.rainMu.Unlock()
	defer channel.Close()
	// Even a quiet socket must stop promptly after another worker rotates the
	// credential, direct settings changes, or a binding generation change.
	guardDone := make(chan struct{})
	guardCtx, guardCancel := context.WithCancel(w.ctx)
	defer func() { guardCancel(); <-guardDone }()
	go func() {
		defer close(guardDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-guardCtx.Done():
				return
			case <-ticker.C:
				if !s.classroomCurrent(w) {
					w.cancel()
					channel.Close()
					return
				}
			}
		}
	}()
	err = channel.Run(w.ctx, func(p platform.ClassroomProblem) {
		if s.renewClassroom(w) {
			s.observeClassroomProblem(w, p)
		}
	})
	_ = s.renewClassroom(w)
	if err != nil && w.ctx.Err() == nil {
		s.classroomError(w, "课堂实时连接中断，等待重连")
	}
}

// Export after bootstrap, observations, preflight and submission (including
// errors). CAS prevents a stale worker overwriting a newly bound/renewed session.
func (s *Server) renewClassroom(w *classroomWatch) bool {
	w.renewMu.Lock()
	defer w.renewMu.Unlock()
	s.rainMu.Lock()
	if !s.classroomCurrentLocked(w) || w.adapter == nil {
		s.rainMu.Unlock()
		return false
	}
	adapter, cookie := w.adapter, w.cookie
	s.rainMu.Unlock()
	updated, err := sessionCredentials(adapter, cookie)
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	if !s.classroomCurrentLocked(w) || w.cookie != cookie {
		return false
	}
	if err == nil && updated != cookie {
		err = s.Vault.Update(func(c *vault.Config) error {
			if c.RainCookie != cookie {
				return errSupersededLogin
			}
			c.RainCookie = updated
			return nil
		})
		if err == nil {
			w.cookie = updated
			if s.rainSyncCancel != nil {
				s.queueSync()
			}
		}
	}
	if err != nil {
		s.classroomStatus.Error = "课堂会话续期保存失败"
		w.cancel()
		return false
	}
	return true
}

func (s *Server) classroomError(w *classroomWatch, message string) {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	if s.classroomCurrentLocked(w) {
		s.classroomStatus.Error = message
	}
}

func (s *Server) observeClassroomProblem(w *classroomWatch, p platform.ClassroomProblem) {
	if p.ID == "" || p.LessonID != w.lesson.LessonID {
		return
	}
	s.rainMu.Lock()
	if !s.classroomCurrentLocked(w) {
		s.rainMu.Unlock()
		return
	}
	_, seen := w.problems[p.ID]
	if !seen && len(w.problems) >= 200 {
		s.rainMu.Unlock()
		return
	}
	w.problems[p.ID] = p
	if p.Closed {
		if err := s.Engine.CancelClassroomAnalysis(w.lesson.LessonID, p.ID); err != nil {
			s.classroomStatus.Error = "题目关闭后的分析通知取消失败"
		}
	}
	close(w.changed)
	w.changed = make(chan struct{})
	settings := s.Engine.Snapshot().Settings
	if settings.RainFallbackEnabled && p.Choice() && !w.fallbacks[p.ID] {
		w.fallbacks[p.ID] = true
		s.startJob(func(context.Context) { s.runFallback(w, p.ID) })
	}
	if settings.RainAnalysisEnabled && p.Choice() && !seen && !p.Answered && !p.Closed && !p.StartedAt.IsZero() && time.Since(p.StartedAt) >= -time.Minute && time.Since(p.StartedAt) < 2*time.Minute {
		s.startJob(func(context.Context) { s.runAnalysis(w, p) })
	}
	now := time.Now()
	notify := settings.InClassEnabled && !seen && !p.Closed && !p.StartedAt.IsZero() &&
		!p.StartedAt.Before(now.Add(-inClassNotifyWindow)) && !p.StartedAt.After(now.Add(time.Minute))
	item := core.InClassQuiz{}
	if notify {
		title := strings.TrimSpace(p.Title)
		category := ""
		body := inClassProblemBody(w.lesson, p, settings.Timezone)
		if p.IsPoll() {
			category = "poll"
			if title == "" {
				title = "课堂投票"
			}
			body = inClassPollBody(w.lesson, p, settings.Timezone)
		} else if title == "" {
			title = "课堂习题"
		}
		item = core.InClassQuiz{
			ClassroomID: w.lesson.ClassroomID,
			ActivityID:  "classroom:" + p.ID,
			Course:      w.lesson.CourseName,
			Title:       title,
			Body:        body,
			Notify:      true,
			Category:    category,
		}
	}
	s.rainMu.Unlock()
	if !notify {
		return
	}
	added, err := s.Engine.ReportInClass(core.PlatformRain, []core.InClassQuiz{item}, now)
	if err != nil {
		s.classroomError(w, "课上答题记录保存失败")
		return
	}
	if added > 0 {
		// Bark delivery may take seconds; never block the socket reader that still
		// has to process extend/close messages for the same problem.
		s.startJob(func(context.Context) { s.deliverInClass(w.ctx, w.generation, w.cookie) })
	}
}

func inClassCourseLine(lesson platform.LiveLesson) string {
	if lesson.ClassroomName != "" && lesson.ClassroomName != lesson.CourseName {
		return lesson.CourseName + " · " + lesson.ClassroomName
	}
	return lesson.CourseName
}

func inClassProblemBody(lesson platform.LiveLesson, p platform.ClassroomProblem, timezone string) string {
	parts := []string{inClassCourseLine(lesson), "课堂发布了新的随堂习题"}
	if !p.Deadline.IsZero() {
		location, err := time.LoadLocation(timezone)
		if err != nil {
			location = time.UTC
		}
		parts = append(parts, "限时截止 "+p.Deadline.In(location).Format("15:04:05"))
	}
	parts = append(parts, "打开雨课堂立即作答")
	return strings.Join(parts, "\n")
}

// inClassPollBody announces a poll without ever participating in it. The
// question text is included when the platform supplies plain text, so the
// student can decide whether to open Rain Classroom.
func inClassPollBody(lesson platform.LiveLesson, p platform.ClassroomProblem, timezone string) string {
	parts := []string{inClassCourseLine(lesson)}
	if p.AnonymousPoll() {
		parts = append(parts, "课堂发布了新的匿名投票")
	} else {
		parts = append(parts, "课堂发布了新的投票")
	}
	if prompt := inClassPollPrompt(p.Body); prompt != "" {
		parts = append(parts, prompt)
	}
	if !p.Deadline.IsZero() {
		location, err := time.LoadLocation(timezone)
		if err != nil {
			location = time.UTC
		}
		parts = append(parts, "截止 "+p.Deadline.In(location).Format("15:04:05"))
	}
	parts = append(parts, "打开雨课堂参与投票")
	return strings.Join(parts, "\n")
}

func inClassPollPrompt(body string) string {
	text := strings.Join(strings.Fields(body), " ")
	if text == "" || strings.ContainsAny(text, "<>") {
		return ""
	}
	runes := []rune(text)
	if len(runes) > 160 {
		text = string(runes[:160]) + "…"
	}
	return text
}

func (s *Server) runFallback(w *classroomWatch, id string) {
	for {
		s.rainMu.Lock()
		if !s.classroomCurrentLocked(w) || !s.Engine.Snapshot().Settings.RainFallbackEnabled {
			s.rainMu.Unlock()
			return
		}
		p := w.problems[id]
		changed := w.changed
		s.rainMu.Unlock()
		if p.Answered || p.Closed {
			return
		}
		var wake <-chan time.Time
		var timer *time.Timer
		if !p.Deadline.IsZero() {
			timer = time.NewTimer(max(time.Until(p.Deadline.Add(-rainFallbackLead)), 0))
			wake = timer.C
		}
		select {
		case <-w.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-wake:
		}
		preflight, cancel := context.WithTimeout(w.ctx, 4*time.Second)
		fresh, err := w.channel.CurrentProblem(preflight, id)
		cancel()
		if !s.renewClassroom(w) {
			return
		}
		if err != nil {
			s.classroomError(w, "兜底前无法确认作答状态，已跳过")
			return
		}
		if !fresh.FallbackReady(time.Now()) {
			if !fresh.Closed && !fresh.Answered && fresh.Deadline.After(time.Now().Add(rainFallbackLead)) {
				continue
			}
			return
		}
		label, err := randomOption(fresh.Options)
		if err != nil {
			return
		}
		s.rainMu.Lock()
		if !s.classroomCurrentLocked(w) || !s.Engine.Snapshot().Settings.RainFallbackEnabled {
			s.rainMu.Unlock()
			return
		}
		reserved, err := s.Engine.ReserveClassroomAction(core.PlatformRain, w.lesson.LessonID, id, "fallback", time.Now())
		s.rainMu.Unlock()
		if err != nil {
			s.classroomError(w, "兜底记录保存失败，未提交")
			return
		}
		if !reserved || !s.classroomCurrent(w) || !s.Engine.Snapshot().Settings.RainFallbackEnabled {
			return
		}
		// No lock is held over transport. A request already accepted by the
		// platform cannot be recalled; cancellation stops any not-yet-started one.
		submit, cancel := context.WithDeadline(w.ctx, fresh.Deadline)
		result, err := w.channel.SubmitChoice(submit, fresh, label)
		cancel()
		_ = s.renewClassroom(w)
		s.rainMu.Lock()
		if s.classroomCurrentLocked(w) {
			if err != nil {
				s.classroomStatus.Error = "兜底提交结果未确认，不自动重试"
			} else if result == "already_answered" {
				s.classroomStatus.LastAction = "已手动作答，未覆盖"
			} else {
				s.classroomStatus.LastAction = "随机兜底已提交 · " + label
				s.classroomStatus.Error = ""
			}
		}
		s.rainMu.Unlock()
		return
	}
}

func randomOption(options []string) (string, error) {
	if len(options) < 2 {
		return "", errors.New("no options")
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(options))))
	if err != nil {
		return "", err
	}
	return options[n.Int64()], nil
}

func (s *Server) runAnalysis(w *classroomWatch, p platform.ClassroomProblem) {
	s.rainMu.Lock()
	config := s.Vault.Get()
	if !s.analysisCurrentLocked(w, config, p.ID) {
		s.rainMu.Unlock()
		return
	}
	reserved, err := s.Engine.ReserveClassroomAction(core.PlatformRain, w.lesson.LessonID, p.ID, "analysis", time.Now())
	s.rainMu.Unlock()
	if err != nil {
		s.classroomError(w, "分析记录保存失败，未调用模型")
		return
	}
	if !reserved {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 60*time.Second)
	defer cancel()
	select {
	case s.analysisGate <- struct{}{}:
		defer func() { <-s.analysisGate }()
	case <-ctx.Done():
		return
	}
	if ctx.Err() != nil || !s.analysisCurrent(w, config, p.ID) {
		return
	}
	answer, err := s.analyzeProblem(ctx, config, p)
	if ctx.Err() != nil || !s.analysisCurrent(w, config, p.ID) {
		return
	}
	if err != nil {
		s.classroomError(w, "题目分析失败，未生成参考答案")
		return
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		s.classroomError(w, "模型没有返回参考答案")
		return
	}
	if len([]rune(answer)) > 2000 {
		answer = string([]rune(answer)[:2000])
	}
	// Reuse the durable urgent Bark queue rather than a direct, untracked send.
	s.rainMu.Lock()
	if ctx.Err() != nil || !s.analysisCurrentLocked(w, config, p.ID) {
		s.rainMu.Unlock()
		return
	}
	expires := time.Now().Add(2 * time.Minute)
	latest := w.problems[p.ID]
	if !latest.Deadline.IsZero() && latest.Deadline.Before(expires) {
		expires = latest.Deadline
	}
	err = s.Engine.ReportClassroomAnalysis(w.lesson.LessonID, p.ID, p.Title, w.lesson.CourseName+"\n"+answer+"\n仅供参考，请自行判断；不会自动提交模型答案。", expires, time.Now())
	s.rainMu.Unlock()
	if err != nil {
		s.classroomError(w, "分析通知保存失败")
		return
	}
	s.deliverWithGuard(ctx, func() bool { return s.analysisCurrent(w, config, p.ID) })
}

func (s *Server) analysisCurrent(w *classroomWatch, config vault.Config, id string) bool {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	return s.analysisCurrentLocked(w, config, id)
}

func (s *Server) analysisCurrentLocked(w *classroomWatch, config vault.Config, id string) bool {
	current := s.Vault.Get()
	p, known := w.problems[id]
	return s.classroomCurrentLocked(w) && s.Engine.Snapshot().Settings.RainAnalysisEnabled && config.AnalysisAPIKey != "" && current.BarkKey != "" &&
		current.AnalysisAPIKey == config.AnalysisAPIKey && current.AnalysisModel == config.AnalysisModel &&
		known && !p.Closed && !p.Answered && (p.Deadline.IsZero() || time.Now().Before(p.Deadline))
}
