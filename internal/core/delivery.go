package core

import "time"

// DeliveryHealth is derived from durable events, including after a restart.
// A later successful event must not hide a different event still awaiting retry.
type DeliveryHealth struct {
	UnresolvedFailures int
	LastSuccessAt      *time.Time
}

func (e *Engine) DeliveryHealth(now time.Time) DeliveryHealth {
	e.mu.Lock()
	defer e.mu.Unlock()
	var health DeliveryHealth
	for _, event := range e.state.Events {
		if event.SentAt != nil && (health.LastSuccessAt == nil || event.SentAt.After(*health.LastSuccessAt)) {
			health.LastSuccessAt = cloneTime(event.SentAt)
		}
		if (actionable(event) || event.Status == "failed") && event.Attempts > 0 && event.LastError != "" && eventValid(&e.state, event, now) {
			health.UnresolvedFailures++
		}
	}
	return health
}

// ReturnUnsent releases a claim only when the caller knows that transport was
// never invoked. The lease and attempt count identify this exact reservation;
// a stale caller cannot release a newer claim or refund a real network attempt.
func (e *Engine) ReturnUnsent(claim Event) error {
	return e.change(func(s *State) error {
		for i := range s.Events {
			ev := &s.Events[i]
			if ev.ID == claim.ID && ev.Status == "sending" && ev.Attempts == claim.Attempts && sameTime(ev.NextAttemptAt, claim.NextAttemptAt) {
				ev.Status = "pending"
				if ev.Attempts > 0 {
					ev.Attempts--
				}
				ev.NextAttemptAt = nil
				break
			}
		}
		return nil
	})
}

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
		if IsQuiet(e.state.Settings, now) && !urgentEvent(ev) {
			return false
		}
		if ev.Kind == "fault" {
			return true
		}
		for _, a := range e.state.Accounts {
			if a.Platform == ev.Platform {
				if ev.Kind == "in_class" {
					return !a.NeedsRevalidation
				}
				return !a.NeedsRevalidation && a.SyncState == "success" && a.LastSuccessAt != nil && now.Sub(*a.LastSuccessAt) <= FreshnessLimit
			}
		}
	}
	return false
}
