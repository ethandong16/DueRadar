package core

import "time"

// ClaimStillValid rechecks each notification immediately before network I/O.
// A sync/settings change may invalidate later members of an earlier batch.
func (e *Engine) ClaimStillValid(id string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.state.Events {
		if ev.ID != id {
			continue
		}
		if ev.Status != "sending" || !eventValid(&e.state, ev, now) {
			return false
		}
		urgent := ev.Kind == "exam_start" && ev.MinutesBefore == 60 || (ev.Kind == "deadline" || ev.Kind == "video_deadline") && ev.MinutesBefore == 30
		if IsQuiet(e.state.Settings, now) && !urgent {
			return false
		}
		if ev.Kind == "fault" {
			return true
		}
		for _, a := range e.state.Accounts {
			if a.Platform == ev.Platform {
				return !a.NeedsRevalidation && a.SyncState == "success" && a.LastSuccessAt != nil && now.Sub(*a.LastSuccessAt) <= FreshnessLimit
			}
		}
	}
	return false
}
