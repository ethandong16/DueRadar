package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"
)

const MaxDeliveryAttempts = 6
const FreshnessLimit = 30 * time.Minute
const deliveryLease = 2 * time.Minute

func validPlatform(p string) bool {
	return p == PlatformRain || p == PlatformZhidaoShared || p == PlatformZhidaoCampus
}

func ValidateSettings(s Settings) error {
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return errors.New("unknown timezone")
	}
	start, err := clockMinute(s.QuietStart)
	if err != nil {
		return errors.New("quiet_start must be HH:MM")
	}
	end, err := clockMinute(s.QuietEnd)
	if err != nil {
		return errors.New("quiet_end must be HH:MM")
	}
	if s.QuietEnabled && start == end {
		return errors.New("quiet start and end must differ")
	}
	for _, offsets := range [][]int{s.DeadlineMinutes, s.ExamStartMinutes, s.VideoMinutes} {
		if len(offsets) > 32 {
			return errors.New("at most 32 reminders per type")
		}
		seen := map[int]bool{}
		for _, n := range offsets {
			if n < 1 || n > 525600 {
				return errors.New("reminder minutes must be between 1 and 525600")
			}
			if seen[n] {
				return errors.New("duplicate reminder minutes")
			}
			seen[n] = true
		}
	}
	return nil
}

func clockMinute(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, errors.New("invalid clock")
	}
	return t.Hour()*60 + t.Minute(), nil
}

func IsQuiet(s Settings, now time.Time) bool {
	if !s.QuietEnabled {
		return false
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return true
	}
	start, err := clockMinute(s.QuietStart)
	if err != nil {
		return true
	}
	end, err := clockMinute(s.QuietEnd)
	if err != nil {
		return true
	}
	local := now.In(loc)
	minute := local.Hour()*60 + local.Minute()
	if start < end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}

func (e *Engine) UpdateSettings(settings Settings) error {
	return e.UpdateSettingsAt(settings, time.Now())
}

// UpdateSettingsAt permits a deterministic administrative update in tests and
// import tools. New schedules start from now and do not backfill past offsets.
func (e *Engine) UpdateSettingsAt(settings Settings, now time.Time) error {
	if err := ValidateSettings(settings); err != nil {
		return err
	}
	return e.change(func(s *State) error {
		s.Settings = settings
		for i := range s.Events {
			ev := &s.Events[i]
			if actionable(*ev) && scheduled(ev.Kind) && !offsetConfigured(settings, *ev) {
				cancel(ev, "reminder setting removed")
			}
		}
		for _, task := range s.Tasks {
			schedule(s, task, now)
		}
		return nil
	})
}

func accountIndex(s *State, p string) int {
	for i := range s.Accounts {
		if s.Accounts[i].Platform == p {
			return i
		}
	}
	s.Accounts = append(s.Accounts, Account{Platform: p, LoginState: "unbound", SyncState: "idle", NeedsRevalidation: true})
	return len(s.Accounts) - 1
}

func (e *Engine) SetAccount(a Account) error {
	if !validPlatform(a.Platform) {
		return errors.New("unknown platform")
	}
	return e.change(func(s *State) error {
		i := accountIndex(s, a.Platform)
		old := s.Accounts[i]
		// Binding changes never manufacture a successful task synchronization.
		old.LoginState = a.LoginState
		if a.LastError != "" {
			old.LastError = a.LastError
		}
		if a.LoginState != "logged_in" {
			old.NeedsRevalidation = true
		}
		s.Accounts[i] = old
		return nil
	})
}

func (e *Engine) RequireRevalidation() error {
	return e.change(func(s *State) error {
		for i := range s.Accounts {
			s.Accounts[i].NeedsRevalidation = true
			if s.Accounts[i].LastSuccessAt != nil {
				s.Accounts[i].SyncState = "revalidating"
			}
		}
		for i := range s.Events {
			if s.Events[i].Status == "sending" {
				s.Events[i].Status = "pending"
				s.Events[i].NextAttemptAt = nil
			}
		}
		return nil
	})
}

