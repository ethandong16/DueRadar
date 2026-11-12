package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethandong16/due-radar/internal/bark"
	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/vault"
)

func seedTasks(t *testing.T, s *Server, tasks ...core.Task) {
	t.Helper()
	if err := s.Engine.Sync(core.PlatformRain, tasks, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestTaskDismissalOnlyAcceptsUnsubmittableOverdueWork(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	now := time.Now()
	past, future := now.Add(-2*time.Hour), now.Add(2*time.Hour)
	seedTasks(t, s,
		core.Task{ID: "late", Title: "Late homework", Kind: "homework", Status: "pending", EndAt: &past},
		core.Task{ID: "soon", Title: "Future homework", Kind: "homework", Status: "pending", EndAt: &future},
		core.Task{ID: "done", Title: "Submitted", Kind: "homework", Status: "completed", EndAt: &past},
	)

	dismiss := func(id string, dismissed bool) *httptestResponse {
		body, _ := json.Marshal(map[string]any{"platform": core.PlatformRain, "id": id, "dismissed": dismissed})
		w := request(h, "PUT", "/api/tasks/dismissal", string(body), cookie, "")
		return &httptestResponse{code: w.Code, body: w.Body.String()}
	}
	if got := dismiss("late", true); got.code != http.StatusOK {
		t.Fatalf("dismiss overdue: %d %s", got.code, got.body)
	}
	found := false
	for _, task := range s.Engine.Snapshot().Tasks {
		if task.ID == "late" {
			found = task.DismissedAt != nil
		}
	}
	if !found {
		t.Fatal("dismissal was not persisted")
	}
	if got := dismiss("soon", true); got.code != http.StatusBadRequest {
		t.Fatalf("future task must stay dismissible: %d %s", got.code, got.body)
	}
	if got := dismiss("done", true); got.code != http.StatusBadRequest {
		t.Fatalf("completed task must not be dismissed: %d %s", got.code, got.body)
	}
	if got := dismiss("late", false); got.code != http.StatusOK {
		t.Fatalf("restore: %d %s", got.code, got.body)
	}
	for _, task := range s.Engine.Snapshot().Tasks {
		if task.ID == "late" && task.DismissedAt != nil {
			t.Fatal("restore did not clear dismissal")
		}
	}
}

type httptestResponse struct {
	code int
	body string
}

func TestReminderTestUsesSavedRuleWithoutCreatingEvents(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	var sent int
	var title, body string
	s.sendBark = func(_ context.Context, _, _, nextTitle, nextBody string, _ bark.Options) error {
		sent++
		title, body = nextTitle, nextBody
		return nil
	}
	settings := s.Engine.Snapshot().Settings
	settings.QuietEnabled = false
	settings.DeadlineMinutes = []int{1440, 180, 30}
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	before := len(s.Engine.Snapshot().Events)
	w := request(h, "POST", "/api/reminders/test", `{"kind":"deadline"}`, cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("test reminder: %d %s", w.Code, w.Body.String())
	}
	if sent != 1 || !strings.Contains(title, "任务即将截止") || !strings.Contains(body, "1 天") || !strings.Contains(body, "30 分钟") {
		t.Fatalf("unexpected sample: sent=%d title=%q body=%q", sent, title, body)
	}
	if got := len(s.Engine.Snapshot().Events); got != before {
		t.Fatalf("test reminder created events: before=%d after=%d", before, got)
	}
	if w := request(h, "POST", "/api/reminders/test", `{"kind":"deadline"}`, cookie, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second immediate test must be rate limited: %d", w.Code)
	}
	if w := request(h, "POST", "/api/reminders/test", `{"kind":"unknown"}`, cookie, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind: %d", w.Code)
	}
}

func TestReminderTestReportsQuietHoursInsteadOfBypassingThem(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	sent := 0
	s.sendBark = func(context.Context, string, string, string, string, bark.Options) error {
		sent++
		return nil
	}
	settings := s.Engine.Snapshot().Settings
	settings.QuietEnabled = true
	settings.QuietStart = "00:00"
	settings.QuietEnd = "23:59"
	settings.DeadlineMinutes = []int{1440}
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	w := request(h, "POST", "/api/reminders/test", `{"kind":"deadline"}`, cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("quiet test: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Sent  bool   `json:"sent"`
		Quiet bool   `json:"quiet"`
		Note  string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Sent || !result.Quiet || !strings.Contains(result.Note, "静默时段") {
		t.Fatalf("quiet hours were not explained: %+v", result)
	}
	if sent != 0 {
		t.Fatalf("quiet-hours test sent %d notifications", sent)
	}
}

func TestReminderTestUsesConfiguredBarkCredentials(t *testing.T) {
	s, h := testApp(t)
	cookie := loginCookie(t, h)
	if err := s.Vault.Update(func(c *vault.Config) error {
		c.BarkURL, c.BarkKey = "https://api.day.app", "TEST_KEY"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var server, key string
	s.sendBark = func(_ context.Context, nextServer, nextKey, _, _ string, _ bark.Options) error {
		server, key = nextServer, nextKey
		return errors.New("provider failure")
	}
	settings := s.Engine.Snapshot().Settings
	settings.QuietEnabled = false
	if err := s.Engine.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if w := request(h, "POST", "/api/reminders/test", `{"kind":"exam_start"}`, cookie, ""); w.Code != http.StatusBadGateway {
		t.Fatalf("transport failure must be visible: %d %s", w.Code, w.Body.String())
	}
	if server != "https://api.day.app" || key != "TEST_KEY" {
		t.Fatalf("test did not use saved credentials: %q %q", server, key)
	}
}
