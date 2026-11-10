package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/bark"
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
	s.sendBark = func(_ context.Context, _, _, title, _ string, _ bark.Options) error {
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
	s.sendBark = func(_ context.Context, _, _, title, body string, _ bark.Options) error {
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

func TestPollWakeUsesCriticalBarkOptions(t *testing.T) {
	s, _ := testApp(t)
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.BarkURL, c.BarkKey = "https://api.day.app", "test-key"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	settings := s.Engine.Snapshot().Settings
	settings.PollWakeEnabled = true
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Sync(core.PlatformZhidaoCampus, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	sent := map[string]bark.Options{}
	s.sendBark = func(_ context.Context, _, _, title, _ string, opts bark.Options) error {
		sent[title] = opts
		return nil
	}
	now := time.Now()
	items := []core.InClassQuiz{
		{ClassroomID: "123", ActivityID: "vote:1", Course: "概统", Title: "投票", Body: "投票", Notify: true, Category: "poll"},
		{ClassroomID: "123", ActivityID: "quiz:1", Course: "概统", Title: "习题", Body: "习题", Notify: true},
	}
	if _, err := s.Engine.ReportInClass(core.PlatformZhidaoCampus, items, now); err != nil {
		t.Fatal(err)
	}
	s.Deliver(context.Background())
	if len(sent) != 2 {
		t.Fatalf("sent=%v", sent)
	}
	if got := sent["课上投票 · 投票"]; got != bark.Wake() {
		t.Fatalf("poll options=%+v", got)
	}
	if got := sent["课上答题 · 习题"]; got != (bark.Options{}) {
		t.Fatalf("quiz options=%+v", got)
	}
}

type renewingLiveRain struct {
	fakeLiveRain
	export func() (string, error)
}

func (f renewingLiveRain) Credentials() (string, error) { return f.export() }

func TestCheckInClassSkipsBusyOrCanceledBeforePolling(t *testing.T) {
	for _, scenario := range []string{"initializing", "waiting", "parent_canceled", "shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := testApp(t)
			prepareInClass(t, s, fakeLiveRain{}, time.Now())
			original := inClassStatus{Error: "existing", Detected: 3}
			s.setInClassStatus(func(status *inClassStatus) { *status = original })
			called := false
			s.newRain = func(string) rainAdapter { called = true; return fakeLiveRain{} }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "initializing", "waiting":
				s.rainMu.Lock()
				s.rainState = scenario
				s.rainMu.Unlock()
			case "parent_canceled":
				cancel()
			case "shutdown":
				if err := s.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			s.checkInClass(ctx)
			if called || !reflect.DeepEqual(s.inClassSnapshot(), original) {
				t.Fatal("busy or canceled poll contacted Rain or changed status")
			}
		})
	}
}

func TestCheckInClassRejectsSupersededPoll(t *testing.T) {
	for _, stage := range []string{"lessons", "lesson_error", "activities", "quick", "export", "no_lessons"} {
		for _, reason := range []string{"generation", "cookie", "disabled", "parent_canceled", "shutdown"} {
			t.Run(stage+"/"+reason, func(t *testing.T) {
				s, _ := testApp(t)
				now := time.Now()
				prepareInClass(t, s, fakeLiveRain{}, now)
				before := s.Engine.Snapshot()
				original := inClassStatus{Error: "existing", Lessons: []string{"existing"}, Detected: 3}
				s.setInClassStatus(func(status *inClassStatus) { *status = original })
				entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				pause := func(point string) {
					if stage == point {
						close(entered)
						<-release // Deliberately return data even if the context was canceled.
					}
				}
				var activityCalls, quickCalls, exports, sends atomic.Int32
				live := fakeLiveRain{
					lessons: func(context.Context) ([]platform.LiveLesson, error) {
						switch stage {
						case "lesson_error":
							pause("lesson_error")
							return nil, errors.New("old-session-error")
						case "no_lessons":
							pause("no_lessons")
							return nil, nil
						default:
							pause("lessons")
							return []platform.LiveLesson{{ClassroomID: "1", LessonID: "2", CourseName: "Old"}}, nil
						}
					},
					activities: func(context.Context, string) ([]platform.LiveActivity, error) {
						activityCalls.Add(1)
						pause("activities")
						return []platform.LiveActivity{{ID: "3", Type: 5, Title: "Old quiz", CreatedAt: now}}, nil
					},
					quick: func(context.Context, string) ([]platform.LiveQuickAnswer, error) {
						quickCalls.Add(1)
						pause("quick")
						return []platform.LiveQuickAnswer{{ID: "4", Title: "Old answer", SentAt: now}}, nil
					},
				}
				s.newRain = func(string) rainAdapter {
					return renewingLiveRain{fakeLiveRain: live, export: func() (string, error) {
						exports.Add(1)
						pause("export")
						return "latest-renewal", nil
					}}
				}
				s.sendBark = func(context.Context, string, string, string, string, bark.Options) error { sends.Add(1); return nil }
				go func() { defer close(done); s.checkInClass(ctx) }()
				await(t, entered)
				switch reason {
				case "generation":
					s.rainMu.Lock()
					s.rainGeneration++ // Even rebinding with the same cookie must supersede.
					s.rainMu.Unlock()
				case "cookie":
					s.rainMu.Lock()
					err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "replacement"; return nil })
					s.rainMu.Unlock()
					if err != nil {
						t.Fatal(err)
					}
				case "disabled":
					settings := s.Engine.Snapshot().Settings
					settings.InClassEnabled = false
					if err := s.Engine.UpdateSettings(settings); err != nil {
						t.Fatal(err)
					}
				case "parent_canceled":
					cancel()
				case "shutdown":
					if err := s.Shutdown(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				await(t, done)
				wantCookie := "sessionid=test"
				if reason == "cookie" {
					wantCookie = "replacement"
				} else if reason == "disabled" {
					// Preserve a rotation from the last request even though no
					// observations may be committed after disabling monitoring.
					wantCookie = "latest-renewal"
				}
				if s.Vault.Get().RainCookie != wantCookie {
					t.Fatal("superseded poll overwrote the current cookie or lost a rotation")
				}
				after := s.Engine.Snapshot()
				if !reflect.DeepEqual(after.Events, before.Events) || !reflect.DeepEqual(after.InClassSeen, before.InClassSeen) {
					t.Fatal("superseded poll persisted old quiz observations")
				}
				if !reflect.DeepEqual(s.inClassSnapshot(), original) || sends.Load() != 0 {
					t.Fatal("superseded poll changed status or initiated a notification")
				}
				if stage == "lessons" && activityCalls.Load() != 0 || stage == "activities" && quickCalls.Load() != 0 {
					t.Fatal("superseded poll made an additional platform request")
				}
				if exports.Load() != 1 {
					t.Fatalf("credentials exported %d times", exports.Load())
				}
			})
		}
	}
}

func TestCheckInClassUsesRenewedCredentialOnNextPoll(t *testing.T) {
	s, _ := testApp(t)
	prepareInClass(t, s, fakeLiveRain{}, time.Now())
	var inputs []string
	s.newRain = func(cookie string) rainAdapter {
		inputs = append(inputs, cookie)
		return renewingLiveRain{fakeLiveRain: fakeLiveRain{}, export: func() (string, error) { return "renewed", nil }}
	}
	s.checkInClass(context.Background())
	s.checkInClass(context.Background())
	if !reflect.DeepEqual(inputs, []string{"sessionid=test", "renewed"}) {
		t.Fatalf("subsequent poll did not use the persisted renewal: %v", inputs)
	}
}

func TestCheckInClassDoesNotPublishPartialAfterReenable(t *testing.T) {
	s, _ := testApp(t)
	now := time.Now()
	prepareInClass(t, s, fakeLiveRain{}, now)
	readDone, continueRead := make(chan struct{}), make(chan struct{})
	exportStarted, continueExport, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var sends, quickCalls atomic.Int32
	s.sendBark = func(context.Context, string, string, string, string, bark.Options) error { sends.Add(1); return nil }
	live := fakeLiveRain{
		lessons: func(context.Context) ([]platform.LiveLesson, error) {
			return []platform.LiveLesson{{ClassroomID: "1", LessonID: "2", CourseName: "Test"}}, nil
		},
		activities: func(context.Context, string) ([]platform.LiveActivity, error) {
			close(readDone)
			<-continueRead
			return []platform.LiveActivity{{ID: "3", Type: 5, Title: "Quiz", CreatedAt: now}}, nil
		},
		quick: func(context.Context, string) ([]platform.LiveQuickAnswer, error) {
			quickCalls.Add(1)
			return nil, nil
		},
	}
	s.newRain = func(string) rainAdapter {
		return renewingLiveRain{fakeLiveRain: live, export: func() (string, error) {
			close(exportStarted)
			<-continueExport
			return "renewed", nil
		}}
	}
	go func() { defer close(done); s.checkInClass(context.Background()) }()
	await(t, readDone)
	settings := s.Engine.Snapshot().Settings
	settings.InClassEnabled = false
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	close(continueRead)
	await(t, exportStarted)
	settings.InClassEnabled = true
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	close(continueExport)
	await(t, done)
	if s.Vault.Get().RainCookie != "renewed" || quickCalls.Load() != 0 || sends.Load() != 0 || len(s.Engine.Snapshot().InClassSeen) != 0 || !reflect.DeepEqual(s.inClassSnapshot(), inClassStatus{}) {
		t.Fatal("interrupted poll lost its renewal or published partial observations")
	}
}

func TestCheckInClassPersistsFinalRenewal(t *testing.T) {
	for _, failure := range []string{"none", "lessons", "activities", "quick", "empty_lessons"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := testApp(t)
			now := time.Now()
			prepareInClass(t, s, fakeLiveRain{}, now)
			latest := ""
			var order []string
			live := fakeLiveRain{
				lessons: func(context.Context) ([]platform.LiveLesson, error) {
					order = append(order, "lessons")
					latest = "lessons-renewal"
					if failure == "lessons" {
						return nil, errors.New("fixture failure")
					}
					if failure == "empty_lessons" {
						return nil, nil
					}
					return []platform.LiveLesson{{ClassroomID: "1", LessonID: "2", CourseName: "Test"}}, nil
				},
				activities: func(context.Context, string) ([]platform.LiveActivity, error) {
					order = append(order, "activities")
					latest = "activities-renewal"
					if failure == "activities" {
						return nil, errors.New("fixture failure")
					}
					return []platform.LiveActivity{{ID: "3", Type: 5, Title: "Quiz", CreatedAt: now}}, nil
				},
				quick: func(context.Context, string) ([]platform.LiveQuickAnswer, error) {
					order = append(order, "quick")
					latest = "quick-renewal"
					if err := s.Vault.Update(func(c *vault.Config) error { c.BarkKey = "updated-bark"; return nil }); err != nil {
						return nil, err
					}
					if failure == "quick" {
						return nil, errors.New("fixture failure")
					}
					return nil, nil
				},
			}
			s.newRain = func(cookie string) rainAdapter {
				if cookie != "sessionid=test" {
					t.Errorf("unexpected initial cookie %q", cookie)
				}
				return renewingLiveRain{fakeLiveRain: live, export: func() (string, error) {
					order = append(order, "export")
					return latest, nil
				}}
			}
			s.checkInClass(context.Background())
			wantOrder := []string{"lessons", "activities", "quick", "export"}
			wantCookie, wantBark := "quick-renewal", "updated-bark"
			if failure == "lessons" || failure == "empty_lessons" {
				wantOrder = []string{"lessons", "export"}
				wantCookie, wantBark = "lessons-renewal", "test-key"
			}
			if !reflect.DeepEqual(order, wantOrder) {
				t.Fatalf("request order=%v", order)
			}
			if got := s.Vault.Get(); got.RainCookie != wantCookie || got.BarkKey != wantBark {
				t.Fatal("final renewal or concurrent Bark configuration not preserved")
			}
			status := s.inClassSnapshot()
			if (failure == "none" || failure == "empty_lessons") != (status.Error == "") {
				t.Fatalf("unexpected status=%+v", status)
			}
		})
	}
}