func (e *Engine) Sync(platform string, tasks []Task, now time.Time) error {
	if !validPlatform(platform) {
		return errors.New("unknown platform")
	}
	seen := map[string]bool{}
	for i := range tasks {
		t := &tasks[i]
		if strings.TrimSpace(t.ID) == "" {
			return errors.New("task ID is required")
		}
		if seen[t.ID] {
			return errors.New("duplicate task ID in snapshot")
		}
		seen[t.ID] = true
		if strings.TrimSpace(t.Title) == "" {
			return errors.New("task title is required")
		}
		if t.Kind != "homework" && t.Kind != "exam" && t.Kind != "chapter" && t.Kind != "video" {
			return errors.New("unknown task kind")
		}
		if t.Status != "pending" && t.Status != "completed" && t.Status != "unknown" {
			return errors.New("unknown task status")
		}
		if t.Progress != nil && (math.IsNaN(*t.Progress) || math.IsInf(*t.Progress, 0) || *t.Progress < 0 || *t.Progress > 100) {
			return errors.New("progress must be between 0 and 100")
		}
		if t.StartAt != nil && t.EndAt != nil && t.EndAt.Before(*t.StartAt) {
			return errors.New("task end precedes start")
		}
	}
	return e.change(func(s *State) error {
		ai := accountIndex(s, platform)
		previous := s.Accounts[ai]
		recovery := previous.NeedsRevalidation || previous.LastSuccessAt != nil && now.Sub(*previous.LastSuccessAt) > FreshnessLimit
		byID := map[string]int{}
		for i, t := range s.Tasks {
			if t.Platform == platform {
				byID[t.ID] = i
			}
		}
		for _, incoming := range tasks {
			incoming.Platform = platform
			incoming.LastSyncedAt = now
			if incoming.SourceTimezone == "" {
				incoming.SourceTimezone = s.Settings.Timezone
			}
			if i, exists := byID[incoming.ID]; exists {
				old := s.Tasks[i]
				s.Tasks[i] = incoming
				changed := !sameTime(old.StartAt, incoming.StartAt) || !sameTime(old.EndAt, incoming.EndAt)
				if changed || old.Kind != incoming.Kind || incoming.Status != "pending" {
					invalidateObsoleteTask(s, platform, incoming.ID, now)
				}
				if changed && taskPending(incoming, now) {
					addEvent(s, Event{ID: eventID(platform, incoming.ID, "time_changed", timeKey(incoming.StartAt), timeKey(incoming.EndAt), now.UTC().Format(time.RFC3339Nano)), TaskID: incoming.ID, Platform: platform, Kind: "time_changed", Title: "任务时间变更 · " + incoming.Title, Body: taskBody(incoming, s.Settings), DueAt: now, CreatedAt: now, Status: "pending"})
				}
				if old.Status == "unknown" && taskPending(incoming, now) {
					addEvent(s, Event{ID: eventID(platform, incoming.ID, "new_task"), TaskID: incoming.ID, Platform: platform, Kind: "new_task", Title: "新任务 · " + incoming.Title, Body: taskBody(incoming, s.Settings), DueAt: now, CreatedAt: now, Status: "pending"})
				}
			} else {
				s.Tasks = append(s.Tasks, incoming)
				if taskPending(incoming, now) {
					addEvent(s, Event{ID: eventID(platform, incoming.ID, "new_task"), TaskID: incoming.ID, Platform: platform, Kind: "new_task", Title: "新任务 · " + incoming.Title, Body: taskBody(incoming, s.Settings), DueAt: now, CreatedAt: now, Status: "pending"})
				}
			}
			schedule(s, incoming, now)
		}
		// A full platform snapshot that omits a previous task cannot prove its
		// completion. Retain history as unknown and suppress stale reminders.
		for i := range s.Tasks {
			t := &s.Tasks[i]
			if t.Platform == platform && !seen[t.ID] && t.Status != "completed" {
				t.Status = "unknown"
				t.LastSyncedAt = now
				invalidateTask(s, platform, t.ID, "task absent from latest snapshot")
			}
		}
		a := &s.Accounts[ai]
		a.LoginState = "logged_in"
		a.SyncState = "success"
		a.LastSuccessAt = timePtr(now)
		a.ConsecutiveFailures = 0
		a.LastError = ""
		a.NeedsRevalidation = false
		a.FaultNotified = false
		for i := range s.Events {
			ev := &s.Events[i]
			if ev.Platform == platform && ev.Kind == "fault" && actionable(*ev) {
				cancel(ev, "platform recovered")
			}
		}
		if recovery {
			collapseRecovery(s, platform, now)
		}
		return nil
	})
}

