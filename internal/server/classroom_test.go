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

type fakeClassroom struct {
	events  chan platform.ClassroomProblem
	current func(context.Context, string) (platform.ClassroomProblem, error)
	submit  func(context.Context, platform.ClassroomProblem, string) (string, error)
}

func (f *fakeClassroom) Run(ctx context.Context, observe func(platform.ClassroomProblem)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case p := <-f.events:
			observe(p)
		}
	}
}
func (f *fakeClassroom) CurrentProblem(ctx context.Context, id string) (platform.ClassroomProblem, error) {
	return f.current(ctx, id)
}
func (f *fakeClassroom) SubmitChoice(ctx context.Context, p platform.ClassroomProblem, label string) (string, error) {
	return f.submit(ctx, p, label)
}
func (*fakeClassroom) Close() {}

func serverClassroomProblem() platform.ClassroomProblem {
	now := time.Now()
	return platform.ClassroomProblem{LessonID: "10", ID: "40", PresentationID: "20", Title: "Fixture problem", Body: "1+1? A:2 B:3", Type: "MultipleChoice", Options: []string{"A", "B"}, StartedAt: now.Add(-time.Minute), TimingAt: now, Deadline: now.Add(8 * time.Second), AnswerKnown: true}
}

func prepareClassroom(t *testing.T, s *Server, fallback, analysis bool, channel classroomChannel) *classroomWatch {
	t.Helper()
	settings := s.Engine.Snapshot().Settings
	settings.RainFallbackEnabled, settings.RainAnalysisEnabled = fallback, analysis
	settings.InClassEnabled = false // These tests isolate the assistant action paths.
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.RainCookie, c.BarkURL, c.BarkKey = "sessionid=test", "https://api.day.app", "fixture-bark-key"
		c.AnalysisAPIKey = "fixture-tenant-model-key"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Sync(core.PlatformRain, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(s.lifetime)
	t.Cleanup(cancel)
	p := serverClassroomProblem()
	w := &classroomWatch{generation: s.rainGeneration, epoch: s.classroomEpoch, cookie: s.Vault.Get().RainCookie,
		lesson: platform.LiveLesson{LessonID: "10", ClassroomID: "100", CourseName: "Fixture course", Role: 5},
		ctx:    ctx, cancel: cancel, adapter: fakeRainAdapter{}, channel: channel, problems: map[string]platform.ClassroomProblem{p.ID: p}, fallbacks: map[string]bool{}, changed: make(chan struct{})}
	s.classrooms[w.lesson.LessonID] = w
	s.sendBark = func(context.Context, string, string, string, string) error {
		t.Error("unexpected real notification path")
		return errors.New("fixture")
	}
	s.analyzeProblem = func(context.Context, vault.Config, platform.ClassroomProblem) (string, error) {
		t.Error("unexpected model path")
		return "", errors.New("fixture")
	}
	return w
}

func TestClassroomFallbackPreflightSafetyAndDeduplication(t *testing.T) {
	for _, mode := range []string{"ready", "manual answer", "unknown answer", "expired", "unsupported", "read failure", "ambiguous submit", "already answered"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := testApp(t)
			var submissions atomic.Int32
			f := &fakeClassroom{}
			f.current = func(context.Context, string) (platform.ClassroomProblem, error) {
				p := serverClassroomProblem()
				switch mode {
				case "manual answer":
					p.Answered = true
				case "unknown answer":
					p.AnswerKnown = false
				case "expired":
					p.Deadline = time.Now().Add(-time.Second)
				case "unsupported":
					p.Type = "Polling"
				case "read failure":
					return p, errors.New("fixture private provider error")
				}
				return p, nil
			}
			f.submit = func(_ context.Context, p platform.ClassroomProblem, label string) (string, error) {
				submissions.Add(1)
				if label != "A" && label != "B" {
					t.Error("not a single option label")
				}
				if !p.FallbackReady(time.Now()) {
					t.Error("submitted outside fallback window")
				}
				if mode == "ambiguous submit" {
					return "", errors.New("fixture private response")
				}
				if mode == "already answered" {
					return "already_answered", nil
				}
				return "submitted", nil
			}
			w := prepareClassroom(t, s, true, false, f)
			s.runFallback(w, "40")
			s.runFallback(w, "40") // Reconnect/repeated worker must not retry.
			want := int32(0)
			if mode == "ready" || mode == "ambiguous submit" || mode == "already answered" {
				want = 1
			}
			if submissions.Load() != want {
				t.Fatalf("submitted %d times, want %d", submissions.Load(), want)
			}
			if len(s.Engine.Snapshot().ClassroomActions) != int(want) {
				t.Fatal("incorrect durable action count")
			}
			s.rainMu.Lock()
			status := s.classroomStatus
			s.rainMu.Unlock()
			if strings.Contains(status.Error, "private") {
				t.Fatal("provider details leaked")
			}
			if mode == "already answered" && !strings.Contains(status.LastAction, "未覆盖") {
				t.Fatal("already-answered status missing")
			}
		})
	}
}

