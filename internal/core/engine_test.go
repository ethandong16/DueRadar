package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func stamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
func at(s string) *time.Time { t := stamp(s); return &t }
func openTest(t *testing.T) *Engine {
	t.Helper()
	e, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func mustSync(t *testing.T, e *Engine, tasks []Task, now time.Time) {
	t.Helper()
	if err := e.Sync(PlatformRain, tasks, now); err != nil {
		t.Fatal(err)
	}
}
func completeEvents(t *testing.T, e *Engine, now time.Time) []Event {
	t.Helper()
	events := e.Pending(now)
	for _, ev := range events {
		if err := e.MarkSent(ev.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	return events
}
func homework(id string, end time.Time) Task {
	return Task{ID: id, Title: "任务 " + id, Course: "测试课程", Kind: "homework", Status: "pending", EndAt: timePtr(end)}
}
func withoutQuiet(t *testing.T, e *Engine, now time.Time) {
	t.Helper()
	s := e.Snapshot().Settings
	s.QuietEnabled = false
	if err := e.UpdateSettingsAt(s, now); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultSettingsAndQuietBoundaries(t *testing.T) {
	s := DefaultSettings()
	if fmt.Sprint(s.DeadlineMinutes) != "[1440 180 30]" || fmt.Sprint(s.ExamStartMinutes) != "[1440 60]" || fmt.Sprint(s.VideoMinutes) != "[4320 1440 180]" {
		t.Fatal(s)
	}
	for _, tc := range []struct {
		clock string
		want  bool
	}{{"22:59", false}, {"23:00", true}, {"00:00", true}, {"07:59", true}, {"08:00", false}} {
		now := stamp("2026-10-21T" + tc.clock + ":00+08:00")
		if got := IsQuiet(s, now.UTC()); got != tc.want {
			t.Errorf("%s quiet=%v", tc.clock, got)
		}
	}
	s.QuietStart = "21:30"
	s.QuietEnd = "06:15"
	if !IsQuiet(s, stamp("2026-10-22T06:14:00+08:00")) || IsQuiet(s, stamp("2026-10-22T06:15:00+08:00")) {
		t.Fatal("custom cross-midnight boundary")
	}
	s.QuietStart = "12:00"
	s.QuietEnd = "13:00"
	if !IsQuiet(s, stamp("2026-10-22T12:30:00+08:00")) || IsQuiet(s, stamp("2026-10-22T23:00:00+08:00")) {
		t.Fatal("same-day quiet boundary")
	}
}

func TestFirstSyncPerTaskAndPersistentDedup(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	tasks := []Task{homework("one", now.Add(48*time.Hour)), homework("two", now.Add(49*time.Hour))}
	mustSync(t, e, tasks, now)
	if got := completeEvents(t, e, now); len(got) != 2 || got[0].TaskID == got[1].TaskID {
		t.Fatalf("expected two distinct task notifications: %+v", got)
	}
	mustSync(t, e, tasks, now.Add(time.Minute))
	if got := e.Pending(now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("duplicate discovery: %+v", got)
	}
	if err := e.save(e.Snapshot()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Pending(now.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("restart sent without sync: %+v", got)
	}
	mustSync(t, reopened, tasks, now.Add(time.Hour))
	if got := reopened.Pending(now.Add(time.Hour)); len(got) != 0 {
		t.Fatal("restart repeated initial list")
	}
}

func TestExactUrgentQuietExceptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		task    Task
		now     time.Time
		kind    string
		minutes int
	}{
		{"exam", Task{ID: "exam", Title: "考试", Kind: "exam", Status: "pending", StartAt: at("2026-10-22T07:00:00+08:00"), EndAt: at("2026-10-22T09:00:00+08:00")}, stamp("2026-10-22T06:00:00+08:00"), "exam_start", 60},
		{"deadline", homework("hw", stamp("2026-10-22T00:00:00+08:00")), stamp("2026-10-21T23:30:00+08:00"), "deadline", 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := openTest(t)
			seed := stamp("2026-10-21T10:00:00+08:00")
			mustSync(t, e, []Task{tc.task}, seed)
			completeEvents(t, e, seed)
			mustSync(t, e, []Task{tc.task}, tc.now.Add(-5*time.Minute))
			got := e.Pending(tc.now)
			if len(got) != 1 || got[0].Kind != tc.kind || got[0].MinutesBefore != tc.minutes {
				t.Fatalf("expected exact urgent reminder, got %+v", got)
			}
			if second := e.Pending(tc.now); len(second) != 0 {
				t.Fatal("claimed event delivered twice")
			}
			if err := e.MarkFailed(got[0].ID, "private transport URL", tc.now); err != nil {
				t.Fatal(err)
			}
			if retry := e.Pending(tc.now.Add(time.Minute)); len(retry) != 1 {
				t.Fatalf("exact urgent delayed retry should bypass quiet: %+v", retry)
			}
		})
	}
}

func TestCustomOffsetsDoNotCreateOrInheritUrgency(t *testing.T) {
	e := openTest(t)
	seed := stamp("2026-10-21T10:00:00+08:00")
	s := DefaultSettings()
	s.DeadlineMinutes = []int{45}
	s.ExamStartMinutes = []int{90}
	if err := e.UpdateSettingsAt(s, seed); err != nil {
		t.Fatal(err)
	}
	tasks := []Task{homework("hw", stamp("2026-10-22T00:00:00+08:00")), {ID: "exam", Title: "考试", Kind: "exam", Status: "pending", StartAt: at("2026-10-22T07:00:00+08:00"), EndAt: at("2026-10-22T09:00:00+08:00")}}
	mustSync(t, e, tasks, seed)
	completeEvents(t, e, seed)
	now := stamp("2026-10-21T23:30:00+08:00")
	mustSync(t, e, tasks, now)
	if got := e.Pending(now); len(got) != 0 {
		t.Fatalf("45-minute event inherited urgency: %+v", got)
	}
	now = stamp("2026-10-22T06:00:00+08:00")
	mustSync(t, e, tasks, now)
	if got := e.Pending(now); len(got) != 0 {
		t.Fatalf("90-minute event inherited urgency: %+v", got)
	}
	for _, ev := range e.Snapshot().Events {
		if ev.MinutesBefore == 30 || ev.MinutesBefore == 60 {
			t.Fatal("unconfigured urgent event invented")
		}
	}
}

func TestVideoScheduleAndUnknownTimes(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	video := homework("video", now.Add(5*24*time.Hour))
	video.Kind = "video"
	unknown := homework("unknown", now.Add(time.Hour))
	unknown.Status = "unknown"
	noTime := homework("no-time", now.Add(time.Hour))
	noTime.EndAt = nil
	mustSync(t, e, []Task{video, unknown, noTime}, now)
	n := 0
	for _, ev := range e.Snapshot().Events {
		if ev.TaskID == "video" && ev.Kind == "video_deadline" {
			n++
			if ev.MinutesBefore == 30 {
				t.Fatal("video invented 30-minute event")
			}
		}
		if ev.TaskID == "unknown" {
			t.Fatal("unknown task generated reminder")
		}
		if ev.TaskID == "no-time" && scheduled(ev.Kind) {
			t.Fatal("unknown date generated reminder")
		}
	}
	if n != 3 {
		t.Fatalf("video reminders=%d", n)
	}
}

func TestCompletionUnknownAndOmissionSuppress(t *testing.T) {
	for _, state := range []string{"completed", "unknown", "omitted"} {
		t.Run(state, func(t *testing.T) {
			e := openTest(t)
			now := stamp("2026-10-21T10:00:00+08:00")
			task := homework("one", now.Add(time.Hour))
			mustSync(t, e, []Task{task}, now)
			completeEvents(t, e, now)
			if state == "omitted" {
				mustSync(t, e, nil, now.Add(30*time.Minute))
			} else {
				task.Status = state
				mustSync(t, e, []Task{task}, now.Add(30*time.Minute))
			}
			if got := e.Pending(now.Add(30 * time.Minute)); len(got) != 0 {
				t.Fatalf("invalid reminder %+v", got)
			}
			if len(e.Snapshot().Tasks) != 1 {
				t.Fatal("task history lost")
			}
		})
	}
}

func TestRecoveryRevalidatesAndCollapsesMissed(t *testing.T) {
	e := openTest(t)
	seed := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, seed)
	task := homework("one", seed.Add(48*time.Hour))
	mustSync(t, e, []Task{task}, seed)
	completeEvents(t, e, seed)
	recovered := seed.Add(47 * time.Hour)
	if err := e.RequireRevalidation(); err != nil {
		t.Fatal(err)
	}
	if got := e.Pending(recovered); len(got) != 0 {
		t.Fatal("unverified recovery sent task")
	}
	mustSync(t, e, []Task{task}, recovered)
	got := e.Pending(recovered)
	if len(got) != 1 || got[0].MinutesBefore != 180 {
		t.Fatalf("expected most recent 3-hour reminder only: %+v", got)
	}
	if err := e.MarkSent(got[0].ID, recovered); err != nil {
		t.Fatal(err)
	}
	if got = e.Pending(recovered); len(got) != 0 {
		t.Fatal("recovery duplicated")
	}
	// Future events survive catch-up collapse and run independently of sync.
	mustSync(t, e, []Task{task}, recovered.Add(20*time.Minute))
	got = e.Pending(recovered.Add(30 * time.Minute))
	if len(got) != 1 || got[0].MinutesBefore != 30 {
		t.Fatalf("future event lost: %+v", got)
	}
}

func TestRecoveryDoesNotSendStartedExamOrCompletedTask(t *testing.T) {
	e := openTest(t)
	seed := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, seed)
	exam := Task{ID: "exam", Title: "考试", Kind: "exam", Status: "pending", StartAt: timePtr(seed.Add(48 * time.Hour)), EndAt: timePtr(seed.Add(52 * time.Hour))}
	completed := homework("completed", seed.Add(50*time.Hour))
	mustSync(t, e, []Task{exam, completed}, seed)
	completeEvents(t, e, seed)
	if err := e.RequireRevalidation(); err != nil {
		t.Fatal(err)
	}
	completed.Status = "completed"
	now := seed.Add(49 * time.Hour)
	mustSync(t, e, []Task{exam, completed}, now)
	for _, ev := range e.Pending(now) {
		if ev.Kind == "exam_start" || ev.TaskID == "completed" {
			t.Fatalf("invalid catch-up: %+v", ev)
		}
	}
}

