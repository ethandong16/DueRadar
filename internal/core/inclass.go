package core

import (
	"errors"
	"strings"
	"time"
)

// InClassWindow bounds how long a detected in-class quiz stays worth sending.
// The quiz is answered inside the lesson; a notification that could only be
// delivered after this window is dropped rather than arriving late.
const InClassWindow = 15 * time.Minute

// inClassSeenRetention keeps deduplication keys long enough to cover a restart
// in the middle of a class without growing the state file indefinitely.
const inClassSeenRetention = 7 * 24 * time.Hour

// InClassQuiz is one answerable activity observed in a classroom that currently
// has a live lesson. Notify is false for activities that already existed when
// the lesson was first observed; those are recorded so they are not reported
// later, but they are not announced.
type InClassQuiz struct {
	ClassroomID string
	ActivityID  string
	Course      string
	Title       string
	Body        string
	Notify      bool
}

// urgentEvent marks notifications that must bypass quiet hours. A quiz pushed
// during class is only answerable for a short time, so it is always urgent.
func urgentEvent(ev Event) bool {
	if ev.Kind == "in_class" {
		return true
	}
	return ev.Kind == "exam_start" && ev.MinutesBefore == 60 || (ev.Kind == "deadline" || ev.Kind == "video_deadline") && ev.MinutesBefore == 30
}

// ReportInClass records observed in-class activities and creates a notification
// event for each newly seen activity that should be announced. It returns the
// number of events created. Repeated polls, restarts, and multiple live lessons
// share the same persisted key, so one quiz is announced exactly once.
func (e *Engine) ReportInClass(platform string, items []InClassQuiz, now time.Time) (int, error) {
	if !validPlatform(platform) {
		return 0, errors.New("unknown platform")
	}
	for _, item := range items {
		if strings.TrimSpace(item.ClassroomID) == "" || strings.TrimSpace(item.ActivityID) == "" {
			return 0, errors.New("in-class activity requires classroom and activity identifiers")
		}
		if item.Notify && strings.TrimSpace(item.Title) == "" {
			return 0, errors.New("in-class activity requires a title")
		}
	}
	added := 0
	// Polling every 20 seconds must not rewrite the state file each time. Only a
	// newly observed activity is worth a durable transaction.
	e.mu.Lock()
	novel := false
	for _, item := range items {
		if _, seen := e.state.InClassSeen[platform+"\x00"+item.ClassroomID+"\x00"+item.ActivityID]; !seen {
			novel = true
			break
		}
	}
	e.mu.Unlock()
	if !novel {
		return 0, nil
	}
	err := e.change(func(s *State) error {
		if s.InClassSeen == nil {
			s.InClassSeen = map[string]time.Time{}
		}
		for key, seen := range s.InClassSeen {
			if now.Sub(seen) > inClassSeenRetention {
				delete(s.InClassSeen, key)
			}
		}
		for _, item := range items {
			key := platform + "\x00" + item.ClassroomID + "\x00" + item.ActivityID
			if _, seen := s.InClassSeen[key]; seen {
				continue
			}
			s.InClassSeen[key] = now
			if !item.Notify {
				continue
			}
			before := len(s.Events)
			addEvent(s, Event{
				ID:        eventID(platform, "in_class", item.ClassroomID, item.ActivityID),
				Platform:  platform,
				Kind:      "in_class",
				Title:     "课上答题 · " + item.Title,
				Body:      strings.TrimSpace(item.Body),
				DueAt:     now,
				CreatedAt: now,
				Status:    "pending",
			})
			if len(s.Events) > before {
				added++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}
