package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

// inClassAdapter is the extra capability the fast monitor needs. Adapters used
// only for task sync do not implement it and are ignored.
type inClassAdapter interface {
	LiveLessons(context.Context) ([]platform.LiveLesson, error)
	LiveActivities(context.Context, string) ([]platform.LiveActivity, error)
}

type inClassQuickAnswerAdapter interface {
	LiveQuickAnswers(context.Context, string) ([]platform.LiveQuickAnswer, error)
}

// inClassNotifyWindow decides whether a newly observed activity is still worth
// announcing. The first poll of a lesson may already see a quiz pushed a few
// minutes earlier; anything older is recorded silently to avoid a burst of
// stale notifications when monitoring starts in the middle of a class.
const inClassNotifyWindow = 30 * time.Minute

// Teachers often prepare and push the quiz shortly before the lesson record is
// written. The lesson start therefore extends the window backwards instead of
// being used as a hard lower bound.
const inClassPreLessonWindow = 90 * time.Minute

const inClassPollTimeout = 20 * time.Second

type inClassStatus struct {
	LastCheckAt time.Time
	Lessons     []string
	LastQuizAt  time.Time
	LastQuiz    string
	Detected    int
	Error       string
}

func (s *Server) setInClassStatus(update func(*inClassStatus)) {
	s.inClassMu.Lock()
	defer s.inClassMu.Unlock()
	update(&s.inClassStatus)
}

func (s *Server) inClassSnapshot() inClassStatus {
	s.inClassMu.Lock()
	defer s.inClassMu.Unlock()
	status := s.inClassStatus
	status.Lessons = append([]string(nil), status.Lessons...)
	return status
}

func (s *Server) inClassInterval() time.Duration {
	seconds := s.Engine.Snapshot().Settings.InClassPollSeconds
	if seconds < 10 || seconds > 300 {
		seconds = core.DefaultInClassPollSeconds
	}
	return time.Duration(seconds) * time.Second
}

// runInClass keeps a light polling loop that only works while a Rain lesson is
// live. It is deliberately separate from the 15-minute task sync so a quiz can
// be reported within seconds without increasing full synchronizations.
func (s *Server) runInClass(ctx context.Context) {
	delay := 5 * time.Second
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.checkInClass(ctx)
		delay = s.inClassInterval()
	}
}

// Rain's binding and renewal paths also hold rainMu while changing its cookie.
func (s *Server) inClassBindingCurrentLocked(ctx context.Context, generation uint64, cookie string) bool {
	return ctx.Err() == nil && s.lifetime.Err() == nil &&
		generation == s.rainGeneration && s.rainState != "initializing" && s.rainState != "waiting" &&
		s.Vault.Get().RainCookie == cookie
}

func (s *Server) inClassCurrentLocked(ctx context.Context, generation uint64, cookie string) bool {
	return s.inClassBindingCurrentLocked(ctx, generation, cookie) && s.Engine.Snapshot().Settings.InClassEnabled
}

func (s *Server) inClassCurrent(ctx context.Context, generation uint64, cookie string) bool {
	s.rainMu.Lock()
	defer s.rainMu.Unlock()
	return s.inClassCurrentLocked(ctx, generation, cookie)
}

