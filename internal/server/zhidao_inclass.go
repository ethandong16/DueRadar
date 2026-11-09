package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
)

// zhidaoInClassAdapter is the read-only classroom capability. The normal task
// adapter does not implement it, and no method can publish or answer a vote.
type zhidaoInClassAdapter interface {
	LivePollLessons(context.Context) ([]platform.ZhidaoPollLesson, error)
	LivePollSignals(context.Context, platform.ZhidaoPollLesson) ([]platform.ZhidaoPollSignal, error)
	PollDetail(context.Context, platform.ZhidaoPollLesson, string) (platform.ZhidaoPoll, error)
}

const (
	zhidaoInClassTimeout = 30 * time.Second
	// A vote published shortly before the first snapshot is still worth
	// announcing; older waterfall entries become a silent baseline.
	zhidaoPollFreshWindow = 2 * time.Minute
	zhidaoPollFutureSkew  = time.Minute
	zhidaoPollBaselineCap = 200
)

type zhidaoInClassState struct {
	LastCheckAt time.Time
	Lessons     []string
	LastPollAt  time.Time
	LastPoll    string
	Detected    int
	Error       string
}

func (s *Server) setZhidaoInClassStatus(update func(*zhidaoInClassState)) {
	s.zhidaoInClassMu.Lock()
	defer s.zhidaoInClassMu.Unlock()
	update(&s.zhidaoInClassStatus)
}

func (s *Server) zhidaoInClassSnapshot() zhidaoInClassState {
	s.zhidaoInClassMu.Lock()
	defer s.zhidaoInClassMu.Unlock()
	status := s.zhidaoInClassStatus
	status.Lessons = append([]string(nil), status.Lessons...)
	return status
}