func TestCheckInClassRenewalFailureDoesNotCommitObservations(t *testing.T) {
	for _, failure := range []string{"export_error", "empty", "vault_write"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := testApp(t)
			directory := t.TempDir()
			var err error
			s.Vault, err = vault.Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			live := fakeLiveRain{
				lessons: func(context.Context) ([]platform.LiveLesson, error) {
					return []platform.LiveLesson{{ClassroomID: "1", CourseName: "Test"}}, nil
				},
				activities: func(context.Context, string) ([]platform.LiveActivity, error) {
					return []platform.LiveActivity{{ID: "3", Type: 5, Title: "Quiz", CreatedAt: now}}, nil
				},
			}
			sent := prepareInClass(t, s, live, now)
			before := s.Engine.Snapshot()
			var restore func()
			defer func() {
				if restore != nil {
					restore()
				}
			}()
			s.newRain = func(string) rainAdapter {
				return renewingLiveRain{fakeLiveRain: live, export: func() (string, error) {
					switch failure {
					case "export_error":
						return "", errors.New("private-cookie-diagnostic")
					case "empty":
						return " ", nil
					case "vault_write":
						restore = blockStateDirectory(t, directory)
					}
					return "renewed", nil
				}}
			}
			s.checkInClass(context.Background())
			if s.Vault.Get().RainCookie != "sessionid=test" || len(*sent) != 0 {
				t.Fatal("failed renewal changed the cookie or sent a quiz")
			}
			after := s.Engine.Snapshot()
			if !reflect.DeepEqual(after.InClassSeen, before.InClassSeen) || !reflect.DeepEqual(after.Events, before.Events) {
				t.Fatal("failed renewal committed quiz observations")
			}
			status := s.inClassSnapshot()
			if status.Error == "" || strings.Contains(status.Error, "private-cookie-diagnostic") {
				t.Fatalf("renewal error not sanitized: %+v", status)
			}
		})
	}
}