func (e *Engine) Fail(platform string, expired bool, message string, now time.Time) error {
	if !validPlatform(platform) {
		return errors.New("unknown platform")
	}
	return e.change(func(s *State) error {
		i := accountIndex(s, platform)
		a := &s.Accounts[i]
		if a.ConsecutiveFailures == 0 {
			a.Incident++
		}
		a.ConsecutiveFailures++
		a.NeedsRevalidation = true
		a.SyncState = "error"
		// Provider errors can contain signed URLs and credentials. Durable state
		// deliberately records only a classified, fixed diagnostic.
		_ = message
		a.LastError = "平台同步失败，请检查连接及账号状态"
		if expired {
			a.LoginState = "expired"
			a.LastError = "登录已过期，请重新绑定账号"
		}
		if !a.FaultNotified && (expired || a.ConsecutiveFailures >= 3) {
			a.FaultNotified = true
			addEvent(s, Event{ID: eventID(platform, "fault", fmt.Sprint(a.Incident)), Platform: platform, Kind: "fault", Title: "平台需要处理 · " + platform, Body: a.LastError, DueAt: now, CreatedAt: now, Status: "pending"})
		}
		return nil
	})
}

func taskPending(t Task, now time.Time) bool {
	return t.Status == "pending" && (t.EndAt == nil || t.EndAt.After(now))
}
func timePtr(t time.Time) *time.Time { return &t }
func sameTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}
func timeKey(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func eventID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}
func actionable(e Event) bool { return e.Status == "pending" || e.Status == "sending" }
func scheduled(kind string) bool {
	return kind == "deadline" || kind == "video_deadline" || kind == "exam_start"
}
func cancel(e *Event, reason string) {
	e.Status = "canceled"
	e.LastError = reason
	e.NextAttemptAt = nil
}
func invalidateTask(s *State, p, id, reason string) {
	for i := range s.Events {
		ev := &s.Events[i]
		if ev.Platform == p && ev.TaskID == id && actionable(*ev) {
			cancel(ev, reason)
		}
	}
}
func invalidateObsoleteTask(s *State, p, id string, now time.Time) {
	for i := range s.Events {
		ev := &s.Events[i]
		if ev.Platform == p && ev.TaskID == id && actionable(*ev) && (!scheduled(ev.Kind) || !eventValid(s, *ev, now)) {
			cancel(ev, "task status or time changed")
		}
	}
}

func addEvent(s *State, ev Event) {
	for i, old := range s.Events {
		if old.ID == ev.ID {
			if old.Status == "canceled" && scheduled(ev.Kind) && !ev.DueAt.Before(ev.CreatedAt) {
				s.Events[i] = ev
			}
			return
		}
	}
	s.Events = append(s.Events, ev)
}

func offsetsFor(s Settings, kind string) []int {
	switch kind {
	case "exam_start":
		return s.ExamStartMinutes
	case "video_deadline":
		return s.VideoMinutes
	default:
		return s.DeadlineMinutes
	}
}
func offsetConfigured(s Settings, ev Event) bool {
	for _, n := range offsetsFor(s, ev.Kind) {
		if n == ev.MinutesBefore {
			return true
		}
	}
	return false
}

func taskBody(t Task, s Settings) string {
	body := t.Course
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		loc = time.UTC
	}
	if t.StartAt != nil {
		body += "\n开始：" + t.StartAt.In(loc).Format("2006-01-02 15:04 MST")
	}
	if t.EndAt != nil {
		body += "\n截止：" + t.EndAt.In(loc).Format("2006-01-02 15:04 MST")
	}
	return strings.TrimSpace(body)
}

