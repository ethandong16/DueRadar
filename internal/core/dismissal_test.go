package core

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestDismissOverdueTaskStopsRemindersUntilRestored(t *testing.T) {
	e := openTest(t)
	discovered := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, discovered)
	mustSync(t, e, []Task{homework("late", discovered.Add(time.Hour))}, discovered)
	// Two hours later the platform still lists the work, but it can no longer
	// be submitted. Dismissal must cancel anything still queued for it.
	now := discovered.Add(2 * time.Hour)
	mustSync(t, e, []Task{homework("late", discovered.Add(time.Hour))}, now)
	if err := e.SetTaskDismissed(PlatformRain, "late", true, now); err != nil {
		t.Fatal(err)
	}
	task := e.Snapshot().Tasks[0]
	if task.DismissedAt == nil {
		t.Fatal("dismissal was not recorded")
	}
	if got := e.Pending(now); len(got) != 0 {
		t.Fatalf("dismissed task still has pending reminders: %+v", got)
	}
	if err := e.SetTaskDismissed(PlatformRain, "late", false, now); err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().Tasks[0].DismissedAt != nil {
		t.Fatal("restore did not clear dismissal")
	}
}

func TestSyncPreservesDismissalUntilTheDeadlineMoves(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	past := now.Add(-time.Hour)
	mustSync(t, e, []Task{homework("late", past)}, now)
	if err := e.SetTaskDismissed(PlatformRain, "late", true, now); err != nil {
		t.Fatal(err)
	}
	// An unchanged snapshot must not undo the user's decision.
	mustSync(t, e, []Task{homework("late", past)}, now.Add(time.Minute))
	if e.Snapshot().Tasks[0].DismissedAt == nil {
		t.Fatal("ordinary sync cleared dismissal")
	}
	// A teacher extending the deadline makes the work actionable again.
	extended := now.Add(2 * time.Hour)
	mustSync(t, e, []Task{homework("late", extended)}, now.Add(2*time.Minute))
	if e.Snapshot().Tasks[0].DismissedAt != nil {
		t.Fatal("extended deadline did not restore reminders")
	}
}

func TestDismissRejectsWorkThatCanStillBeSubmitted(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	mustSync(t, e, []Task{
		homework("future", now.Add(time.Hour)),
		{ID: "done", Title: "done", Kind: "homework", Status: "completed", EndAt: timePtr(now.Add(-time.Hour))},
		{ID: "unknown", Title: "unknown", Kind: "homework", Status: "unknown", EndAt: timePtr(now.Add(-time.Hour))},
		{ID: "undated", Title: "undated", Kind: "homework", Status: "pending"},
	}, now)
	for _, id := range []string{"future", "done", "unknown", "undated", "missing"} {
		if err := e.SetTaskDismissed(PlatformRain, id, true, now); err == nil {
			t.Fatalf("%s should not be dismissible", id)
		}
	}
}

func TestDailyStateBackupRefreshesAfterAChange(t *testing.T) {
	e := openTest(t)
	backup := e.path + ".bak"
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("startup backup missing: %v", err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(backup, old, old); err != nil {
		t.Fatal(err)
	}
	settings := e.Snapshot().Settings
	settings.Timezone = "Asia/Tokyo"
	if err := e.UpdateSettingsAt(settings, time.Now()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("backup was not refreshed: %v", info.ModTime())
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("Asia/Tokyo")) {
		t.Fatal("backup does not contain the committed state")
	}
}

func TestProviderRetryAfterExtendsButNeverShortensBackoff(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	settings := e.Snapshot().Settings
	settings.DeadlineMinutes = []int{30}
	if err := e.UpdateSettingsAt(settings, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustSync(t, e, []Task{homework("soon", now.Add(30*time.Minute))}, now.Add(-time.Minute))
	claims, err := e.PendingOneWithError(now)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %+v %v", claims, err)
	}
	if err := e.MarkFailedClaimAfter(claims[0], "transport", 10*time.Minute, now); err != nil {
		t.Fatal(err)
	}
	for _, event := range e.Snapshot().Events {
		if event.ID != claims[0].ID {
			continue
		}
		if event.NextAttemptAt == nil || !event.NextAttemptAt.Equal(now.Add(10*time.Minute)) {
			t.Fatalf("provider delay not honored: %+v", event.NextAttemptAt)
		}
		if event.BlockedReason != BlockedRetryBackoff {
			t.Fatalf("retry state not explained: %+v", event)
		}
	}
}

func TestSerialSenderCollapsesBackloggedRemindersPerTask(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	settings := e.Snapshot().Settings
	settings.DeadlineMinutes = []int{30}
	if err := e.UpdateSettingsAt(settings, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The task is discovered one minute before its 30-minute reminder fires, so
	// the discovery event and the scheduled reminder are due together.
	mustSync(t, e, []Task{homework("soon", now.Add(30*time.Minute))}, now.Add(-time.Minute))
	claims, err := e.PendingOneWithError(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Kind != "deadline" {
		t.Fatalf("expected only the scheduled reminder, got %+v", claims)
	}
	for _, event := range e.Snapshot().Events {
		if event.Kind == "new_task" {
			if event.Status != "canceled" || event.LastError != "已合并为更近的一条提醒" {
				t.Fatalf("discovery event was not collapsed: %+v", event)
			}
		}
	}
	if next, err := e.PendingOneWithError(now); err != nil || len(next) != 0 {
		t.Fatalf("collapsed backlog should drain in one send: %+v %v", next, err)
	}
}