func TestCheckInClassRebindThroughLogin(t *testing.T) {
	for _, outcome := range []string{"complete", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			s, h := testApp(t)
			admin := loginCookie(t, h)
			now := time.Now()
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			live := fakeLiveRain{
				lessons: func(context.Context) ([]platform.LiveLesson, error) {
					return []platform.LiveLesson{{ClassroomID: "old-class", CourseName: "Old"}}, nil
				},
				activities: func(context.Context, string) ([]platform.LiveActivity, error) {
					close(entered)
					<-release
					return []platform.LiveActivity{{ID: "old-quiz", Type: 5, Title: "Old", CreatedAt: now}}, nil
				},
			}
			prepareInClass(t, s, live, now)
			var sends atomic.Int32
			s.sendBark = func(context.Context, string, string, string, string, bark.Options) error { sends.Add(1); return nil }
			var adapters atomic.Int32
			s.newRain = func(string) rainAdapter {
				if adapters.Add(1) == 1 {
					return renewingLiveRain{fakeLiveRain: live, export: func() (string, error) { return "old-renewal", nil }}
				}
				return fakeRainAdapter{}
			}
			loginRelease := make(chan struct{})
			s.startRain = func(context.Context) (*rainFlow, error) {
				return &rainFlow{image: []byte("fixture"), wait: func(ctx context.Context) (string, error) {
					select {
					case <-loginRelease:
						return "new-binding", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}}, nil
			}
			go func() { defer close(done); s.checkInClass(context.Background()) }()
			await(t, entered)
			response := request(h, "POST", "/api/accounts/rain/login", "", admin, "")
			if response.Code != 200 {
				close(release)
				await(t, done)
				t.Fatalf("login start status=%d body=%s", response.Code, response.Body.String())
			}
			if outcome == "complete" {
				close(loginRelease)
				eventually(t, func() bool { return s.Vault.Get().RainCookie == "new-binding" && rainState(s) == "logged_in" })
			} else {
				response = request(h, "POST", "/api/accounts/rain/login/cancel", "", admin, "")
				if response.Code != 200 {
					close(release)
					await(t, done)
					t.Fatalf("login cancel status=%d body=%s", response.Code, response.Body.String())
				}
			}
			close(release)
			await(t, done)
			wantCookie := "sessionid=test"
			if outcome == "complete" {
				wantCookie = "new-binding"
			}
			if s.Vault.Get().RainCookie != wantCookie || len(s.Engine.Snapshot().InClassSeen) != 0 || sends.Load() != 0 || !reflect.DeepEqual(s.inClassSnapshot(), inClassStatus{}) {
				t.Fatal("previous-generation poll contaminated the current binding")
			}
		})
	}
}

