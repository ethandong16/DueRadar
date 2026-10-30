package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

type fakeLiveRain struct {
	fakeRainAdapter
	lessons    func(context.Context) ([]platform.LiveLesson, error)
	activities func(context.Context, string) ([]platform.LiveActivity, error)
	quick      func(context.Context, string) ([]platform.LiveQuickAnswer, error)
}

func (f fakeLiveRain) LiveLessons(ctx context.Context) ([]platform.LiveLesson, error) {
	if f.lessons == nil {
		return nil, nil
	}
	return f.lessons(ctx)
}

func (f fakeLiveRain) LiveActivities(ctx context.Context, classroomID string) ([]platform.LiveActivity, error) {
	if f.activities == nil {
		return nil, nil
	}
	return f.activities(ctx, classroomID)
}

func (f fakeLiveRain) LiveQuickAnswers(ctx context.Context, lessonID string) ([]platform.LiveQuickAnswer, error) {
	if f.quick == nil {
		return nil, nil
	}
	return f.quick(ctx, lessonID)
}

func prepareInClass(t *testing.T, s *Server, live fakeLiveRain, now time.Time) *[]string {
	t.Helper()
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.RainCookie, c.BarkURL, c.BarkKey = "sessionid=test", "https://api.day.app", "test-key"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Sync(core.PlatformRain, nil, now); err != nil {
		t.Fatal(err)
	}
	sent := []string{}
	s.sendBark = func(_ context.Context, _, _, title, _ string) error {
		sent = append(sent, title)
		return nil
	}
	s.newRain = func(string) rainAdapter { return live }
	return &sent
}

func TestCheckInClassNotifiesNewQuizOnce(t *testing.T) {
	s, _ := testApp(t)
	now := time.Now()
	live := fakeLiveRain{
		lessons: func(context.Context) ([]platform.LiveLesson, error) {
			return []platform.LiveLesson{{ClassroomID: "100", LessonID: "9001", CourseName: "离散数学", ClassroomName: "A-01"}}, nil
		},
		activities: func(context.Context, string) ([]platform.LiveActivity, error) {
			return []platform.LiveActivity{
				{ID: "7", Type: 5, Title: "随堂测验6", CreatedAt: time.Now().Add(-2 * time.Minute), ProblemCount: 2, TotalScore: 2},
				{ID: "6", Type: 14, Title: "第2章", CreatedAt: time.Now()},
			}, nil
		},
	}
	sent := prepareInClass(t, s, live, now)

	s.checkInClass(context.Background())
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "课上答题 · ") {
		t.Fatalf("sent=%v", *sent)
	}
	events := s.Engine.Snapshot().Events
	if len(events) != 1 || events[0].Kind != "in_class" {
		t.Fatalf("events=%+v", events)
	}
	status := s.inClassSnapshot()
	if len(status.Lessons) != 1 || status.Detected != 1 || status.LastQuiz != "随堂测验6" || status.Error != "" {
		t.Fatalf("status=%+v", status)
	}

	// The same page on the next poll must not send anything again.
	s.checkInClass(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("duplicate notification: %v", *sent)
	}
}

func TestCheckInClassNotifiesPublishedQuickAnswer(t *testing.T) {
	s, _ := testApp(t)
	now := time.Now()
	live := fakeLiveRain{
		lessons: func(context.Context) ([]platform.LiveLesson, error) {
			return []platform.LiveLesson{{ClassroomID: "100", LessonID: "9001", CourseName: "离散数学", Role: 5}}, nil
		},
		activities: func(context.Context, string) ([]platform.LiveActivity, error) {
			return nil, nil
		},
		quick: func(_ context.Context, lessonID string) ([]platform.LiveQuickAnswer, error) {
			if lessonID != "9001" {
				t.Fatalf("lessonID=%s", lessonID)
			}
			return []platform.LiveQuickAnswer{{ID: "qa-1", SentAt: now.Add(-2 * time.Minute)}}, nil
		},
	}
	sent := prepareInClass(t, s, live, now)
	s.checkInClass(context.Background())
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "课堂答题") {
		t.Fatalf("sent=%v", *sent)
	}
	events := s.Engine.Snapshot().Events
	if len(events) != 1 || events[0].Kind != "in_class" || events[0].Title != "课上答题 · 课堂答题" {
		t.Fatalf("events=%+v", events)
	}
}

func TestCheckInClassSkipsQuickAnswersForTeacherLessons(t *testing.T) {
	for _, role := range []int{1, 2} {
		t.Run(fmt.Sprintf("role-%d", role), func(t *testing.T) {
			s, _ := testApp(t)
			now := time.Now()
			queried := false
			live := fakeLiveRain{
				lessons: func(context.Context) ([]platform.LiveLesson, error) {
					return []platform.LiveLesson{{ClassroomID: "200", LessonID: "9002", CourseName: "Test", Role: role}}, nil
				},
				activities: func(context.Context, string) ([]platform.LiveActivity, error) {
					return nil, nil
				},
				quick: func(context.Context, string) ([]platform.LiveQuickAnswer, error) {
					queried = true
					return nil, errors.New("雨课堂 HTTP 状态 500")
				},
			}
			prepareInClass(t, s, live, now)
			s.checkInClass(context.Background())
			if queried {
				t.Fatal("teacher or assistant classroom must not call the student quick-answer endpoint")
			}
			if status := s.inClassSnapshot(); status.Error != "" {
				t.Fatalf("teacher or assistant classroom must not surface a read error: %+v", status)
			}
		})
	}
}

