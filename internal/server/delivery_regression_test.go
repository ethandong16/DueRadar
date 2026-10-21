package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

type notificationHealthStatus struct {
	Error       string     `json:"notification_error"`
	Failures    int        `json:"notification_failure_count"`
	LastAttempt *time.Time `json:"notification_last_attempt_at"`
	LastSuccess *time.Time `json:"notification_last_success_at"`
}

func readNotificationHealth(t *testing.T, s *Server) notificationHealthStatus {
	t.Helper()
	recorder := httptest.NewRecorder()
	s.status(recorder, httptest.NewRequest("GET", "/api/status", nil))
	var status notificationHealthStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestNotificationHealthClearsWhenLastFailureBecomesIrrelevant(t *testing.T) {
	for _, outcome := range []string{"all-failed", "last-failed"} {
		for _, resolution := range []string{"completed", "expired"} {
			t.Run(outcome+"/"+resolution, func(t *testing.T) {
				s, _ := deliveryApp(t)
				calls := 0
				s.sendBark = func(context.Context, string, string, string, string) error {
					calls++
					if outcome == "last-failed" && calls == 1 {
						return nil
					}
					return errors.New("private-provider-error")
				}
				s.Deliver(context.Background())
				wantFailures := 2
				if outcome == "last-failed" {
					wantFailures = 1
				}
				before := readNotificationHealth(t, s)
				if calls != 2 || before.Failures != wantFailures || before.Error == "" || before.LastAttempt == nil || strings.Contains(before.Error, "private-provider") {
					t.Fatalf("failed delivery was not surfaced: calls=%d status=%+v", calls, before)
				}
				if (before.LastSuccess != nil) != (outcome == "last-failed") {
					t.Fatalf("success history does not match actual transport: %+v", before)
				}
				tasks := s.Engine.Snapshot().Tasks
				past := time.Now().Add(-time.Minute)
				for i := range tasks {
					if resolution == "completed" {
						tasks[i].Status = "completed"
					} else {
						tasks[i].EndAt = &past
					}
				}
				if err := s.Engine.Sync(core.PlatformRain, tasks, time.Now()); err != nil {
					t.Fatal(err)
				}
				// Status must clear immediately and remain clear after an idle tick.
				for tick := 0; tick < 2; tick++ {
					after := readNotificationHealth(t, s)
					if after.Failures != 0 || after.Error != "" || calls != 2 {
						t.Fatalf("resolved reminder retained a failure: tick=%d calls=%d status=%+v", tick, calls, after)
					}
					if after.LastAttempt == nil || !after.LastAttempt.Equal(*before.LastAttempt) || (after.LastSuccess == nil) != (before.LastSuccess == nil) || before.LastSuccess != nil && !after.LastSuccess.Equal(*before.LastSuccess) {
						t.Fatalf("resolution erased actual delivery history: before=%+v after=%+v", before, after)
					}
					s.Deliver(context.Background())
				}
			})
		}
	}
}

func TestNotificationHealthClearsAfterReminderRuleRemoval(t *testing.T) {
	s, _ := deliveryApp(t)
	settings := s.Engine.Snapshot().Settings
	settings.DeadlineMinutes = []int{60}
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	// Schedule the 60-minute reminders from an earlier observation so they are
	// already due, without sleeps or changing the server's clock.
	if err := s.Engine.Sync(core.PlatformRain, s.Engine.Snapshot().Tasks, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	calls, failed := 0, 0
	s.sendBark = func(_ context.Context, _, _, title, _ string) error {
		calls++
		if strings.HasPrefix(title, "任务即将截止") {
			failed++
			return errors.New("private-provider-error")
		}
		return nil
	}
	s.Deliver(context.Background())
	before := readNotificationHealth(t, s)
	if calls != 4 || failed != 2 || before.Failures != 2 || before.Error == "" {
		t.Fatalf("scheduled failures were not exercised: calls=%d failed=%d status=%+v", calls, failed, before)
	}
	settings.DeadlineMinutes = nil
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	for tick := 0; tick < 2; tick++ {
		after := readNotificationHealth(t, s)
		if after.Failures != 0 || after.Error != "" || calls != 4 {
			t.Fatalf("removed rule retained a failure: tick=%d calls=%d status=%+v", tick, calls, after)
		}
		s.Deliver(context.Background())
	}
}

func TestNotificationHealthPreservesUnpersistedOutcomeAfterCompletion(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed-transport", true: "accepted-transport"}[sent], func(t *testing.T) {
			s, dir := deliveryApp(t)
			calls := 0
			var restore func()
			t.Cleanup(func() {
				if restore != nil {
					restore()
				}
			})
			s.sendBark = func(context.Context, string, string, string, string) error {
				calls++
				tasks := s.Engine.Snapshot().Tasks
				for i := range tasks {
					tasks[i].Status = "completed"
				}
				if err := s.Engine.Sync(core.PlatformRain, tasks, time.Now()); err != nil {
					t.Fatal(err)
				}
				restore = blockStateDirectory(t, dir)
				if !sent {
					return errors.New("private-provider-error")
				}
				return nil
			}
			s.Deliver(context.Background())
			for tick := 0; tick < 2; tick++ {
				status := readNotificationHealth(t, s)
				if status.Failures != 0 || !strings.Contains(status.Error, "通知结果保存失败") || strings.Contains(status.Error, dir) || strings.Contains(status.Error, "private-provider") || s.deliveryResult == nil || calls != 1 {
					t.Fatalf("unpersisted outcome was hidden or sent again: tick=%d calls=%d status=%+v", tick, calls, status)
				}
				s.Deliver(context.Background())
			}
			restore()
			restore = nil
			s.Deliver(context.Background())
			status := readNotificationHealth(t, s)
			if status.Error != "" || status.Failures != 0 || status.LastAttempt == nil || (status.LastSuccess != nil) != sent || s.deliveryResult != nil || calls != 1 {
				t.Fatalf("outcome recovery failed: calls=%d status=%+v", calls, status)
			}
		})
	}
}