func (s *Server) checkInClass(parent context.Context) {
	if s.Demo {
		return
	}
	operationCtx, stop := s.operationContext(parent)
	defer stop()
	s.rainMu.Lock()
	if operationCtx.Err() != nil || s.lifetime.Err() != nil {
		s.rainMu.Unlock()
		return
	}
	settings := s.Engine.Snapshot().Settings
	generation, cookie := s.rainGeneration, s.Vault.Get().RainCookie
	if !settings.InClassEnabled {
		if operationCtx.Err() == nil && s.lifetime.Err() == nil && !s.Engine.Snapshot().Settings.InClassEnabled {
			s.setInClassStatus(func(status *inClassStatus) { *status = inClassStatus{} })
		}
		s.rainMu.Unlock()
		return
	}
	busy := s.rainState == "initializing" || s.rainState == "waiting"
	s.rainMu.Unlock()
	if busy || cookie == "" || operationCtx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(operationCtx, inClassPollTimeout)
	defer cancel()
	adapter := s.newRain(cookie)
	live, ok := adapter.(inClassAdapter)
	if !ok || !s.inClassCurrent(ctx, generation, cookie) {
		return
	}

	lessons, lessonErr := live.LiveLessons(ctx)
	now := time.Now()
	items := make([]core.InClassQuiz, 0, 8)
	names := make([]string, 0, len(lessons))
	var firstErr string
	interrupted := !s.inClassCurrent(ctx, generation, cookie)
	if lessonErr != nil {
		// Some servers rotate a session even on an error response. Export it
		// below, but never process a partial lesson list.
		lessons = nil
		firstErr = "读取在课状态失败"
	}
	for _, lesson := range lessons {
		// A later re-enable must not make an interrupted poll's partial result
		// look complete. Still export any issued renewal below.
		if interrupted || !s.inClassCurrent(ctx, generation, cookie) {
			interrupted = true
			break
		}
		names = append(names, lesson.CourseName)
		activities, err := live.LiveActivities(ctx, lesson.ClassroomID)
		if !s.inClassCurrent(ctx, generation, cookie) {
			interrupted = true
			break
		}
		if err != nil {
			if firstErr == "" {
				firstErr = "读取课堂活动失败"
			}
		}
		// The newest lesson record marks when this class started; the feed is
		// newest first, so the first type-14 entry is the current lesson.
		threshold := now.Add(-inClassNotifyWindow)
		for _, activity := range activities {
			if activity.Type != 14 || activity.CreatedAt.IsZero() {
				continue
			}
			if pre := activity.CreatedAt.Add(-inClassPreLessonWindow); pre.Before(threshold) {
				threshold = pre
			}
			break
		}
		for _, activity := range activities {
			if !activity.Answerable() || activity.ID == "" {
				continue
			}
			notify := !activity.CreatedAt.IsZero() && !activity.CreatedAt.Before(threshold) && !activity.CreatedAt.After(now.Add(time.Minute))
			items = append(items, core.InClassQuiz{
				ClassroomID: lesson.ClassroomID,
				ActivityID:  activity.ID,
				Course:      lesson.CourseName,
				Title:       activity.Title,
				Body:        inClassBody(lesson, activity, settings.Timezone),
				Notify:      notify,
			})
		}
		// Rain's live "quick answer" prompts are kept in the lesson-summary
		// endpoint and do not necessarily create a logs/learn activity. Read
		// that list as a second, read-only source when the lesson has an id.
		// Teacher and assistant classrooms expose the activity feed but reject
		// the student quick-answer endpoint, so only student lessons are read.
		if quick, ok := live.(inClassQuickAnswerAdapter); ok && lesson.LessonID != "" && lesson.IsStudent() {
			if !s.inClassCurrent(ctx, generation, cookie) {
				interrupted = true
				break
			}
			answers, quickErr := quick.LiveQuickAnswers(ctx, lesson.LessonID)
			if !s.inClassCurrent(ctx, generation, cookie) {
				interrupted = true
				break
			}
			if quickErr != nil {
				if firstErr == "" {
					firstErr = "读取课堂答题失败"
				}
			} else {
				for _, answer := range answers {
					if answer.ID == "" || answer.SentAt.IsZero() {
						continue
					}
					title := strings.TrimSpace(answer.Title)
					if title == "" {
						title = "课堂答题"
					}
					items = append(items, core.InClassQuiz{
						ClassroomID: lesson.ClassroomID,
						ActivityID:  "quick:" + answer.ID,
						Course:      lesson.CourseName,
						Title:       title,
						Body:        inClassQuickAnswerBody(lesson),
						Notify:      !answer.SentAt.Before(threshold) && !answer.SentAt.After(now.Add(time.Minute)),
					})
				}
			}
		}
	}
	// Every request, including an unsuccessful one, can rotate a cookie. Export
	// only after the final request; never expose provider diagnostics in status.
	// Finish that renewal even if monitoring was disabled during a request.
	updated, exportErr := sessionCredentials(adapter, cookie)
	s.rainMu.Lock()
	if !s.inClassBindingCurrentLocked(ctx, generation, cookie) {
		s.rainMu.Unlock()
		return
	}
	if exportErr == nil && updated != cookie {
		exportErr = s.Vault.Update(func(c *vault.Config) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.lifetime.Err(); err != nil {
				return err
			}
			if c.RainCookie != cookie {
				return errSupersededLogin
			}
			c.RainCookie = updated
			return nil
		})
		if exportErr == nil {
			cookie = updated
			// Only an overlapping full sync needs a follow-up: routinely syncing
			// after every live cookie rotation would defeat the 15-minute cadence.
			if s.rainSyncCancel != nil {
				s.queueSync()
			}
		}
	}
	// Do not publish observations if the current session could not be saved.
	if interrupted || !s.inClassCurrentLocked(ctx, generation, cookie) {
		s.rainMu.Unlock()
		return
	}
	if exportErr != nil {
		s.setInClassStatus(func(status *inClassStatus) {
			status.LastCheckAt = now
			status.Lessons = nil
			status.Error = "课上会话续期保存失败"
		})
		s.rainMu.Unlock()
		return
	}
	added := 0
	if lessonErr == nil && len(lessons) > 0 {
		var err error
		added, err = s.Engine.ReportInClass(core.PlatformRain, items, now)
		if err != nil {
			firstErr = "课上答题记录保存失败"
		}
	}
	// Cancellation or a concurrent settings change during disk persistence
	// must not result in a subsequent status update or immediate delivery.
	if !s.inClassCurrentLocked(ctx, generation, cookie) {
		s.rainMu.Unlock()
		return
	}
	s.setInClassStatus(func(status *inClassStatus) {
		status.LastCheckAt = now
		status.Lessons = names
		status.Error = firstErr
		if added > 0 {
			status.Detected += added
			status.LastQuizAt = now
			status.LastQuiz = latestQuizTitle(items)
		}
	})
	s.rainMu.Unlock()
	if added > 0 {
		s.deliverInClass(operationCtx, generation, cookie)
	}
}