func TestQuietExpiryAndRecoveryUseOneEvent(t *testing.T) {
	e := openTest(t)
	seed := stamp("2026-10-21T20:00:00+08:00")
	task := homework("one", stamp("2026-10-22T10:00:00+08:00"))
	mustSync(t, e, []Task{task}, seed)
	completeEvents(t, e, seed)
	now := stamp("2026-10-22T07:00:00+08:00")
	mustSync(t, e, []Task{task}, now)
	if got := e.Pending(now); len(got) != 0 {
		t.Fatal("quiet reminder sent")
	}
	if err := e.RequireRevalidation(); err != nil {
		t.Fatal(err)
	}
	now = stamp("2026-10-22T08:00:00+08:00")
	mustSync(t, e, []Task{task}, now)
	got := completeEvents(t, e, now)
	if len(got) != 1 || got[0].MinutesBefore != 180 {
		t.Fatalf("quiet/recovery duplicates: %+v", got)
	}
	if got = e.Pending(now); len(got) != 0 {
		t.Fatal("repeated event")
	}
}

func TestTimeChangeInvalidatesOldAnchorButKeepsUnchangedDeadline(t *testing.T) {
	e := openTest(t)
	seed := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, seed)
	task := Task{ID: "exam", Title: "考试", Kind: "exam", Status: "pending", StartAt: timePtr(seed.Add(2 * time.Hour)), EndAt: timePtr(seed.Add(4 * time.Hour))}
	mustSync(t, e, []Task{task}, seed)
	completeEvents(t, e, seed)
	task.StartAt = timePtr(seed.Add(3 * time.Hour))
	now := seed.Add(30 * time.Minute)
	mustSync(t, e, []Task{task}, now)
	got := completeEvents(t, e, now)
	if len(got) != 1 || got[0].Kind != "time_changed" {
		t.Fatalf("missing change: %+v", got)
	}
	now = seed.Add(time.Hour)
	mustSync(t, e, []Task{task}, now)
	got = e.Pending(now)
	if len(got) != 1 || got[0].Kind != "deadline" || got[0].MinutesBefore != 180 {
		t.Fatalf("unchanged deadline lost or old start retained: %+v", got)
	}
}

