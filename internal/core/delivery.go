package core

import (
	"errors"
	"time"
)

const (
	claimActive   = "active"
	claimReturned = "returned"
	claimSent     = "sent"
	claimFailed   = "failed"
)

// ErrStaleClaim means the reservation is missing, superseded, or already
// settled with a different outcome. It is not a storage error. An issued claim
// has a nonzero ClaimID; the ReturnUnsent legacy zero-ID path remains best effort.
var ErrStaleClaim = errors.New("delivery claim is stale")

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

// ReturnUnsent releases a claim only when the caller knows transport was never
// invoked. Superseded or already-settled claims return ErrStaleClaim; repeating
// the same successful return is harmless. A canceled event remains canceled.
// Legacy zero-generation returns retain their old best-effort no-op behavior.
func (e *Engine) ReturnUnsent(claim Event) error {
	return e.change(func(s *State) error {
		ev := claimedEvent(s, claim)
		if ev == nil {
			// Preserve the pre-fencing API for a legacy claim already handed to
			// a caller. Never let it match an event with a new ClaimID.
			if claim.ClaimID == 0 {
				for i := range s.Events {
					legacy := &s.Events[i]
					if legacy.ID == claim.ID && legacy.ClaimID == 0 && legacy.Status == "sending" && legacy.Attempts == claim.Attempts && sameTime(legacy.NextAttemptAt, claim.NextAttemptAt) {
						returnUnsent(legacy)
						return nil
					}
				}
				return nil
			}
			return ErrStaleClaim
		}
		if ev.ClaimState == claimReturned {
			return nil
		}
		if !unsettledClaim(ev) {
			return ErrStaleClaim
		}
		returnUnsent(ev)
		return nil
	})
}

// MarkSentClaim records an accepted transport for this exact reservation. A
// lease may expire while a request is in flight; its outcome is still accepted
// until a newer reservation or event replaces it. It also records success after
// sync canceled the event. Repeating the same success is harmless.
func (e *Engine) MarkSentClaim(claim Event, now time.Time) error {
	return e.change(func(s *State) error {
		ev := claimedEvent(s, claim)
		if ev == nil {
			return ErrStaleClaim
		}
		if ev.ClaimState == claimSent && ev.Status == "sent" {
			return nil
		}
		if !unsettledClaim(ev) {
			return ErrStaleClaim
		}
		markSent(ev, now)
		return nil
	})
}

// MarkFailedClaim records a failed transport for this exact reservation. A
// canceled event stays canceled; failure never exposes the transport message.
// Repeating the same failure is harmless until another claim is issued.
func (e *Engine) MarkFailedClaim(claim Event, message string, now time.Time) error {
	return e.change(func(s *State) error {
		ev := claimedEvent(s, claim)
		if ev == nil {
			return ErrStaleClaim
		}
		if ev.ClaimState == claimFailed {
			return nil
		}
		if !unsettledClaim(ev) {
			return ErrStaleClaim
		}
		_ = message // External transport errors may contain the private Bark key.
		if ev.Status == "canceled" {
			ev.ClaimState = claimFailed
			ev.ClaimUntil = time.Time{}
			ev.ClaimPriorError = ""
			return nil
		}
		markFailed(ev, now)
		return nil
	})
}

func claimedEvent(s *State, claim Event) *Event {
	if claim.ID == "" || claim.ClaimID == 0 {
		return nil
	}
	for i := range s.Events {
		if s.Events[i].ID == claim.ID && s.Events[i].ClaimID == claim.ClaimID {
			return &s.Events[i]
		}
	}
	return nil
}

func unsettledClaim(ev *Event) bool {
	return ev.ClaimState == claimActive && (ev.Status == "sending" || ev.Status == "pending" || ev.Status == "canceled" || ev.Status == "failed")
}

func returnUnsent(ev *Event) {
	if ev.Status == "failed" {
		ev.LastError = ev.ClaimPriorError
	}
	if ev.Status != "canceled" {
		ev.Status = "pending"
	}
	if ev.Attempts > 0 {
		ev.Attempts--
	}
	ev.NextAttemptAt = nil
	ev.ClaimState = claimReturned
	ev.ClaimUntil = time.Time{}
	ev.ClaimPriorError = ""
}

func markSent(ev *Event, now time.Time) {
	ev.Status = "sent"
	ev.SentAt = timePtr(now)
	ev.NextAttemptAt = nil
	ev.LastError = ""
	if ev.ClaimID != 0 {
		ev.ClaimState = claimSent
	}
	ev.ClaimUntil = time.Time{}
	ev.ClaimPriorError = ""
}

func markFailed(ev *Event, now time.Time) {
	ev.LastError = "通知发送失败，请检查 Bark 配置及网络"
	if ev.ClaimID != 0 {
		ev.ClaimState = claimFailed
	}
	ev.ClaimUntil = time.Time{}
	ev.ClaimPriorError = ""
	if ev.Attempts >= MaxDeliveryAttempts {
		ev.Status = "failed"
		ev.NextAttemptAt = nil
		return
	}
	delay := time.Minute * time.Duration(1<<uint(ev.Attempts-1))
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	ev.Status = "pending"
	ev.NextAttemptAt = timePtr(now.Add(delay))
}

// ClaimStillValidClaim checks ownership, the unexpired lease, and current
// eligibility immediately before transport. This is not atomic with network I/O.
func (e *Engine) ClaimStillValidClaim(claim Event, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	ev := claimedEvent(&e.state, claim)
	return ev != nil && ev.ClaimState == claimActive && !ev.ClaimUntil.IsZero() && ev.ClaimUntil.After(now) && claimEligible(&e.state, *ev, now)
}

// ClaimStillValid is the legacy ID-only preflight. It cannot distinguish an old
// sender from a newer claim; use ClaimStillValidClaim for concurrent senders.
func (e *Engine) ClaimStillValid(id string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.state.Events {
		if ev.ID == id {
			return claimEligible(&e.state, ev, now)
		}
	}
	return false
}

func claimEligible(s *State, ev Event, now time.Time) bool {
	if ev.Status != "sending" || !eventValid(s, ev, now) {
		return false
	}
	if IsQuiet(s.Settings, now) && !urgentEvent(ev) {
		return false
	}
	if ev.Kind == "fault" {
		return true
	}
	for _, a := range s.Accounts {
		if a.Platform == ev.Platform {
			if ev.Kind == "in_class" || ev.Kind == "rain_analysis" {
				return !a.NeedsRevalidation
			}
			return !a.NeedsRevalidation && a.SyncState == "success" && a.LastSuccessAt != nil && now.Sub(*a.LastSuccessAt) <= FreshnessLimit
		}
	}
	return false
}
