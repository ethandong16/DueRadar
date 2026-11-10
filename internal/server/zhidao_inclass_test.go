package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/platform"
	"github.com/ethandong16/due-radar/internal/vault"
)

type fakeZhidaoInClass struct {
	lessons    []platform.ZhidaoPollLesson
	signals    []platform.ZhidaoPollSignal
	lessonErr  error
	signalErr  error
	detailErr  error
	lessonsGot atomic.Int32
	signalsGot atomic.Int32
	detailsGot atomic.Int32
	entered    chan struct{}
	release    chan struct{}
}

func (*fakeZhidaoInClass) Validate(context.Context) error             { return nil }
func (*fakeZhidaoInClass) Tasks(context.Context) ([]core.Task, error) { return nil, nil }

func (f *fakeZhidaoInClass) LivePollLessons(context.Context) ([]platform.ZhidaoPollLesson, error) {
	f.lessonsGot.Add(1)
	return f.lessons, f.lessonErr
}

func (f *fakeZhidaoInClass) LivePollSignals(context.Context, platform.ZhidaoPollLesson) ([]platform.ZhidaoPollSignal, error) {
	f.signalsGot.Add(1)
	if f.entered != nil {
		close(f.entered)
		<-f.release
	}
	return f.signals, f.signalErr
}

func (f *fakeZhidaoInClass) PollDetail(context.Context, platform.ZhidaoPollLesson, string) (platform.ZhidaoPoll, error) {
	f.detailsGot.Add(1)
	return platform.ZhidaoPoll{}, f.detailErr
}

type fakeZhidaoDetail struct {
	fakeZhidaoInClass
	polls map[string]platform.ZhidaoPoll
}

func (f *fakeZhidaoDetail) PollDetail(_ context.Context, _ platform.ZhidaoPollLesson, id string) (platform.ZhidaoPoll, error) {
	f.detailsGot.Add(1)
	poll, ok := f.polls[id]
	if !ok {
		return platform.ZhidaoPoll{}, errors.New("fixture detail missing")
	}
	return poll, f.detailErr
}

func prepareZhidaoInClass(t *testing.T, s *Server, enabled bool, adapter rainAdapter) {
	t.Helper()
	settings := s.Engine.Snapshot().Settings
	settings.ZhidaoInClassEnabled = enabled
	settings.InClassPollSeconds = 10
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Sync(core.PlatformZhidaoCampus, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.ZhidaoSession = "fixture-zhidao-session"
		c.BarkURL, c.BarkKey = "https://api.day.app", "fixture-bark-key"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.newZhidao = func(string) rainAdapter { return adapter }
}

func zhidaoTestLesson() platform.ZhidaoPollLesson {
	return platform.ZhidaoPollLesson{LessonID: "123", CourseID: "course-1", Term: 20271, Course: "概率与统计"}
}

func TestZhidaoInClassBaselineNewVoteAndDedup(t *testing.T) {
	s, _ := testApp(t)
	now := time.Now()
	fake := &fakeZhidaoDetail{
		fakeZhidaoInClass: fakeZhidaoInClass{
			lessons: []platform.ZhidaoPollLesson{zhidaoTestLesson()},
			signals: []platform.ZhidaoPollSignal{
				{ID: "9001", Title: "历史投票", CreatedAt: now.Add(-10 * time.Minute)},
				{ID: "9002", Title: "刚发布的投票", CreatedAt: now.Add(-30 * time.Second)},
			},
		},
		polls: map[string]platform.ZhidaoPoll{
			"9002": {ID: "9002", Title: "刚发布的投票", Options: []string{"赞成", "反对"}, Anonymous: true, ParticipationKnown: true},
		},
	}
	prepareZhidaoInClass(t, s, true, fake)
	var mu sync.Mutex
	var pushes int
	var titles, bodies []string
	s.sendBark = func(_ context.Context, _, _, title, body string) error {
		mu.Lock()
		defer mu.Unlock()
		pushes++
		titles = append(titles, title)
		bodies = append(bodies, body)
		return nil
	}
	s.checkZhidaoInClass(context.Background())
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return pushes == 1
	})
	mu.Lock()
	title, body := titles[0], bodies[0]
	mu.Unlock()
	if !strings.Contains(title, "课上投票") || !strings.Contains(title, "刚发布的投票") {
		t.Fatalf("title=%q", title)
	}
	for _, want := range []string{"概率与统计", "匿名投票", "选项：赞成 / 反对", "打开知到参与投票"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %q", want, body)
		}
	}
	if strings.Contains(body, "作答") || strings.Contains(body, "自动") {
		t.Fatalf("poll body implies an answer: %q", body)
	}
	if fake.detailsGot.Load() != 1 {
		t.Fatalf("unexpected detail calls %d", fake.detailsGot.Load())
	}
	// The historical vote is a silent baseline, not a second notification.
	if len(s.Engine.Snapshot().Events) != 1 {
		t.Fatalf("historical baseline created an event: %+v", s.Engine.Snapshot().Events)
	}
	// Repeating the same snapshot must not refetch or repush.
	s.checkZhidaoInClass(context.Background())
	mu.Lock()
	if pushes != 1 {
		t.Fatalf("duplicate push: %d", pushes)
	}
	mu.Unlock()
	if fake.detailsGot.Load() != 1 {
		t.Fatalf("historical/already recorded vote was re-fetched: %d", fake.detailsGot.Load())
	}
	// A genuinely new vote on the next poll is announced.
	fake.signals = append(fake.signals, platform.ZhidaoPollSignal{ID: "9003", Title: "第三个投票", CreatedAt: now})
	fake.polls["9003"] = platform.ZhidaoPoll{ID: "9003", Title: "第三个投票", ParticipationKnown: true}
	s.checkZhidaoInClass(context.Background())
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return pushes == 2
	})
}