func TestCheckInClassRevalidatesQueuedImmediateDelivery(t *testing.T) {
	for _, reason := range []string{"generation", "cookie", "disabled", "canceled", "shutdown"} {
		t.Run(reason, func(t *testing.T) {
			s, _ := testApp(t)
			now := time.Now()
			live := fakeLiveRain{
				lessons: func(context.Context) ([]platform.LiveLesson, error) {
					return []platform.LiveLesson{{ClassroomID: "1", CourseName: "Test"}}, nil
				},
				activities: func(context.Context, string) ([]platform.LiveActivity, error) {
					return []platform.LiveActivity{{ID: "3", Type: 5, Title: "Quiz", CreatedAt: now}}, nil
				},
			}
			prepareInClass(t, s, live, now)
			var sends atomic.Int32
			s.sendBark = func(context.Context, string, string, string, string, bark.Options) error { sends.Add(1); return nil }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			s.notifyMu.Lock()
			locked := true
			defer func() {
				if locked {
					s.notifyMu.Unlock()
				}
			}()
			go func() { defer close(done); s.checkInClass(ctx) }()
			eventually(t, func() bool { return s.inClassSnapshot().Detected == 1 })
			switch reason {
			case "generation":
				s.rainMu.Lock()
				s.rainGeneration++
				s.rainMu.Unlock()
			case "cookie":
				s.rainMu.Lock()
				err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "replacement"; return nil })
				s.rainMu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "disabled":
				settings := s.Engine.Snapshot().Settings
				settings.InClassEnabled = false
				if err := s.Engine.UpdateSettings(settings); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			case "shutdown":
				if err := s.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			s.notifyMu.Unlock()
			locked = false
			await(t, done)
			events := s.Engine.Snapshot().Events
			if sends.Load() != 0 || len(events) != 1 || events[0].Status != "pending" || events[0].Attempts != 0 {
				t.Fatalf("superseded delivery was sent or claimed: sends=%d events=%+v", sends.Load(), events)
			}
		})
	}
}