func TestClassroomConcurrentFallbackReservesOnlyOnce(t *testing.T) {
	s, _ := testApp(t)
	var attempts atomic.Int32
	f := &fakeClassroom{
		current: func(context.Context, string) (platform.ClassroomProblem, error) { return serverClassroomProblem(), nil },
		submit: func(context.Context, platform.ClassroomProblem, string) (string, error) {
			attempts.Add(1)
			return "submitted", nil
		},
	}
	w := prepareClassroom(t, s, true, false, f)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { s.runFallback(w, "40") })
	}
	wg.Wait()
	if attempts.Load() != 1 {
		t.Fatalf("concurrent workers submitted %d times", attempts.Load())
	}
}

func TestClassroomPreflightFencedByDisableRebindAndShutdown(t *testing.T) {
	for _, mode := range []string{"disable", "same-cookie rebind", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := testApp(t)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var attempts atomic.Int32
			f := &fakeClassroom{
				current: func(context.Context, string) (platform.ClassroomProblem, error) {
					close(entered)
					<-release
					return serverClassroomProblem(), nil
				},
				submit: func(context.Context, platform.ClassroomProblem, string) (string, error) {
					attempts.Add(1)
					return "submitted", nil
				},
			}
			w := prepareClassroom(t, s, true, false, f)
			if !s.startJob(func(context.Context) { defer close(done); s.runFallback(w, "40") }) {
				t.Fatal("could not start fixture job")
			}
			await(t, entered)
			if mode == "shutdown" {
				s.stopBackground()
			} else {
				s.rainMu.Lock()
				if mode == "disable" {
					settings := s.Engine.Snapshot().Settings
					settings.RainFallbackEnabled = false
					if err := s.Engine.UpdateSettings(settings); err != nil {
						t.Error(err)
					}
				} else {
					s.rainGeneration++
				}
				s.invalidateClassroomsLocked()
				s.rainMu.Unlock()
			}
			close(release)
			await(t, done)
			if attempts.Load() != 0 || len(s.Engine.Snapshot().ClassroomActions) != 0 {
				t.Fatal("stale preflight authorized a submission")
			}
		})
	}
}

