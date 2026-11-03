package core

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fencingFixture(t *testing.T) (*Engine, Task, time.Time) {
	t.Helper()
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	task := homework("one", now.Add(48*time.Hour))
	mustSync(t, e, []Task{task}, now)
	return e, task, now
}

func fencingClaim(t *testing.T, e *Engine, now time.Time) Event {
	t.Helper()
	claims, err := e.PendingOneWithError(now)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %v, %+v", err, claims)
	}
	if claims[0].ClaimID == 0 {
		t.Fatal("claim lacks a generation")
	}
	return claims[0]
}

func fencingEvent(t *testing.T, e *Engine, id string) Event {
	t.Helper()
	for _, event := range e.Snapshot().Events {
		if event.ID == id {
			return event
		}
	}
	t.Fatalf("event %s not found", id)
	return Event{}
}

func TestClaimFencesSupersededTransportOutcomes(t *testing.T) {
	for _, outcome := range []string{"sent", "failed", "unsent"} {
		for _, currentOutcome := range []string{"sent", "failed"} {
			t.Run(outcome+"/current-"+currentOutcome, func(t *testing.T) {
				e, _, now := fencingFixture(t)
				old := fencingClaim(t, e, now)
				if !e.ClaimStillValidClaim(old, old.NextAttemptAt.Add(-time.Nanosecond)) {
					t.Fatal("live claim rejected before lease expiry")
				}
				if e.ClaimStillValidClaim(old, *old.NextAttemptAt) {
					t.Fatal("claim accepted at lease expiry")
				}
				current := fencingClaim(t, e, *old.NextAttemptAt)
				if old.ID != current.ID || current.ClaimID <= old.ClaimID || current.Attempts != old.Attempts+1 {
					t.Fatalf("bad reclaim: old=%+v current=%+v", old, current)
				}
				if e.ClaimStillValidClaim(old, *old.NextAttemptAt) || !e.ClaimStillValidClaim(current, *old.NextAttemptAt) {
					t.Fatal("preflight did not distinguish the reservations")
				}
				before := e.Snapshot()
				var err error
				switch outcome {
				case "sent":
					err = e.MarkSentClaim(old, now.Add(3*time.Minute))
				case "failed":
					err = e.MarkFailedClaim(old, "private-provider-error", now.Add(3*time.Minute))
				case "unsent":
					err = e.ReturnUnsent(old)
				}
				if !errors.Is(err, ErrStaleClaim) || !reflect.DeepEqual(before, e.Snapshot()) {
					t.Fatalf("stale outcome changed current state: %v", err)
				}
				if currentOutcome == "sent" {
					err = e.MarkSentClaim(current, now.Add(3*time.Minute))
				} else {
					err = e.MarkFailedClaim(current, "private-provider-error", now.Add(3*time.Minute))
				}
				if err != nil {
					t.Fatalf("current outcome: %v", err)
				}
				event := fencingEvent(t, e, current.ID)
				want := "sent"
				if currentOutcome == "failed" {
					want = "pending"
					if event.NextAttemptAt == nil || !event.NextAttemptAt.Equal(now.Add(5*time.Minute)) || strings.Contains(event.LastError, "private") {
						t.Fatalf("current failure backoff or sanitization: %+v", event)
					}
				}
				if event.Status != want || event.Attempts != 2 {
					t.Fatalf("current outcome corrupted: %+v", event)
				}
			})
		}
	}
}

func TestClaimGenerationPreventsReturnABAAndSurvivesRestart(t *testing.T) {
	e, task, now := fencingFixture(t)
	old := fencingClaim(t, e, now)
	if err := e.ReturnUnsent(old); err != nil {
		t.Fatal(err)
	}
	if err := e.ReturnUnsent(old); err != nil {
		t.Fatalf("duplicate return: %v", err)
	}
	current := fencingClaim(t, e, now)
	if old.Attempts != current.Attempts || !sameTime(old.NextAttemptAt, current.NextAttemptAt) || current.ClaimID <= old.ClaimID {
		t.Fatalf("fixture did not reproduce identical old lease values: old=%+v current=%+v", old, current)
	}
	before := e.Snapshot()
	if err := e.ReturnUnsent(old); !errors.Is(err, ErrStaleClaim) || !reflect.DeepEqual(before, e.Snapshot()) {
		t.Fatalf("old return refunded new claim: %v", err)
	}
	if err := e.ReturnUnsent(current); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(e.path)
	if err != nil {
		t.Fatal(err)
	}
	mustSync(t, reopened, []Task{task}, now)
	next := fencingClaim(t, reopened, now)
	if next.ClaimID <= current.ClaimID || next.Attempts != 1 {
		t.Fatalf("generation or attempt budget lost on restart: %+v", next)
	}
	if err := reopened.MarkSentClaim(current, now); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("previous process claim was accepted: %v", err)
	}
}