func schedule(s *State, t Task, now time.Time) {
	if !taskPending(t, now) {
		return
	}
	add := func(kind string, anchor *time.Time) {
		if anchor == nil || !anchor.After(now) {
			return
		}
		for _, minutes := range offsetsFor(s.Settings, kind) {
			due := anchor.Add(-time.Duration(minutes) * time.Minute)
			if due.Before(now) {
				continue
			}
			title := "任务即将截止 · " + t.Title
			if kind == "exam_start" {
				title = "考试即将开始 · " + t.Title
			}
			addEvent(s, Event{ID: eventID(t.Platform, t.ID, kind, timeKey(anchor), fmt.Sprint(minutes)), TaskID: t.ID, Platform: t.Platform, Kind: kind, Title: title, Body: taskBody(t, s.Settings), DueAt: due, CreatedAt: now, Status: "pending", AnchorAt: timePtr(*anchor), MinutesBefore: minutes})
		}
	}
	kind := "deadline"
	if t.Kind == "video" {
		kind = "video_deadline"
	}
	add(kind, t.EndAt)
	if t.Kind == "exam" {
		add("exam_start", t.StartAt)
	}
}

func eventValid(s *State, ev Event, now time.Time) bool {
	if ev.Kind == "fault" {
		for _, a := range s.Accounts {
			if a.Platform == ev.Platform {
				return a.FaultNotified && a.ConsecutiveFailures > 0
			}
		}
		return false
	}
	for _, t := range s.Tasks {
		if t.Platform != ev.Platform || t.ID != ev.TaskID {
			continue
		}
		if !taskPending(t, now) {
			return false
		}
		if ev.Kind == "exam_start" {
			return t.Kind == "exam" && sameTime(ev.AnchorAt, t.StartAt) && t.StartAt != nil && t.StartAt.After(now) && offsetConfigured(s.Settings, ev)
		}
		if ev.Kind == "deadline" || ev.Kind == "video_deadline" {
			expected := "deadline"
			if t.Kind == "video" {
				expected = "video_deadline"
			}
			return ev.Kind == expected && sameTime(ev.AnchorAt, t.EndAt) && t.EndAt != nil && t.EndAt.After(now) && offsetConfigured(s.Settings, ev)
		}
		return true
	}
	return false
}

func collapseRecovery(s *State, p string, now time.Time) {
	latest := map[string]int{}
	for i := range s.Events {
		ev := &s.Events[i]
		if ev.Platform != p || ev.TaskID == "" || !actionable(*ev) {
			continue
		}
		if !eventValid(s, *ev, now) {
			cancel(ev, "no longer valid after recovery")
			continue
		}
		if ev.DueAt.After(now) {
			continue
		}
		if old, ok := latest[ev.TaskID]; ok {
			// On an equal due time, a scheduled reminder is more specific than
			// the first-sync discovery notification.
			if ev.DueAt.After(s.Events[old].DueAt) || ev.DueAt.Equal(s.Events[old].DueAt) && scheduled(ev.Kind) {
				cancel(&s.Events[old], "superseded during recovery")
				latest[ev.TaskID] = i
			} else {
				cancel(ev, "superseded during recovery")
			}
		} else {
			latest[ev.TaskID] = i
		}
	}
}

// Pending durably claims events for two minutes. Call MarkSent or MarkFailed
// after each send. Only an already configured exact 60-minute exam-start or
// 30-minute deadline event bypasses quiet hours, including its delayed retry.
// Other offsets and discovery/time-change events never inherit urgency.
func (e *Engine) Pending(now time.Time) []Event {
	events, _ := e.PendingWithError(now)
	return events
}

// PendingWithError reports durable claim failures separately from an idle queue.
func (e *Engine) PendingWithError(now time.Time) ([]Event, error) {
	return e.pendingWithLimit(now, 0)
}

// PendingOneWithError claims only the next due event. Serial transports use it
// to leave later notifications untouched until they are ready to send them.
func (e *Engine) PendingOneWithError(now time.Time) ([]Event, error) {
	return e.pendingWithLimit(now, 1)
}