// Revalidate after the shared delivery gate, before claims and transport, but
// never hold rainMu over network I/O. An already-started attempt cannot be
// retracted; independently scheduled delivery has its own validity checks.
func (s *Server) deliverInClass(parent context.Context, generation uint64, cookie string) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	s.deliverWithGuard(ctx, func() bool { return s.inClassCurrent(ctx, generation, cookie) })
}

func inClassQuickAnswerBody(lesson platform.LiveLesson) string {
	parts := []string{lesson.CourseName}
	if lesson.ClassroomName != "" && lesson.ClassroomName != lesson.CourseName {
		parts[0] = lesson.CourseName + " · " + lesson.ClassroomName
	}
	parts = append(parts, "课堂发布了新的答题")
	parts = append(parts, "打开雨课堂立即作答")
	return strings.Join(parts, "\n")
}

func latestQuizTitle(items []core.InClassQuiz) string {
	for _, item := range items {
		if item.Notify {
			return item.Title
		}
	}
	return ""
}

func inClassBody(lesson platform.LiveLesson, activity platform.LiveActivity, timezone string) string {
	parts := []string{lesson.CourseName}
	if lesson.ClassroomName != "" && lesson.ClassroomName != lesson.CourseName {
		parts[0] = lesson.CourseName + " · " + lesson.ClassroomName
	}
	detail := make([]string, 0, 2)
	if activity.ProblemCount > 0 {
		detail = append(detail, fmt.Sprintf("共 %d 题", activity.ProblemCount))
	}
	if activity.TotalScore > 0 {
		detail = append(detail, fmt.Sprintf("满分 %g 分", activity.TotalScore))
	}
	if len(detail) > 0 {
		parts = append(parts, strings.Join(detail, " · "))
	}
	if activity.Deadline != nil {
		location, err := time.LoadLocation(timezone)
		if err != nil {
			location = time.UTC
		}
		parts = append(parts, "截止 "+activity.Deadline.In(location).Format("01-02 15:04"))
	}
	parts = append(parts, "打开雨课堂立即作答")
	return strings.Join(parts, "\n")
}

// testInClass sends one labelled notification through the real in-class path so
// the delivery chain, including the quiet-hours exception, can be verified
// without waiting for a live class.
func (s *Server) testInClass(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		fail(w, 409, "演示模式不发送真实通知")
		return
	}
	if s.Vault.Get().BarkKey == "" {
		fail(w, 409, "请先配置 Bark 通知")
		return
	}
	now := time.Now()
	item := core.InClassQuiz{
		ClassroomID: "test",
		ActivityID:  fmt.Sprintf("%d", now.UnixNano()),
		Course:      "DueRadar",
		Title:       "链路测试",
		Body:        "DueRadar 课上答题提醒链路正常。\n真实课堂出现新的随堂答题时，会以同样方式立即提醒，静默时段也照常发送。",
		Notify:      true,
	}
	added, err := s.Engine.ReportInClass(core.PlatformRain, []core.InClassQuiz{item}, now)
	if err != nil || added == 0 {
		fail(w, 500, "测试通知创建失败")
		return
	}
	deliverCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	s.Deliver(deliverCtx)
	jsonResponse(w, 200, map[string]string{"message": "课上答题测试通知已发送"})
}