func (s *Server) zhidaoInClassInterval() time.Duration {
	seconds := s.Engine.Settings().InClassPollSeconds
	if seconds < 10 || seconds > 300 {
		seconds = core.DefaultInClassPollSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (s *Server) runZhidaoInClass(ctx context.Context) {
	delay := 5 * time.Second
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.checkZhidaoInClass(ctx)
		delay = s.zhidaoInClassInterval()
	}
}

func (s *Server) zhidaoInClassCurrentLocked(ctx context.Context, generation uint64, cookie string) bool {
	return ctx.Err() == nil && s.lifetime.Err() == nil &&
		generation == s.zhidaoGeneration && s.zhidaoState != "initializing" && s.zhidaoState != "waiting" &&
		s.Vault.Get().ZhidaoSession == cookie && s.Engine.Settings().ZhidaoInClassEnabled
}

func (s *Server) zhidaoInClassCurrent(ctx context.Context, generation uint64, cookie string) bool {
	s.zhidaoMu.Lock()
	defer s.zhidaoMu.Unlock()
	return s.zhidaoInClassCurrentLocked(ctx, generation, cookie)
}

func (s *Server) zhidaoClassroomIsBaseline(lessonID string) bool {
	s.zhidaoInClassMu.Lock()
	defer s.zhidaoInClassMu.Unlock()
	return s.zhidaoClassroomBaseline[lessonID]
}

func (s *Server) markZhidaoClassroomBaseline(lessonIDs []string) {
	s.zhidaoInClassMu.Lock()
	defer s.zhidaoInClassMu.Unlock()
	if s.zhidaoClassroomBaseline == nil {
		s.zhidaoClassroomBaseline = make(map[string]bool)
	}
	if len(s.zhidaoClassroomBaseline) >= zhidaoPollBaselineCap {
		// Engine deduplication still suppresses every recorded activity; a
		// cleared baseline only re-checks create times for the next snapshot.
		clear(s.zhidaoClassroomBaseline)
	}
	for _, id := range lessonIDs {
		s.zhidaoClassroomBaseline[id] = true
	}
}

// checkZhidaoInClass reads live Zhidao votes through the official student H5
// endpoints. It never joins a lesson, opens a vote, or submits an answer.
func (s *Server) checkZhidaoInClass(parent context.Context) {
	if s.Demo {
		return
	}
	operationCtx, stop := s.operationContext(parent)
	defer stop()
	s.zhidaoMu.Lock()
	if operationCtx.Err() != nil || s.lifetime.Err() != nil {
		s.zhidaoMu.Unlock()
		return
	}
	settings := s.Engine.Settings()
	generation, cookie := s.zhidaoGeneration, s.Vault.Get().ZhidaoSession
	if !settings.ZhidaoInClassEnabled {
		if operationCtx.Err() == nil && s.lifetime.Err() == nil && !s.Engine.Settings().ZhidaoInClassEnabled {
			s.setZhidaoInClassStatus(func(status *zhidaoInClassState) { *status = zhidaoInClassState{} })
		}
		s.zhidaoMu.Unlock()
		return
	}
	busy := s.zhidaoState == "initializing" || s.zhidaoState == "waiting"
	s.zhidaoMu.Unlock()
	if busy || operationCtx.Err() != nil {
		return
	}
	if cookie == "" {
		s.setZhidaoInClassStatus(func(status *zhidaoInClassState) {
			status.LastCheckAt = time.Now()
			status.Lessons = nil
			status.Error = "请先绑定知到账号"
		})
		return
	}
	ctx, cancel := context.WithTimeout(operationCtx, zhidaoInClassTimeout)
	defer cancel()
	adapter := s.newZhidao(cookie)
	poller, ok := adapter.(zhidaoInClassAdapter)
	if !ok {
		return
	}

	now := time.Now()
	lessons, lessonErr := poller.LivePollLessons(ctx)
	items := make([]core.InClassQuiz, 0, 8)
	baseline := make([]string, 0, len(lessons))
	names := make([]string, 0, len(lessons))
	var firstErr string
	interrupted := !s.zhidaoInClassCurrent(ctx, generation, cookie)
	if lessonErr != nil {
		// Never process a partial discovery response.
		lessons = nil
		firstErr = zhidaoInClassError(lessonErr)
	}
	for _, lesson := range lessons {
		if interrupted || !s.zhidaoInClassCurrent(ctx, generation, cookie) {
			interrupted = true
			break
		}
		names = append(names, lesson.Course)
		signals, err := poller.LivePollSignals(ctx, lesson)
		if !s.zhidaoInClassCurrent(ctx, generation, cookie) {
			interrupted = true
			break
		}
		if err != nil {
			if firstErr == "" {
				firstErr = zhidaoInClassError(err)
			}
			continue
		}
		if !s.zhidaoClassroomIsBaseline(lesson.LessonID) {
			// The first complete snapshot of a lesson records historical votes
			// silently. A vote created moments ago is still announced, so starting
			// the monitor at the beginning of class does not swallow the first poll.
			for _, signal := range signals {
				notify := !signal.CreatedAt.IsZero() &&
					!signal.CreatedAt.Before(now.Add(-zhidaoPollFreshWindow)) &&
					!signal.CreatedAt.After(now.Add(zhidaoPollFutureSkew))
				var poll platform.ZhidaoPoll
				if notify {
					detail, detailErr := poller.PollDetail(ctx, lesson, signal.ID)
					if !s.zhidaoInClassCurrent(ctx, generation, cookie) {
						interrupted = true
						break
					}
					if detailErr != nil {
						if firstErr == "" {
							firstErr = "读取知到投票详情失败"
						}
						continue
					}
					poll = detail
					if poll.StatusKnown && poll.Finished {
						notify = false
					}
				}
				items = append(items, zhidaoPollItem(lesson, signal, poll, notify))
			}
			if !interrupted {
				baseline = append(baseline, lesson.LessonID)
			}
			continue
		}
		for _, signal := range signals {
			activity := "vote:" + signal.ID
			if s.Engine.SeenInClass(core.PlatformZhidaoCampus, lesson.LessonID, activity) {
				continue
			}
			detail, detailErr := poller.PollDetail(ctx, lesson, signal.ID)
			if !s.zhidaoInClassCurrent(ctx, generation, cookie) {
				interrupted = true
				break
			}
			if detailErr != nil {
				if firstErr == "" {
					firstErr = "读取知到投票详情失败"
				}
				continue
			}
			notify := !detail.StatusKnown || !detail.Finished
			items = append(items, zhidaoPollItem(lesson, signal, detail, notify))
		}
		if interrupted {
			break
		}
	}
	if interrupted || !s.zhidaoInClassCurrent(ctx, generation, cookie) {
		return
	}
	added := 0
	if len(items) > 0 {
		var err error
		added, err = s.Engine.ReportInClass(core.PlatformZhidaoCampus, items, now)
		if err != nil {
			firstErr = "知到课堂投票记录保存失败"
		} else {
			s.markZhidaoClassroomBaseline(baseline)
		}
	} else {
		s.markZhidaoClassroomBaseline(baseline)
	}
	if !s.zhidaoInClassCurrent(ctx, generation, cookie) {
		return
	}
	lastPoll := ""
	if added > 0 {
		for _, item := range items {
			if item.Notify {
				lastPoll = item.Title
			}
		}
	}
	s.setZhidaoInClassStatus(func(status *zhidaoInClassState) {
		status.LastCheckAt = now
		status.Lessons = names
		status.Error = firstErr
		if added > 0 {
			status.Detected += added
			status.LastPollAt = now
			status.LastPoll = lastPoll
		}
	})
	if added > 0 {
		s.deliverZhidaoInClass(operationCtx, generation, cookie)
	}
}

func (s *Server) deliverZhidaoInClass(parent context.Context, generation uint64, cookie string) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	s.deliverWithGuard(ctx, func() bool { return s.zhidaoInClassCurrent(ctx, generation, cookie) })
}