func TestClaimGenerationSurvivesEventRecreationAndPruning(t *testing.T) {
	e, task, now := fencingFixture(t)
	settings := e.Snapshot().Settings
	settings.DeadlineMinutes = []int{60}
	if err := e.UpdateSettingsAt(settings, now); err != nil {
		t.Fatal(err)
	}
	mustSync(t, e, []Task{task}, now)
	discovery := fencingClaim(t, e, now)
	if err := e.MarkSentClaim(discovery, now); err != nil {
		t.Fatal(err)
	}
	due := task.EndAt.Add(-time.Hour)
	mustSync(t, e, []Task{task}, due)
	old := fencingClaim(t, e, due)
	settings.DeadlineMinutes = nil
	if err := e.UpdateSettingsAt(settings, due); err != nil {
		t.Fatal(err)
	}
	settings.DeadlineMinutes = []int{60}
	if err := e.UpdateSettingsAt(settings, due); err != nil {
		t.Fatal(err)
	}
	if err := e.MarkSentClaim(old, due); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("recreated event accepted old outcome before new claim: %v", err)
	}
	current := fencingClaim(t, e, due)
	if old.ID != current.ID || current.ClaimID <= old.ClaimID {
		t.Fatalf("event recreation reused a claim: old=%+v current=%+v", old, current)
	}
	if err := e.ReturnUnsent(old); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("old claim returned after recreation: %v", err)
	}
	if err := e.MarkSentClaim(current, due); err != nil {
		t.Fatal(err)
	}
	if err := e.Maintain(due.Add(EventRetention + time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Add the same stable event ID after retention removes the terminal event.
	if err := e.change(func(s *State) error {
		addEvent(s, Event{ID: current.ID, TaskID: task.ID, Platform: PlatformRain, Kind: "new_task", DueAt: due, CreatedAt: due, Status: "pending"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newer := fencingClaim(t, e, due)
	if newer.ClaimID <= current.ClaimID {
		t.Fatal("pruning reused a claim generation")
	}
	if err := e.MarkSentClaim(current, due); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("pruned event claim affected recreated event: %v", err)
	}
}

func TestClaimOutcomesAfterCancellation(t *testing.T) {
	for _, outcome := range []string{"sent", "failed", "unsent"} {
		t.Run(outcome, func(t *testing.T) {
			e, task, now := fencingFixture(t)
			claim := fencingClaim(t, e, now)
			task.Status = "completed"
			mustSync(t, e, []Task{task}, now.Add(time.Second))
			if e.ClaimStillValidClaim(claim, now.Add(time.Second)) {
				t.Fatal("canceled claim remained sendable")
			}
			apply := func() error {
				switch outcome {
				case "sent":
					return e.MarkSentClaim(claim, now.Add(3*time.Minute))
				case "failed":
					return e.MarkFailedClaim(claim, "private-provider-error", now.Add(3*time.Minute))
				default:
					return e.ReturnUnsent(claim)
				}
			}
			if err := apply(); err != nil {
				t.Fatal(err)
			}
			before := fencingEvent(t, e, claim.ID)
			if err := apply(); err != nil || !reflect.DeepEqual(before, fencingEvent(t, e, claim.ID)) {
				t.Fatalf("repeated result was not idempotent: %v", err)
			}
			if outcome == "sent" {
				if before.Status != "sent" || before.SentAt == nil || before.Attempts != 1 {
					t.Fatalf("accepted push not recorded: %+v", before)
				}
			} else if before.Status != "canceled" || before.SentAt != nil || before.Attempts != map[string]int{"failed": 1, "unsent": 0}[outcome] {
				t.Fatalf("canceled result changed scheduling incorrectly: %+v", before)
			}
			if outcome != "sent" {
				if err := e.MarkSentClaim(claim, now); !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("conflicting result accepted: %v", err)
				}
			}
		})
	}
}

func TestClaimLateOutcomeAtRetryLimit(t *testing.T) {
	for _, outcome := range []string{"sent", "failed", "unsent"} {
		t.Run(outcome, func(t *testing.T) {
			e, _, now := fencingFixture(t)
			if err := e.change(func(s *State) error {
				s.Events[0].Attempts = MaxDeliveryAttempts - 1
				s.Events[0].LastError = "previous failure"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			claim := fencingClaim(t, e, now)
			if claims, err := e.PendingOneWithError(*claim.NextAttemptAt); err != nil || len(claims) != 0 {
				t.Fatalf("expired final claim was issued again: %v %+v", err, claims)
			}
			if event := fencingEvent(t, e, claim.ID); event.Status != "failed" || event.ClaimState != claimActive {
				t.Fatalf("automatic exhaustion lost active outcome: %+v", event)
			}
			var err error
			switch outcome {
			case "sent":
				err = e.MarkSentClaim(claim, now.Add(3*time.Minute))
			case "failed":
				err = e.MarkFailedClaim(claim, "private-provider-error", now.Add(3*time.Minute))
			default:
				err = e.ReturnUnsent(claim)
			}
			if err != nil {
				t.Fatal(err)
			}
			event := fencingEvent(t, e, claim.ID)
			switch outcome {
			case "sent":
				if event.Status != "sent" || event.SentAt == nil || event.LastError != "" {
					t.Fatalf("late success lost: %+v", event)
				}
			case "failed":
				if event.Status != "failed" || event.NextAttemptAt != nil || strings.Contains(event.LastError, "private") {
					t.Fatalf("late failure lost: %+v", event)
				}
			case "unsent":
				if event.Status != "pending" || event.Attempts != MaxDeliveryAttempts-1 || event.LastError != "previous failure" || event.NextAttemptAt != nil {
					t.Fatalf("late unsent refund lost: %+v", event)
				}
			}
		})
	}
}

func TestClaimLateOutcomeAfterExpiryWithoutReclaim(t *testing.T) {
	for _, outcome := range []string{"sent", "failed", "unsent"} {
		t.Run(outcome, func(t *testing.T) {
			e, _, now := fencingFixture(t)
			claim := fencingClaim(t, e, now)
			late := claim.NextAttemptAt.Add(time.Nanosecond)
			if e.ClaimStillValidClaim(claim, late) {
				t.Fatal("expired claim can start a new send")
			}
			var err error
			switch outcome {
			case "sent":
				err = e.MarkSentClaim(claim, late)
			case "failed":
				err = e.MarkFailedClaim(claim, "private", late)
			case "unsent":
				err = e.ReturnUnsent(claim)
			}
			if err != nil {
				t.Fatalf("unreplaced expired claim rejected: %v", err)
			}
			event := fencingEvent(t, e, claim.ID)
			if outcome == "sent" && event.Status != "sent" || outcome == "failed" && (event.Status != "pending" || event.NextAttemptAt == nil || !event.NextAttemptAt.Equal(late.Add(time.Minute))) || outcome == "unsent" && (event.Status != "pending" || event.Attempts != 0) {
				t.Fatalf("late outcome incorrect: %+v", event)
			}
		})
	}
}

func TestClaimOutcomeCommitFailureAndStaleAreDistinct(t *testing.T) {
	for _, outcome := range []string{"sent", "failed", "unsent", "canceled-failed"} {
		t.Run(outcome, func(t *testing.T) {
			e, task, now := fencingFixture(t)
			claim := fencingClaim(t, e, now)
			if outcome == "canceled-failed" {
				task.Status = "completed"
				mustSync(t, e, []Task{task}, now.Add(time.Second))
			}
			apply := func() error {
				switch outcome {
				case "sent":
					return e.MarkSentClaim(claim, now)
				case "failed", "canceled-failed":
					return e.MarkFailedClaim(claim, "private-provider-error", now)
				default:
					return e.ReturnUnsent(claim)
				}
			}
			before := e.Snapshot()
			originalPath := e.path
			block := filepath.Join(t.TempDir(), "block")
			if err := os.WriteFile(block, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			e.path = filepath.Join(block, "state.json")
			if err := apply(); err == nil || errors.Is(err, ErrStaleClaim) || !reflect.DeepEqual(before, e.Snapshot()) {
				t.Fatalf("failed commit not isolated: %v", err)
			}
			stale := claim
			stale.ClaimID++
			if err := e.MarkSentClaim(stale, now); !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("stale claim mistaken for storage failure: %v", err)
			}
			e.path = originalPath
			if err := apply(); err != nil {
				t.Fatalf("retry after storage recovery: %v", err)
			}
		})
	}
}

func TestLegacyReturnUnsentCannotRefundFencedClaim(t *testing.T) {
	e, _, now := fencingFixture(t)
	if err := e.change(func(s *State) error {
		s.Events[0].Status = "sending"
		s.Events[0].Attempts = 1
		s.Events[0].NextAttemptAt = timePtr(now.Add(deliveryLease))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	legacy := e.Snapshot().Events[0]
	if err := e.ReturnUnsent(legacy); err != nil {
		t.Fatalf("legacy return failed: %v", err)
	}
	current := fencingClaim(t, e, now)
	before := e.Snapshot()
	if err := e.ReturnUnsent(legacy); err != nil || !reflect.DeepEqual(before, e.Snapshot()) {
		t.Fatalf("legacy return affected fenced claim: %v", err)
	}
	if current.ClaimID == 0 || !sameTime(current.NextAttemptAt, legacy.NextAttemptAt) || current.Attempts != legacy.Attempts {
		t.Fatal("fixture did not recreate old lease identity")
	}
}

func TestClaimLegacyMigrationSequenceAndOverflow(t *testing.T) {
	e, task, now := fencingFixture(t)
	claim := fencingClaim(t, e, now)
	legacy := e.Snapshot()
	legacy.LastClaimID = 0
	for i := range legacy.Events {
		legacy.Events[i].ClaimID = 0
		legacy.Events[i].ClaimState = ""
		legacy.Events[i].ClaimUntil = time.Time{}
		legacy.Events[i].ClaimPriorError = ""
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(e.path)
	if err != nil {
		t.Fatal(err)
	}
	mustSync(t, reopened, []Task{task}, now)
	fresh := fencingClaim(t, reopened, now)
	if fresh.ClaimID != 1 || fresh.Attempts != 2 {
		t.Fatalf("legacy state did not get its first fenced claim: %+v", fresh)
	}
	if err := reopened.MarkSentClaim(Event{ID: claim.ID}, now); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("zero legacy generation accepted: %v", err)
	}
	if err := reopened.ReturnUnsent(fresh); err != nil {
		t.Fatal(err)
	}
	if err := reopened.change(func(s *State) error { s.LastClaimID = math.MaxUint64; return nil }); err != nil {
		t.Fatal(err)
	}
	before := reopened.Snapshot()
	if claims, err := reopened.PendingWithError(now); err == nil || len(claims) != 0 || !reflect.DeepEqual(before, reopened.Snapshot()) {
		t.Fatalf("generation overflow did not fail atomically: %v %+v", err, claims)
	}
}

func TestClaimLeaseDoesNotAliasSnapshots(t *testing.T) {
	e, _, now := fencingFixture(t)
	claim := fencingClaim(t, e, now)
	claim.ClaimUntil = now.Add(-time.Hour)
	if got := e.Pending(now); len(got) != 0 {
		t.Fatal("returned claim mutated the durable lease")
	}
	snapshot := e.Snapshot()
	snapshot.Events[0].ClaimUntil = now.Add(-time.Hour)
	fresh := e.Snapshot().Events[0]
	if !fresh.ClaimUntil.Equal(now.Add(deliveryLease)) || !e.ClaimStillValidClaim(fresh, now) {
		t.Fatal("snapshot mutated the durable claim deadline")
	}
}

func TestClaimTerminalRetentionIsBoundedFromLease(t *testing.T) {
	for _, scenario := range []string{"canceled", "exhausted"} {
		t.Run(scenario, func(t *testing.T) {
			e, task, now := fencingFixture(t)
			if err := e.change(func(s *State) error {
				s.Events[0].CreatedAt = now.Add(-EventRetention - time.Hour)
				if scenario == "exhausted" {
					s.Events[0].Attempts = MaxDeliveryAttempts - 1
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			claim := fencingClaim(t, e, now)
			if scenario == "canceled" {
				task.Status = "completed"
				mustSync(t, e, []Task{task}, now.Add(time.Second))
			} else if got, err := e.PendingOneWithError(*claim.NextAttemptAt); err != nil || len(got) != 0 {
				t.Fatalf("exhaustion: %v %+v", err, got)
			}
			if err := e.Maintain(now.Add(3 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := e.MarkSentClaim(claim, now.Add(3*time.Minute)); err != nil {
				t.Fatalf("maintenance pruned outstanding claim: %v", err)
			}
		})
	}
	e, task, now := fencingFixture(t)
	if err := e.change(func(s *State) error { s.Events[0].CreatedAt = now.Add(-EventRetention); return nil }); err != nil {
		t.Fatal(err)
	}
	claim := fencingClaim(t, e, now)
	task.Status = "completed"
	mustSync(t, e, []Task{task}, now)
	if err := e.Maintain(claim.ClaimUntil.Add(EventRetention + time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := e.MarkSentClaim(claim, now); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("old terminal claim was not pruned: %v", err)
	}
}

func TestClaimReturnAfterRevalidationAndExhaustionRestoresFailure(t *testing.T) {
	e, task, now := fencingFixture(t)
	if err := e.change(func(s *State) error {
		s.Events[0].Attempts = MaxDeliveryAttempts - 1
		s.Events[0].LastError = "previous failure"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	claim := fencingClaim(t, e, now)
	if err := e.RequireRevalidation(); err != nil {
		t.Fatal(err)
	}
	mustSync(t, e, []Task{task}, now)
	if events, err := e.PendingOneWithError(now); err != nil || len(events) != 0 {
		t.Fatalf("revalidation unexpectedly reclaimed exhausted event: %v %+v", err, events)
	}
	if err := e.ReturnUnsent(claim); err != nil {
		t.Fatal(err)
	}
	event := fencingEvent(t, e, claim.ID)
	if event.Status != "pending" || event.Attempts != MaxDeliveryAttempts-1 || event.LastError != "previous failure" {
		t.Fatalf("revalidation exhaustion lost prior failure: %+v", event)
	}
}

func TestResetPlatformForRebindIsolatedAndDurable(t *testing.T) {
	e, oldTask, now := fencingFixture(t)
	otherTask := homework("other", now.Add(48*time.Hour))
	if err := e.Sync(PlatformZhidaoCampus, []Task{otherTask}, now); err != nil {
		t.Fatal(err)
	}
	claim := fencingClaim(t, e, now)
	if claim.Platform != PlatformRain {
		t.Fatalf("expected Rain claim, got %+v", claim)
	}
	sentAt := now.Add(-time.Hour)
	if err := e.change(func(s *State) error {
		for i := range s.Accounts {
			if s.Accounts[i].Platform == PlatformRain {
				s.Accounts[i].Incident = 7
				s.Accounts[i].ConsecutiveFailures = 2
				s.Accounts[i].LastError = "old diagnostic"
				s.Accounts[i].FaultNotified = true
			}
		}
		s.Events = append(s.Events,
			Event{ID: "rain-sent-history", Platform: PlatformRain, Status: "sent", SentAt: timePtr(sentAt)},
			Event{ID: "rain-failed-history", Platform: PlatformRain, Status: "failed", Attempts: 6, LastError: "old failure"},
			Event{ID: "rain-pending", Platform: PlatformRain, Status: "pending"},
			Event{ID: "other-history", Platform: PlatformZhidaoCampus, Status: "failed", LastError: "keep"},
		)
		s.InClassSeen = map[string]time.Time{
			PlatformRain + "\x00class\x00old":          now,
			PlatformZhidaoCampus + "\x00class\x00keep": now,
			PlatformRain + "_other\x00class\x00keep":   now,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := e.Snapshot()
	originalPath := e.path
	block := filepath.Join(t.TempDir(), "block")
	if err := os.WriteFile(block, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	e.path = filepath.Join(block, "state.json")
	if err := e.ResetPlatformForRebind(PlatformRain); err == nil || !reflect.DeepEqual(before, e.Snapshot()) {
		t.Fatalf("failed reset was not atomic: %v", err)
	}
	e.path = originalPath
	if err := e.ResetPlatformForRebind("unknown"); err == nil || !reflect.DeepEqual(before, e.Snapshot()) {
		t.Fatalf("invalid platform changed state: %v", err)
	}
	if err := e.ResetPlatformForRebind(PlatformRain); err != nil {
		t.Fatal(err)
	}
	after := e.Snapshot()
	if len(after.Tasks) != 1 || after.Tasks[0].Platform != PlatformZhidaoCampus || after.Tasks[0].ID != otherTask.ID {
		t.Fatalf("platform tasks not isolated: %+v", after.Tasks)
	}
	for _, account := range after.Accounts {
		if account.Platform == PlatformRain {
			want := Account{Platform: PlatformRain, LoginState: "unbound", SyncState: "idle", NeedsRevalidation: true}
			if !reflect.DeepEqual(account, want) {
				t.Fatalf("Rain account not reset: %+v", account)
			}
		} else {
			for _, previous := range before.Accounts {
				if previous.Platform == account.Platform && !reflect.DeepEqual(previous, account) {
					t.Fatalf("other platform account changed: %+v", account)
				}
			}
		}
	}
	if len(after.InClassSeen) != 2 || after.InClassSeen[PlatformZhidaoCampus+"\x00class\x00keep"].IsZero() || after.InClassSeen[PlatformRain+"_other\x00class\x00keep"].IsZero() {
		t.Fatalf("seen keys not isolated: %+v", after.InClassSeen)
	}
	for _, previous := range before.Events {
		if previous.Platform == PlatformRain {
			for _, current := range after.Events {
				if current.ID == previous.ID {
					t.Fatalf("previous platform event survived reset: %+v", current)
				}
			}
		} else if current := fencingEvent(t, e, previous.ID); !reflect.DeepEqual(previous, current) {
			t.Fatalf("other platform event changed: before=%+v after=%+v", previous, current)
		}
	}
	if e.ClaimStillValidClaim(claim, now) {
		t.Fatal("previous binding claim remained sendable")
	}
	data, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted State
	if err := json.Unmarshal(data, &persisted); err != nil || !reflect.DeepEqual(after, persisted) {
		t.Fatalf("reset was not durable: %v", err)
	}
	for _, result := range []func() error{
		func() error { return e.MarkSentClaim(claim, now) },
		func() error { return e.MarkFailedClaim(claim, "private", now) },
		func() error { return e.ReturnUnsent(claim) },
	} {
		if err := result(); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("old in-flight outcome survived rebind: %v", err)
		}
	}
	// Whether restoring the old account after a failed vault write or syncing a
	// new account with the same platform-local ID, notifications start fresh.
	mustSync(t, e, []Task{oldTask}, now.Add(2*time.Second))
	if len(e.Snapshot().Tasks) != 2 {
		t.Fatal("platform snapshot could not be recovered by sync")
	}
	newClaims, err := e.PendingWithError(now.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var newClaim Event
	for _, event := range newClaims {
		if event.ID == claim.ID {
			newClaim = event
		}
	}
	if newClaim.ID != claim.ID || newClaim.ClaimID <= claim.ClaimID {
		t.Fatalf("recreated task reused reservation or did not notify: %+v", newClaim)
	}
	beforeStale := e.Snapshot()
	if err := e.MarkSentClaim(claim, now); !errors.Is(err, ErrStaleClaim) || !reflect.DeepEqual(beforeStale, e.Snapshot()) {
		t.Fatalf("old outcome contaminated new account: %v", err)
	}
}

func TestClaimRevalidationBlocksSendButAllowsUnreplacedOutcome(t *testing.T) {
	for _, outcome := range []string{"sent", "failed", "unsent", "reclaimed"} {
		t.Run(outcome, func(t *testing.T) {
			e, task, now := fencingFixture(t)
			claim := fencingClaim(t, e, now)
			if err := e.RequireRevalidation(); err != nil {
				t.Fatal(err)
			}
			if e.ClaimStillValidClaim(claim, now) {
				t.Fatal("revalidation left claim usable for a new send")
			}
			var err error
			switch outcome {
			case "sent":
				err = e.MarkSentClaim(claim, now)
			case "failed":
				err = e.MarkFailedClaim(claim, "private", now)
			case "unsent":
				err = e.ReturnUnsent(claim)
			case "reclaimed":
				mustSync(t, e, []Task{task}, now)
				current := fencingClaim(t, e, now)
				if current.ClaimID <= claim.ClaimID {
					t.Fatal("revalidation reused the old claim")
				}
				err = e.MarkSentClaim(claim, now)
				if !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("reclaimed reservation accepted old result: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unreplaced transport outcome rejected: %v", err)
			}
			event := fencingEvent(t, e, claim.ID)
			if outcome == "sent" && event.Status != "sent" || outcome == "failed" && (event.Status != "pending" || event.NextAttemptAt == nil) || outcome == "unsent" && (event.Status != "pending" || event.Attempts != 0) {
				t.Fatalf("revalidation outcome incorrect: %+v", event)
			}
		})
	}
}