func TestCheckInClassRevalidatesBetweenTransports(t *testing.T) {
	for _, reason := range []string{"generation", "cookie", "disabled", "canceled", "shutdown"} {
		t.Run(reason, func(t *testing.T) {
			s, _ := testApp(t)
			now := time.Now()
			live := fakeLiveRain{
				lessons: func(context.Context) ([]platform.LiveLesson, error) {
					return []platform.LiveLesson{{ClassroomID: "1", CourseName: "Test"}}, nil
				},
				activities: func(context.Context, string) ([]platform.LiveActivity, error) {
					return []platform.LiveActivity{
						{ID: "1", Type: 5, Title: "First", CreatedAt: now},
						{ID: "2", Type: 5, Title: "Second", CreatedAt: now},
					}, nil
				},
			}
			prepareInClass(t, s, live, now)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var sends atomic.Int32
			s.sendBark = func(context.Context, string, string, string, string, bark.Options) error {
				if sends.Add(1) == 1 {
					close(entered)
					<-release
				}
				return nil // Simulate a transport which has already accepted the push.
			}
			go func() { defer close(done); s.checkInClass(ctx) }()
			await(t, entered)
			switch reason {
			case "generation":
				s.rainMu.Lock()
				s.rainGeneration++
				s.rainMu.Unlock()
			case "cookie":
				s.rainMu.Lock()
				err := s.Vault.Update(func(c *vault.Config) error { c.RainCookie = "replacement"; return nil })
				s.rainMu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "disabled":
				settings := s.Engine.Snapshot().Settings
				settings.InClassEnabled = false
				if err := s.Engine.UpdateSettings(settings); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			case "shutdown":
				if err := s.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			await(t, done)
			events := s.Engine.Snapshot().Events
			var sent, pending int
			for _, event := range events {
				switch event.Status {
				case "sent":
					sent++
				case "pending":
					pending++
				}
			}
			if sends.Load() != 1 || sent != 1 || pending != 1 {
				t.Fatalf("transport was retried or continued after supersession: sends=%d events=%+v", sends.Load(), events)
			}
		})
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
