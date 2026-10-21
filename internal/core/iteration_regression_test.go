package core

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInvalidTimestampCannotReplaceDurableState(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	task := homework("existing", now.Add(time.Hour))
	mustSync(t, e, []Task{task}, now)
	before, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	bad := homework("bad-date", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := e.Sync(PlatformRain, []Task{bad}, now); err == nil {
		t.Error("invalid timestamp was accepted")
	}
	after, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("rejected snapshot replaced the persisted state")
	}
	if got := e.Snapshot(); got.Version != 1 || len(got.Tasks) != 1 || got.Tasks[0].ID != "existing" {
		t.Error("invalid timestamp destroyed the in-memory state")
	}
	if _, err := Open(e.path); err != nil {
		t.Errorf("previous valid state no longer reopens: %v", err)
	}
}

func TestStaleSnapshotRecoveryCollapsesMissedReminders(t *testing.T) {
	e := openTest(t)
	seed := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, seed)
	task := homework("one", seed.Add(48*time.Hour))
	mustSync(t, e, []Task{task}, seed)
	completeEvents(t, e, seed)
	recovered := task.EndAt.Add(-15 * time.Minute)
	if got := e.Pending(recovered); len(got) != 0 {
		t.Fatal("stale state was delivered before validation")
	}
	// A suspended device or scheduler can miss sync rounds without reporting
	// an explicit Fail. Successful validation still has recovery semantics.
	mustSync(t, e, []Task{task}, recovered)
	got := e.Pending(recovered)
	if len(got) != 1 || got[0].Kind != "deadline" || got[0].MinutesBefore != 30 {
		t.Fatalf("recovery claimed %d reminders instead of the latest 30-minute event", len(got))
	}
}

func TestDeliveryUsesLatestTaskDetailsAndTimezone(t *testing.T) {
	e := openTest(t)
	seed := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, seed)
	task := homework("exam", seed.Add(2*time.Hour))
	task.Kind = "exam"
	task.StartAt = timePtr(seed.Add(time.Hour))
	mustSync(t, e, []Task{task}, seed)
	completeEvents(t, e, seed)
	task.Title, task.Course = "Updated exam", "Updated course"
	task.StartAt = timePtr(seed.Add(75 * time.Minute))
	mustSync(t, e, []Task{task}, seed.Add(10*time.Minute))
	completeEvents(t, e, seed.Add(15*time.Minute))
	settings := e.Snapshot().Settings
	settings.Timezone = "UTC"
	if err := e.UpdateSettingsAt(settings, seed.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	due := task.EndAt.Add(-30 * time.Minute)
	mustSync(t, e, []Task{task}, due.Add(-5*time.Minute))
	got := e.Pending(due)
	if len(got) != 1 || got[0].Kind != "deadline" {
		t.Fatalf("expected the unchanged deadline event, got %+v", got)
	}
	if !strings.Contains(got[0].Title, task.Title) || got[0].Body != taskBody(task, settings) {
		t.Fatal("notification contains outdated task metadata or timezone")
	}
}