func TestClassroomUnknownDeadlineWaitsAndAnalysisDoesNotBlockFallback(t *testing.T) {
	s, _ := testApp(t)
	modelStarted, release, submitted := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	f := &fakeClassroom{
		current: func(context.Context, string) (platform.ClassroomProblem, error) { return serverClassroomProblem(), nil },
		submit: func(context.Context, platform.ClassroomProblem, string) (string, error) {
			submitted <- struct{}{}
			return "submitted", nil
		},
	}
	w := prepareClassroom(t, s, true, true, f)
	delete(w.problems, "40")
	s.analyzeProblem = func(ctx context.Context, _ vault.Config, _ platform.ClassroomProblem) (string, error) {
		close(modelStarted)
		select {
		case <-release:
			return "A: fixture", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	p := serverClassroomProblem()
	p.Deadline = time.Time{}
	s.observeClassroomProblem(w, p)
	await(t, modelStarted)
	if len(submitted) != 0 {
		t.Fatal("unknown deadline allowed fallback")
	}
	p = serverClassroomProblem()
	s.observeClassroomProblem(w, p)
	await(t, submitted) // Must not wait for model completion.
	w.cancel()
	close(release)
}

func TestClassroomAnalysisImmediateOnceAndNeverSubmitsModelAnswer(t *testing.T) {
	s, _ := testApp(t)
	var modelCalls, pushes atomic.Int32
	f := &fakeClassroom{submit: func(context.Context, platform.ClassroomProblem, string) (string, error) {
		t.Error("AI answer was submitted")
		return "", nil
	}}
	w := prepareClassroom(t, s, false, true, f)
	delete(w.problems, "40")
	s.analyzeProblem = func(_ context.Context, c vault.Config, p platform.ClassroomProblem) (string, error) {
		modelCalls.Add(1)
		if c.AnalysisAPIKey != "fixture-tenant-model-key" || p.ID != "40" {
			t.Error("wrong tenant/problem")
		}
		return "A: fixture reason", nil
	}
	s.sendBark = func(_ context.Context, _, _, title, body string) error {
		if !strings.Contains(title, "AI 参考答案") || !strings.Contains(body, "不会自动提交模型答案") {
			t.Error("missing reference-only notice")
		}
		pushes.Add(1)
		return nil
	}
	p := serverClassroomProblem()
	p.Deadline = time.Now().Add(time.Minute) // Analysis starts long before fallback.
	s.observeClassroomProblem(w, p)
	eventually(t, func() bool { return pushes.Load() == 1 })
	s.observeClassroomProblem(w, p)
	s.runAnalysis(w, p) // Reconnect worker uses the same persisted reservation.
	if modelCalls.Load() != 1 || pushes.Load() != 1 {
		t.Fatal("duplicate model cost or Bark push")
	}
}

func TestClassroomTimelineProblemNotifiesWithoutSlideOptions(t *testing.T) {
	s, _ := testApp(t)
	w := prepareClassroom(t, s, false, false, &fakeClassroom{})
	settings := s.Engine.Snapshot().Settings
	settings.InClassEnabled = true
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	var pushes atomic.Int32
	var mu sync.Mutex
	var gotTitle, gotBody string
	s.sendBark = func(_ context.Context, _, _, title, body string) error {
		mu.Lock()
		gotTitle, gotBody = title, body
		mu.Unlock()
		pushes.Add(1)
		return nil
	}
	now := time.Now()
	// v3 lessons expose the live timeline but not the slide payload. Detection
	// must still announce the problem so the student can open Rain Classroom.
	p := platform.ClassroomProblem{LessonID: "10", ID: "50", StartedAt: now, Deadline: now.Add(45 * time.Second)}
	s.observeClassroomProblem(w, p)
	eventually(t, func() bool { return pushes.Load() == 1 })
	mu.Lock()
	title, body := gotTitle, gotBody
	mu.Unlock()
	if !strings.Contains(title, "课上答题") || !strings.Contains(body, "随堂习题") || !strings.Contains(body, "打开雨课堂") {
		t.Fatalf("title=%q body=%q", title, body)
	}
	s.observeClassroomProblem(w, p)
	if pushes.Load() != 1 {
		t.Fatal("duplicate in-class notification")
	}
}

func TestClassroomPollNotifiesWithoutAnswerPath(t *testing.T) {
	s, _ := testApp(t)
	w := prepareClassroom(t, s, false, false, &fakeClassroom{})
	settings := s.Engine.Snapshot().Settings
	settings.InClassEnabled = true
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	var pushes atomic.Int32
	var mu sync.Mutex
	var gotTitle, gotBody string
	s.sendBark = func(_ context.Context, _, _, title, body string) error {
		mu.Lock()
		gotTitle, gotBody = title, body
		mu.Unlock()
		pushes.Add(1)
		return nil
	}
	now := time.Now()
	p := platform.ClassroomProblem{
		LessonID: "10", ID: "60", PresentationID: "20",
		Title: "你支持哪种解法？", Body: "你更认同哪种解释？", Type: "Polling",
		Options: []string{"赞成", "反对"}, StartedAt: now, Deadline: now.Add(time.Minute),
	}
	s.observeClassroomProblem(w, p)
	eventually(t, func() bool { return pushes.Load() == 1 })
	mu.Lock()
	title, body := gotTitle, gotBody
	mu.Unlock()
	if !strings.Contains(title, "课上投票") || !strings.Contains(title, "你支持哪种解法？") {
		t.Fatalf("title=%q", title)
	}
	if !strings.Contains(body, "课堂发布了新的投票") || !strings.Contains(body, "你更认同哪种解释？") || strings.Contains(body, "作答") {
		t.Fatalf("poll body implies an answer: %q", body)
	}
	if actions := s.Engine.Snapshot().ClassroomActions; len(actions) != 0 {
		t.Fatalf("poll created a classroom action: %+v", actions)
	}
	s.observeClassroomProblem(w, p)
	if pushes.Load() != 1 {
		t.Fatal("duplicate poll notification")
	}
}

func TestClassroomStaleAnalysisCannotNotify(t *testing.T) {
	for _, mode := range []string{"disable", "model change", "rebind", "closed"} {
		t.Run(mode, func(t *testing.T) {
			s, h := testApp(t)
			w := prepareClassroom(t, s, false, true, &fakeClassroom{})
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			s.analyzeProblem = func(context.Context, vault.Config, platform.ClassroomProblem) (string, error) {
				close(entered)
				<-release
				return "A", nil
			}
			var pushes atomic.Int32
			s.sendBark = func(context.Context, string, string, string, string) error { pushes.Add(1); return nil }
			s.startJob(func(context.Context) { defer close(done); s.runAnalysis(w, serverClassroomProblem()) })
			await(t, entered)
			if mode == "model change" {
				cookie := loginCookie(t, h)
				if response := request(h, "PUT", "/api/analysis", `{"model":"claude-sonnet-5-5"}`, cookie, ""); response.Code != 200 {
					t.Errorf("model update: %d", response.Code)
				}
			} else {
				s.rainMu.Lock()
				switch mode {
				case "disable":
					settings := s.Engine.Snapshot().Settings
					settings.RainAnalysisEnabled = false
					if err := s.Engine.UpdateSettings(settings); err != nil {
						t.Error(err)
					}
					s.invalidateClassroomsLocked()
				case "rebind":
					s.rainGeneration++
					s.invalidateClassroomsLocked()
				case "closed":
					p := w.problems["40"]
					p.Closed = true
					w.problems["40"] = p
				}
				s.rainMu.Unlock()
			}
			close(release)
			await(t, done)
			if pushes.Load() != 0 || len(s.Engine.Snapshot().Events) != 0 {
				t.Fatal("stale model result was announced")
			}
		})
	}
}

func TestClassroomQueuedAnalysisRechecksProblemBeforePaidCall(t *testing.T) {
	for _, mode := range []string{"closed", "expired", "manual answer", "missing", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := testApp(t)
			w := prepareClassroom(t, s, false, true, &fakeClassroom{})
			s.analysisGate <- struct{}{} // Another question is already using the model.
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { <-s.analysisGate }) }
			t.Cleanup(release)
			var calls atomic.Int32
			s.analyzeProblem = func(context.Context, vault.Config, platform.ClassroomProblem) (string, error) {
				calls.Add(1)
				return "A", nil
			}
			done := make(chan struct{})
			if !s.startJob(func(context.Context) { defer close(done); s.runAnalysis(w, serverClassroomProblem()) }) {
				t.Fatal("could not queue analysis")
			}
			eventually(t, func() bool { return len(s.Engine.Snapshot().ClassroomActions) == 1 })
			s.rainMu.Lock()
			p := w.problems["40"]
			switch mode {
			case "closed":
				p.Closed = true
			case "expired":
				p.Deadline = time.Now().Add(-time.Second)
			case "manual answer":
				p.Answered = true
			case "canceled":
				w.cancel()
			}
			w.problems[p.ID] = p
			if mode == "missing" {
				delete(w.problems, p.ID)
			}
			s.rainMu.Unlock()
			release()
			await(t, done)
			if calls.Load() != 0 || len(s.Engine.Snapshot().Events) != 0 {
				t.Fatal("stale queued question caused a paid call or notification")
			}
		})
	}
}