func TestCheckInClassIgnoresStaleActivities(t *testing.T) {
	s, _ := testApp(t)
	now := time.Now()
	live := fakeLiveRain{
		lessons: func(context.Context) ([]platform.LiveLesson, error) {
			return []platform.LiveLesson{{ClassroomID: "100", CourseName: "离散数学"}}, nil
		},
		activities: func(context.Context, string) ([]platform.LiveActivity, error) {
			return []platform.LiveActivity{{ID: "5", Type: 5, Title: "上周的随堂测验", CreatedAt: now.Add(-3 * time.Hour)}}, nil
		},
	}
	sent := prepareInClass(t, s, live, now)
	s.checkInClass(context.Background())
	if len(*sent) != 0 {
		t.Fatalf("stale quiz must stay silent: %v", *sent)
	}
	if len(s.Engine.Snapshot().InClassSeen) != 1 {
		t.Fatal("stale activity must still be recorded")
	}
}

func TestCheckInClassDisabledOrUnbound(t *testing.T) {
	s, _ := testApp(t)
	settings := s.Engine.Snapshot().Settings
	settings.InClassEnabled = false
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	called := false
	s.newRain = func(string) rainAdapter {
		called = true
		return fakeLiveRain{}
	}
	s.checkInClass(context.Background())
	if called {
		t.Fatal("disabled monitoring must not contact the platform")
	}

	// No bound Rain session means there is nothing to watch.
	s2, _ := testApp(t)
	s2.newRain = func(string) rainAdapter { called = true; return fakeLiveRain{} }
	s2.checkInClass(context.Background())
	if called {
		t.Fatal("unbound account must not contact the platform")
	}
}

func TestCheckInClassRecordsErrorsWithoutFailingSync(t *testing.T) {
	s, _ := testApp(t)
	now := time.Now()
	live := fakeLiveRain{
		lessons: func(context.Context) ([]platform.LiveLesson, error) {
			return nil, errors.New("network down")
		},
	}
	prepareInClass(t, s, live, now)
	s.checkInClass(context.Background())
	status := s.inClassSnapshot()
	if status.Error == "" {
		t.Fatal("monitor failure must be visible")
	}
	for _, account := range s.Engine.Snapshot().Accounts {
		if account.Platform == core.PlatformRain && account.ConsecutiveFailures != 0 {
			t.Fatalf("fast loop must not count sync failures: %+v", account)
		}
	}
}

func TestInClassTestEndpoint(t *testing.T) {
	s, h := testApp(t)
	c := loginCookie(t, h)
	if w := request(h, "POST", "/api/in-class/test", "", c, ""); w.Code != 409 {
		t.Fatalf("unconfigured Bark status=%d body=%s", w.Code, w.Body.String())
	}
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.BarkURL, c.BarkKey = "https://api.day.app", "test-key"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Sync(core.PlatformRain, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	sent := 0
	s.sendBark = func(_ context.Context, _, _, title, body string) error {
		sent++
		if !strings.HasPrefix(title, "课上答题 · ") || !strings.Contains(body, "链路") {
			t.Fatalf("unexpected payload %q %q", title, body)
		}
		return nil
	}
	if w := request(h, "POST", "/api/in-class/test", "", c, ""); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if sent != 1 {
		t.Fatalf("sent=%d", sent)
	}
	events := s.Engine.Snapshot().Events
	if len(events) != 1 || events[0].Kind != "in_class" || events[0].Status != "sent" {
		t.Fatalf("events=%+v", events)
	}
}

func TestCheckInClassUsesLessonStartWindow(t *testing.T) {
	s, _ := testApp(t)
	now := time.Now()
	live := fakeLiveRain{
		lessons: func(context.Context) ([]platform.LiveLesson, error) {
			return []platform.LiveLesson{{ClassroomID: "100", CourseName: "离散数学"}}, nil
		},
		activities: func(context.Context, string) ([]platform.LiveActivity, error) {
			return []platform.LiveActivity{
				// Pushed before the lesson record but during the same class.
				{ID: "7", Type: 5, Title: "随堂测验7", CreatedAt: now.Add(-100 * time.Minute)},
				{ID: "9", Type: 14, Title: "第2章", CreatedAt: now.Add(-80 * time.Minute)},
				// Far older history must stay silent.
				{ID: "5", Type: 5, Title: "上学期的测验", CreatedAt: now.Add(-200 * time.Minute)},
			}, nil
		},
	}
	sent := prepareInClass(t, s, live, now)
	s.checkInClass(context.Background())
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "随堂测验7") {
		t.Fatalf("sent=%v", *sent)
	}
}