func TestNotificationHealthSurvivesRestartAndMixedBatch(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed-batch", true: "restart"}[restart], func(t *testing.T) {
			s, dir := deliveryApp(t)
			calls := 0
			s.sendBark = func(context.Context, string, string, string, string) error {
				calls++
				if calls == 1 {
					return errors.New("private-provider-error")
				}
				return nil
			}
			s.Deliver(context.Background())
			if restart {
				engine, err := core.Open(filepath.Join(dir, "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				s, err = New(engine, s.Vault, "fixture-password", false, false)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Shutdown(context.Background())
			}
			recorder := httptest.NewRecorder()
			s.status(recorder, httptest.NewRequest("GET", "/api/status", nil))
			var status struct {
				Error    string     `json:"notification_error"`
				Failures int        `json:"notification_failure_count"`
				Success  *time.Time `json:"notification_last_success_at"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Error == "" || strings.Contains(status.Error, "private-provider") || status.Failures != 1 || status.Success == nil {
				t.Fatalf("durable transport health was lost: %+v", status)
			}
			// Completion removes the failed reminder from active delivery health.
			tasks := s.Engine.Snapshot().Tasks
			for i := range tasks {
				tasks[i].Status = "completed"
			}
			if err := s.Engine.Sync(core.PlatformRain, tasks, time.Now()); err != nil {
				t.Fatal(err)
			}
			recorder = httptest.NewRecorder()
			s.status(recorder, httptest.NewRequest("GET", "/api/status", nil))
			status = struct {
				Error    string     `json:"notification_error"`
				Failures int        `json:"notification_failure_count"`
				Success  *time.Time `json:"notification_last_success_at"`
			}{}
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Error != "" || status.Failures != 0 || status.Success == nil {
				t.Fatalf("resolved reminder still reported as unresolved: %+v", status)
			}
		})
	}
}

func TestCanceledDeliveryPreservesRetryBudgetAcrossRestarts(t *testing.T) {
	s, dir := deliveryApp(t)
	sends := 0
	s.sendBark = func(context.Context, string, string, string, string) error { sends++; return nil }
	for round := 0; round < core.MaxDeliveryAttempts; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.Deliver(ctx)
		engine, err := core.Open(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		s.Engine = engine
		if err = engine.Sync(core.PlatformRain, engine.Snapshot().Tasks, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	s.Deliver(context.Background())
	if sends != 2 {
		t.Fatalf("unattempted notifications lost across canceled restarts: %d sends", sends)
	}
	for _, event := range s.Engine.Snapshot().Events {
		if event.Kind == "new_task" && (event.Attempts != 1 || event.Status != "sent") {
			t.Fatalf("cancellation spent retry budget: %+v", event)
		}
	}
}

func TestDeliveryRechecksBeforeClaimingNextEvent(t *testing.T) {
	for _, scenario := range []string{"cancellation", "uncertain-send", "quiet", "revalidation"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := deliveryApp(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sends := 0
			s.sendBark = func(context.Context, string, string, string, string) error {
				sends++
				switch scenario {
				case "cancellation":
					cancel()
				case "uncertain-send":
					cancel()
					return context.Canceled
				case "quiet":
					settings := s.Engine.Snapshot().Settings
					settings.Timezone, settings.QuietEnabled = "UTC", true
					settings.QuietStart = time.Now().UTC().Add(-time.Minute).Format("15:04")
					settings.QuietEnd = time.Now().UTC().Add(time.Minute).Format("15:04")
					return s.Engine.UpdateSettings(settings)
				case "revalidation":
					return s.Engine.Fail(core.PlatformRain, false, "fixture sync failure", time.Now())
				}
				return nil
			}
			s.Deliver(ctx)
			if sends != 1 {
				t.Fatalf("expected one actual transport call: %d", sends)
			}
			if s.deliveryResult != nil {
				t.Fatal("actual transport outcome was not persisted")
			}
			attempts, untouched := 0, 0
			for _, event := range s.Engine.Snapshot().Events {
				if event.Kind == "new_task" {
					attempts += event.Attempts
					if event.Attempts == 0 && event.Status == "pending" {
						untouched++
					}
				}
			}
			if attempts != 1 || untouched != 1 {
				t.Fatalf("later event charged or real attempt refunded: attempts=%d untouched=%d", attempts, untouched)
			}
		})
	}
}

func TestDeliveryFailureHealthSurvivesIdleAndStorageRecovery(t *testing.T) {
	s, dir := deliveryApp(t)
	sends := 0
	s.sendBark = func(context.Context, string, string, string, string) error {
		sends++
		return errors.New("private-fixture-provider-error")
	}
	s.Deliver(context.Background())
	status := readNotificationHealth(t, s)
	if status.Error == "" || strings.Contains(status.Error, "private-fixture") || status.LastAttempt == nil || status.Failures != 2 {
		t.Fatal("failed transport not truthfully and safely surfaced")
	}
	failure := status.Error
	s.Deliver(context.Background())
	status = readNotificationHealth(t, s)
	if sends != 2 || status.Error != failure || status.Failures != 2 {
		t.Fatal("idle scheduler tick erased failed transport status")
	}
	// Generate one new candidate, so the next claim exercises a durable write.
	tasks := s.Engine.Snapshot().Tasks
	tasks = append(tasks, core.Task{ID: "third", Title: "Third", Kind: "homework", Status: "pending"})
	if err := s.Engine.Sync(core.PlatformRain, tasks, time.Now()); err != nil {
		t.Fatal(err)
	}
	restore := blockStateDirectory(t, dir)
	s.Deliver(context.Background())
	status = readNotificationHealth(t, s)
	if status.Error == failure || !strings.Contains(status.Error, "通知队列保存失败") || status.Failures != 2 {
		t.Fatal("storage failure did not take precedence")
	}
	restore()
	s.sendBark = func(context.Context, string, string, string, string) error { sends++; return nil }
	s.Deliver(context.Background())
	status = readNotificationHealth(t, s)
	if status.Error == "" || strings.Contains(status.Error, "保存失败") || status.Failures != 2 || status.LastSuccess == nil || sends != 3 {
		t.Fatalf("storage recovery hid unresolved earlier failures: %+v", status)
	}
}

func TestDeliveryRefreshesBarkConfigurationBetweenEvents(t *testing.T) {
	s, _ := deliveryApp(t)
	keys := []string{}
	s.sendBark = func(_ context.Context, _, key, _, _ string) error {
		keys = append(keys, key)
		return s.Vault.Update(func(c *vault.Config) error { c.BarkKey = "rotated-key"; return nil })
	}
	s.Deliver(context.Background())
	if len(keys) != 2 || keys[0] != "fixture-key" || keys[1] != "rotated-key" {
		t.Fatalf("later event used an obsolete notification destination: %v", keys)
	}
}

func TestUnsentReturnRetriesPersistenceBeforeNetwork(t *testing.T) {
	s, dir := deliveryApp(t)
	claims, err := s.Engine.PendingOneWithError(time.Now())
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %v, %d", err, len(claims))
	}
	s.deliveryResult = &deliveryOutcome{unsent: &claims[0]}
	sends := 0
	s.sendBark = func(context.Context, string, string, string, string) error { sends++; return nil }
	restore := blockStateDirectory(t, dir)
	s.Deliver(context.Background())
	if sends != 0 || s.deliveryResult == nil {
		t.Fatal("network resumed while unsent claim return was not durable")
	}
	restore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Deliver(ctx)
	if s.deliveryResult != nil || s.deliveryAt != nil {
		t.Fatal("return was lost or counted as a real network outcome")
	}
	for _, event := range s.Engine.Snapshot().Events {
		if event.Attempts != 0 {
			t.Fatal("unattempted claim was not durably refunded")
		}
	}
	s.Deliver(context.Background())
	if sends != 2 {
		t.Fatal("delivery did not recover after returning the unsent claim")
	}
}
