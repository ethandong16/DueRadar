package core

import (
	"errors"
	"strings"
	"time"
)

// ReserveClassroomAction is an at-most-once fence for external side effects.
// Reservations survive timeouts, failed transports and restarts: an uncertain
// submission must never be retried as a different random answer.
func (e *Engine) ReserveClassroomAction(platform, lesson, problem, kind string, now time.Time) (bool, error) {
	if !validPlatform(platform) || !classroomActionPart(lesson) || !classroomActionPart(problem) || (kind != "fallback" && kind != "analysis") || now.IsZero() {
		return false, errors.New("invalid classroom action")
	}
	key := platform + "\x00" + lesson + "\x00" + problem + "\x00" + kind
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.state.ClassroomActions[key]; exists {
		return false, nil
	}
	next := cloneState(e.state)
	if next.ClassroomActions == nil {
		next.ClassroomActions = make(map[string]time.Time)
	}
	next.ClassroomActions[key] = now
	pruneInClassSeen(&next, now)
	if err := e.save(next); err != nil {
		return false, err
	}
	e.state = next
	return true, nil
}

// ReportClassroomAnalysis uses the normal durable Bark delivery queue, but has
// its own kind so disabling analysis invalidates already queued results too.
func (e *Engine) ReportClassroomAnalysis(lesson, problem, title, body string, expires, now time.Time) error {
	if !classroomActionPart(lesson) || !classroomActionPart(problem) || strings.TrimSpace(body) == "" || !expires.After(now) {
		return errors.New("invalid classroom analysis")
	}
	return e.change(func(s *State) error {
		if !s.Settings.RainAnalysisEnabled {
			return nil
		}
		addEvent(s, Event{ID: eventID(PlatformRain, "rain_analysis", lesson, problem), Platform: PlatformRain, Kind: "rain_analysis", Title: "AI 参考答案 · " + title, Body: body, DueAt: now, CreatedAt: now, AnchorAt: timePtr(expires), Status: "pending"})
		return nil
	})
}

// CancelClassroomAnalyses is used when a key/model is replaced. Reservations are
// retained: changing settings must not charge for or announce an old problem again.
func (e *Engine) CancelClassroomAnalyses() error {
	return e.cancelClassroomAnalyses("", "classroom analysis configuration changed")
}

// CancelClassroomAnalysis prevents a queued/retrying answer arriving after the
// teacher closes its problem. Other questions and normal quiz alerts are kept.
func (e *Engine) CancelClassroomAnalysis(lesson, problem string) error {
	if !classroomActionPart(lesson) || !classroomActionPart(problem) {
		return errors.New("invalid classroom analysis")
	}
	return e.cancelClassroomAnalyses(eventID(PlatformRain, "rain_analysis", lesson, problem), "classroom problem closed")
}

func (e *Engine) cancelClassroomAnalyses(id, reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := cloneState(e.state)
	changed := false
	for i := range next.Events {
		if next.Events[i].Kind == "rain_analysis" && (id == "" || next.Events[i].ID == id) && actionable(next.Events[i]) {
			cancel(&next.Events[i], reason)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := e.save(next); err != nil {
		return err
	}
	e.state = next
	return nil
}

func classroomActionPart(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 160 && !strings.ContainsRune(s, '\x00')
}