func TestClassroomClosedProblemCancelsQueuedAnalysis(t *testing.T) {
	s, _ := testApp(t)
	w := prepareClassroom(t, s, false, true, &fakeClassroom{})
	now := time.Now()
	if err := s.Engine.ReportClassroomAnalysis("10", "40", "fixture", "A", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	claims := s.Engine.Pending(now)
	if len(claims) != 1 {
		t.Fatal("missing queued analysis")
	}
	p := w.problems["40"]
	p.Closed = true
	s.observeClassroomProblem(w, p)
	if s.Engine.ClaimStillValidClaim(claims[0], now) {
		t.Fatal("closed question still authorizes a queued Bark reference answer")
	}
}

func TestClassroomDiscoveryAllOffAndInClassOnlyStudent(t *testing.T) {
	s, _ := testApp(t)
	var discoveries, opened atomic.Int32
	s.newRain = func(string) rainAdapter {
		return fakeLiveRain{lessons: func(context.Context) ([]platform.LiveLesson, error) {
			discoveries.Add(1)
			return []platform.LiveLesson{{LessonID: "10", ClassroomID: "100", Role: 5}, {LessonID: "11", ClassroomID: "101", Role: 1}, {LessonID: "12", ClassroomID: "102", Role: 0}}, nil
		}}
	}
	if err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "sessionid=test"; return nil }); err != nil {
		t.Fatal(err)
	}
	settings := s.Engine.Snapshot().Settings
	settings.InClassEnabled, settings.RainFallbackEnabled, settings.RainAnalysisEnabled = false, false, false
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	s.checkClassrooms(s.lifetime)
	if discoveries.Load() != 0 {
		t.Fatal("all-disabled monitor contacted Rain")
	}
	// In-class notifications alone must open the read-only live channel; the
	// assistant toggles are no longer a prerequisite.
	settings.InClassEnabled = true
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	s.openClassroom = func(_ context.Context, _ rainAdapter, lesson platform.LiveLesson) (classroomChannel, error) {
		if lesson.Role != 5 {
			t.Error("nonstudent channel opened")
		}
		opened.Add(1)
		return &fakeClassroom{events: make(chan platform.ClassroomProblem)}, nil
	}
	s.checkClassrooms(s.lifetime)
	eventually(t, func() bool { return opened.Load() == 1 })
	s.checkClassrooms(s.lifetime)
	if opened.Load() != 1 {
		t.Fatal("discovery duplicated active channel")
	}
}