func TestZhidaoInClassParticipatedVoteStaysSilent(t *testing.T) {
	s, _ := testApp(t)
	now := time.Now()
	fake := &fakeZhidaoDetail{
		fakeZhidaoInClass: fakeZhidaoInClass{
			lessons: []platform.ZhidaoPollLesson{zhidaoTestLesson()},
			signals: []platform.ZhidaoPollSignal{{ID: "9001", Title: "已投票", CreatedAt: now.Add(-time.Minute)}},
		},
		polls: map[string]platform.ZhidaoPoll{
			"9001": {ID: "9001", Title: "已投票", ParticipationKnown: true, Participated: true},
		},
	}
	prepareZhidaoInClass(t, s, true, fake)
	var pushes atomic.Int32
	s.sendBark = func(context.Context, string, string, string, string) error { pushes.Add(1); return nil }
	s.checkZhidaoInClass(context.Background())
	if pushes.Load() != 0 {
		t.Fatal("already-answered vote notified")
	}
	if !s.Engine.SeenInClass(core.PlatformZhidaoCampus, "123", "vote:9001") {
		t.Fatal("already-answered vote was not recorded silently")
	}
	s.checkZhidaoInClass(context.Background())
	if fake.detailsGot.Load() != 1 {
		t.Fatalf("recorded vote detail re-fetched: %d", fake.detailsGot.Load())
	}
}

func TestZhidaoInClassEndedVoteStaysSilent(t *testing.T) {
	s, _ := testApp(t)
	fake := &fakeZhidaoDetail{
		fakeZhidaoInClass: fakeZhidaoInClass{
			lessons: []platform.ZhidaoPollLesson{zhidaoTestLesson()},
			signals: []platform.ZhidaoPollSignal{{ID: "9001", Title: "已结束", CreatedAt: time.Now()}},
		},
		polls: map[string]platform.ZhidaoPoll{
			"9001": {ID: "9001", Title: "已结束", ParticipationKnown: true, EndKnown: true, EndAt: time.Now().Add(-time.Minute)},
		},
	}
	prepareZhidaoInClass(t, s, true, fake)
	var pushes atomic.Int32
	s.sendBark = func(context.Context, string, string, string, string) error { pushes.Add(1); return nil }
	s.checkZhidaoInClass(context.Background())
	if pushes.Load() != 0 {
		t.Fatal("ended vote notified")
	}
	if !s.Engine.SeenInClass(core.PlatformZhidaoCampus, "123", "vote:9001") {
		t.Fatal("ended vote was not recorded silently")
	}
}

func TestZhidaoInClassFailureDoesNotEstablishBaseline(t *testing.T) {
	s, _ := testApp(t)
	fake := &fakeZhidaoDetail{
		fakeZhidaoInClass: fakeZhidaoInClass{
			lessons:   []platform.ZhidaoPollLesson{zhidaoTestLesson()},
			signalErr: errors.New("fixture waterfall failure"),
		},
		polls: map[string]platform.ZhidaoPoll{
			"9001": {ID: "9001", Title: "恢复后的投票", ParticipationKnown: true},
		},
	}
	prepareZhidaoInClass(t, s, true, fake)
	var pushes atomic.Int32
	s.sendBark = func(context.Context, string, string, string, string) error { pushes.Add(1); return nil }
	s.checkZhidaoInClass(context.Background())
	if pushes.Load() != 0 || s.zhidaoClassroomIsBaseline("123") {
		t.Fatal("failed snapshot established a baseline")
	}
	fake.signalErr = nil
	fake.signals = []platform.ZhidaoPollSignal{{ID: "9001", Title: "恢复后的投票", CreatedAt: time.Now()}}
	s.checkZhidaoInClass(context.Background())
	eventually(t, func() bool { return pushes.Load() == 1 })
}

func TestZhidaoInClassDisabledDoesNotContactPlatform(t *testing.T) {
	s, _ := testApp(t)
	fake := &fakeZhidaoDetail{fakeZhidaoInClass: fakeZhidaoInClass{lessons: []platform.ZhidaoPollLesson{zhidaoTestLesson()}}}
	prepareZhidaoInClass(t, s, false, fake)
	s.checkZhidaoInClass(context.Background())
	if fake.lessonsGot.Load() != 0 || fake.signalsGot.Load() != 0 {
		t.Fatal("disabled detector contacted the platform")
	}
}

func TestZhidaoInClassGenerationChangeDiscardsObservation(t *testing.T) {
	s, _ := testApp(t)
	fake := &fakeZhidaoDetail{fakeZhidaoInClass: fakeZhidaoInClass{
		lessons: []platform.ZhidaoPollLesson{zhidaoTestLesson()},
		signals: []platform.ZhidaoPollSignal{{ID: "9001", Title: "竞态投票", CreatedAt: time.Now()}},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}}
	prepareZhidaoInClass(t, s, true, fake)
	var pushes atomic.Int32
	s.sendBark = func(context.Context, string, string, string, string) error { pushes.Add(1); return nil }
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.checkZhidaoInClass(context.Background())
	}()
	await(t, fake.entered)
	s.zhidaoMu.Lock()
	s.zhidaoGeneration++
	s.zhidaoMu.Unlock()
	close(fake.release)
	await(t, done)
	if pushes.Load() != 0 || len(s.Engine.Snapshot().Events) != 0 {
		t.Fatal("stale generation announced a vote")
	}
}