func TestFaultEpisodesAndFreshnessGate(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	task := homework("one", now.Add(time.Hour))
	mustSync(t, e, []Task{task}, now)
	completeEvents(t, e, now)
	for round := 1; round <= 4; round++ {
		err := e.Fail(PlatformRain, false, "https://secret/token", now.Add(time.Duration(round)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		got := completeEvents(t, e, now.Add(time.Duration(round)*time.Minute))
		want := 0
		if round == 3 {
			want = 1
		}
		if len(got) != want {
			t.Fatalf("round %d events=%+v", round, got)
		}
	}
	if got := e.Pending(now.Add(30 * time.Minute)); len(got) != 0 {
		t.Fatalf("stale task reminder %+v", got)
	}
	mustSync(t, e, []Task{task}, now.Add(31*time.Minute))
	completeEvents(t, e, now.Add(31*time.Minute))
	if err := e.Fail(PlatformRain, true, "private credential", now.Add(32*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := completeEvents(t, e, now.Add(32*time.Minute))
	if len(got) != 1 || got[0].Kind != "fault" {
		t.Fatal("new expired-login episode missing")
	}
	data, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret/token") || strings.Contains(string(data), "private credential") {
		t.Fatal("untrusted diagnostic leaked secret")
	}
}

func TestFreshnessExpiresWithoutFailureReport(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	task := homework("one", now.Add(2*time.Hour))
	mustSync(t, e, []Task{task}, now)
	completeEvents(t, e, now)
	if got := e.Pending(now.Add(90 * time.Minute)); len(got) != 0 {
		t.Fatal("stale snapshot sent deadline")
	}
}

func TestDeliveryRetryBoundedAndDurable(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	task := homework("one", now.Add(48*time.Hour))
	mustSync(t, e, []Task{task}, now)
	id := ""
	for attempt := 1; attempt <= MaxDeliveryAttempts; attempt++ {
		mustSync(t, e, []Task{task}, now)
		got := e.Pending(now)
		if len(got) != 1 {
			t.Fatalf("attempt %d missing: %+v", attempt, got)
		}
		if id != "" && got[0].ID != id {
			t.Fatal("retry created new event")
		}
		id = got[0].ID
		if got[0].Attempts != attempt {
			t.Fatalf("attempt count %d", got[0].Attempts)
		}
		if err := e.MarkFailed(id, "https://bark/private-key", now); err != nil {
			t.Fatal(err)
		}
		if got := e.Pending(now.Add(30 * time.Second)); len(got) != 0 {
			t.Fatal("retry ignored backoff")
		}
		now = now.Add(time.Minute * time.Duration(1<<uint(attempt-1)))
	}
	mustSync(t, e, []Task{task}, now)
	if got := e.Pending(now); len(got) != 0 {
		t.Fatal("retry exceeded cap")
	}
	for _, ev := range e.Snapshot().Events {
		if ev.ID == id && ev.Status != "failed" {
			t.Fatal("exhausted event not failed")
		}
	}
}

func TestSettingsRemovalAndReadd(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	task := homework("one", now.Add(time.Hour))
	mustSync(t, e, []Task{task}, now)
	completeEvents(t, e, now)
	s := e.Snapshot().Settings
	s.DeadlineMinutes = []int{20}
	if err := e.UpdateSettingsAt(s, now); err != nil {
		t.Fatal(err)
	}
	s.DeadlineMinutes = []int{30}
	if err := e.UpdateSettingsAt(s, now); err != nil {
		t.Fatal(err)
	}
	mustSync(t, e, []Task{task}, now.Add(30*time.Minute))
	got := e.Pending(now.Add(30 * time.Minute))
	if len(got) != 1 || got[0].MinutesBefore != 30 {
		t.Fatalf("re-added offset not scheduled: %+v", got)
	}
}

func TestStateIsolationValidationAndFailedCommit(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	task := homework("one", now.Add(time.Hour))
	mustSync(t, e, []Task{task}, now)
	*task.EndAt = now.Add(10 * time.Hour)
	copy := e.Snapshot()
	*copy.Tasks[0].EndAt = now.Add(20 * time.Hour)
	copy.Settings.DeadlineMinutes[0] = 999
	if !e.Snapshot().Tasks[0].EndAt.Equal(now.Add(time.Hour)) || e.Snapshot().Settings.DeadlineMinutes[0] != 1440 {
		t.Fatal("snapshot or input aliases internal state")
	}
	s := e.Snapshot().Settings
	s.QuietStart = "25:00"
	if e.UpdateSettingsAt(s, now) == nil {
		t.Fatal("invalid quiet settings accepted")
	}
	if e.Sync(PlatformRain, []Task{task, task}, now) == nil {
		t.Fatal("duplicate task IDs accepted")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	e.path = filepath.Join(blocked, "state.json")
	s = e.Snapshot().Settings
	s.QuietEnabled = false
	if e.UpdateSettingsAt(s, now) == nil {
		t.Fatal("failed disk commit was accepted")
	}
	if !e.Snapshot().Settings.QuietEnabled {
		t.Fatal("failed disk commit mutated in-memory state")
	}
}

func TestConcurrentSnapshotsAndSync(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if n == 0 {
					task := homework("one", now.Add(48*time.Hour))
					if err := e.Sync(PlatformRain, []Task{task}, now); err != nil {
						t.Error(err)
					}
				} else {
					_ = e.Snapshot()
				}
			}
		}(n)
	}
	wg.Wait()
}

func TestClaimedEventsDoNotAliasPersistentState(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	task := homework("one", now.Add(time.Hour))
	mustSync(t, e, []Task{task}, now)
	claimed := e.Pending(now)
	if len(claimed) != 1 {
		t.Fatal("missing discovery")
	}
	*claimed[0].NextAttemptAt = now.Add(-time.Hour)
	if got := e.Pending(now); len(got) != 0 {
		t.Fatal("caller mutated persisted delivery lease")
	}
}

func TestPendingWithErrorDoesNotHideFailedClaimCommit(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	task := homework("one", now.Add(time.Hour))
	mustSync(t, e, []Task{task}, now)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	e.path = filepath.Join(blocked, "state.json")
	events, err := e.PendingWithError(now)
	if err == nil || len(events) != 0 {
		t.Fatal("failed claim persistence was hidden")
	}
	for _, event := range e.Snapshot().Events {
		if event.Attempts != 0 || event.Status == "sending" {
			t.Fatal("failed durable claim changed in-memory state")
		}
	}
}