func (e *Engine) pendingWithLimit(now time.Time, limit int) ([]Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := cloneState(e.state)
	changed := false
	result := []Event{}
	indices := make([]int, len(s.Events))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool { return s.Events[indices[i]].DueAt.Before(s.Events[indices[j]].DueAt) })
	for _, i := range indices {
		ev := &s.Events[i]
		if !actionable(*ev) {
			continue
		}
		if !eventValid(&s, *ev, now) {
			cancel(ev, "event expired or no longer valid")
			changed = true
			continue
		}
		if ev.DueAt.After(now) || ev.NextAttemptAt != nil && ev.NextAttemptAt.After(now) {
			continue
		}
		if ev.Attempts >= MaxDeliveryAttempts {
			ev.Status = "failed"
			ev.LastError = "通知发送达到重试上限"
			changed = true
			continue
		}
		if ev.Kind != "fault" {
			fresh := false
			for _, a := range s.Accounts {
				if a.Platform == ev.Platform {
					fresh = !a.NeedsRevalidation && a.SyncState == "success" && a.LastSuccessAt != nil && now.Sub(*a.LastSuccessAt) <= FreshnessLimit
					break
				}
			}
			if !fresh {
				continue
			}
		}
		urgent := ev.Kind == "exam_start" && ev.MinutesBefore == 60 || (ev.Kind == "deadline" || ev.Kind == "video_deadline") && ev.MinutesBefore == 30
		if IsQuiet(s.Settings, now) && !urgent {
			continue
		}
		// Keep event identity and due time stable while sending the latest task
		// metadata, other timestamp, and display timezone confirmed by sync.
		if ev.Kind != "fault" {
			for _, task := range s.Tasks {
				if task.Platform == ev.Platform && task.ID == ev.TaskID {
					prefix := "任务即将截止 · "
					switch ev.Kind {
					case "new_task":
						prefix = "新任务 · "
					case "time_changed":
						prefix = "任务时间变更 · "
					case "exam_start":
						prefix = "考试即将开始 · "
					}
					ev.Title = prefix + task.Title
					ev.Body = taskBody(task, s.Settings)
					break
				}
			}
		}
		ev.Status = "sending"
		ev.Attempts++
		ev.NextAttemptAt = timePtr(now.Add(deliveryLease))
		changed = true
		claimed := *ev
		if ev.AnchorAt != nil {
			claimed.AnchorAt = timePtr(*ev.AnchorAt)
		}
		if ev.SentAt != nil {
			claimed.SentAt = timePtr(*ev.SentAt)
		}
		claimed.NextAttemptAt = timePtr(*ev.NextAttemptAt)
		result = append(result, claimed)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	if changed {
		if err := e.save(s); err != nil {
			return nil, err
		}
		e.state = s
	}
	sort.Slice(result, func(i, j int) bool { return result[i].DueAt.Before(result[j].DueAt) })
	return result, nil
}

func (e *Engine) MarkSent(id string, now time.Time) error {
	return e.change(func(s *State) error {
		for i := range s.Events {
			ev := &s.Events[i]
			if ev.ID == id {
				if ev.Status == "sent" {
					return nil
				}
				if ev.Status != "sending" && !(ev.Status == "canceled" && ev.Attempts > 0) {
					return errors.New("event is not claimed")
				}
				ev.Status = "sent"
				ev.SentAt = timePtr(now)
				ev.NextAttemptAt = nil
				ev.LastError = ""
				return nil
			}
		}
		return errors.New("event not found")
	})
}

func (e *Engine) MarkFailed(id string, message string, now time.Time) error {
	return e.change(func(s *State) error {
		for i := range s.Events {
			ev := &s.Events[i]
			if ev.ID == id {
				if ev.Status == "canceled" {
					return nil
				}
				if ev.Status != "sending" {
					return errors.New("event is not claimed")
				}
				_ = message // External transport errors may contain the private Bark key.
				ev.LastError = "通知发送失败，请检查 Bark 配置及网络"
				if ev.Attempts >= MaxDeliveryAttempts {
					ev.Status = "failed"
					ev.NextAttemptAt = nil
					return nil
				}
				delay := time.Minute * time.Duration(1<<uint(ev.Attempts-1))
				if delay > 30*time.Minute {
					delay = 30 * time.Minute
				}
				ev.Status = "pending"
				ev.NextAttemptAt = timePtr(now.Add(delay))
				return nil
			}
		}
		return errors.New("event not found")
	})
}