func zhidaoInClassError(err error) string {
	switch {
	case errors.Is(err, platform.ErrZhidaoClassroomSession):
		return "知到会话缺少课堂端凭据，请重新扫码登录"
	case errors.Is(err, platform.ErrSessionExpired):
		return "知到课堂会话已过期，请重新扫码登录"
	default:
		return "读取知到课堂互动失败"
	}
}

func zhidaoPollItem(lesson platform.ZhidaoPollLesson, signal platform.ZhidaoPollSignal, poll platform.ZhidaoPoll, notify bool) core.InClassQuiz {
	title := poll.Title
	if title == "" {
		title = signal.Title
	}
	if title == "" {
		title = "课堂投票"
	}
	return core.InClassQuiz{
		ClassroomID: lesson.LessonID,
		ActivityID:  "vote:" + signal.ID,
		Course:      lesson.Course,
		Title:       title,
		Body:        zhidaoPollBody(lesson, poll),
		Notify:      notify,
		Category:    "poll",
	}
}

// zhidaoPollBody announces a vote without participating in it. Option texts
// are plain labels only; counts and participant identities are never included.
func zhidaoPollBody(lesson platform.ZhidaoPollLesson, poll platform.ZhidaoPoll) string {
	parts := []string{lesson.Course}
	if poll.Anonymous {
		parts = append(parts, "课堂发布了新的匿名投票")
	} else {
		parts = append(parts, "课堂发布了新的投票")
	}
	options := make([]string, 0, 6)
	for _, option := range poll.Options {
		if text := inClassPollPrompt(option); text != "" {
			options = append(options, text)
		}
		if len(options) == 6 {
			break
		}
	}
	if len(options) > 0 {
		parts = append(parts, "选项："+strings.Join(options, " / "))
	}
	if !poll.StatusKnown {
		parts = append(parts, "投票状态暂未确认，请打开知到查看")
	}
	parts = append(parts, "打开知到参与投票")
	return strings.Join(parts, "\n")
}
