package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSingleClaimLeavesUnsentEventsUntouched(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	mustSync(t, e, []Task{homework("one", now.Add(time.Hour)), homework("two", now.Add(time.Hour))}, now)
	claims, err := e.PendingOneWithError(now)
	if err != nil || len(claims) != 1 {
		t.Fatalf("single claim: %v, %d", err, len(claims))
	}
	for _, event := range e.Snapshot().Events {
		if event.ID != claims[0].ID && event.Attempts != 0 {
			t.Fatal("later event spent an attempt before transport was ready")
		}
	}
	if err = e.ReturnUnsent(claims[0]); err != nil {
		t.Fatal(err)
	}
	for _, event := range e.Snapshot().Events {
		if event.Attempts != 0 || event.Status != "pending" {
			t.Fatal("unsent reservation was not returned")
		}
	}
	reopened, err := Open(e.path)
	if err != nil {
		t.Fatal(err)
	}
	mustSync(t, reopened, reopened.Snapshot().Tasks, now)
	if got := reopened.Pending(now); len(got) != 2 {
		t.Fatal("return did not survive a restart")
	}
}

func TestSingleClaimChoosesEarliestDue(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	mustSync(t, e, []Task{homework("later", now.Add(2*time.Hour))}, now.Add(time.Minute))
	mustSync(t, e, []Task{homework("later", now.Add(2*time.Hour)), homework("earlier", now.Add(2*time.Hour))}, now)
	claims, err := e.PendingOneWithError(now.Add(2 * time.Minute))
	if err != nil || len(claims) != 1 || claims[0].TaskID != "earlier" {
		t.Fatalf("did not claim the earliest due event: %v %+v", err, claims)
	}
}

func TestReturnUnsentDoesNotRefundOtherAttempts(t *testing.T) {
	for _, scenario := range []string{"newer-lease", "sent", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			e := openTest(t)
			now := stamp("2026-10-21T10:00:00+08:00")
			withoutQuiet(t, e, now)
			mustSync(t, e, []Task{homework("one", now.Add(time.Hour))}, now)
			claim := e.Pending(now)[0]
			switch scenario {
			case "newer-lease":
				if got := e.Pending(now.Add(3 * time.Minute)); len(got) != 1 {
					t.Fatal("lease did not expire")
				}
			case "sent":
				if err := e.MarkSent(claim.ID, now); err != nil {
					t.Fatal(err)
				}
			case "failed":
				if err := e.MarkFailed(claim.ID, "fixture", now); err != nil {
					t.Fatal(err)
				}
			}
			before := e.Snapshot().Events[0]
			if err := e.ReturnUnsent(claim); err != nil {
				t.Fatal(err)
			}
			after := e.Snapshot().Events[0]
			if before.Status != after.Status || before.Attempts != after.Attempts || !sameTime(before.NextAttemptAt, after.NextAttemptAt) {
				t.Fatal("stale release altered another or actual transport attempt")
			}
		})
	}
}

func TestReturnUnsentPersistenceFailurePreservesClaim(t *testing.T) {
	e := openTest(t)
	now := stamp("2026-10-21T10:00:00+08:00")
	withoutQuiet(t, e, now)
	mustSync(t, e, []Task{homework("one", now.Add(time.Hour))}, now)
	claim := e.Pending(now)[0]
	originalPath := e.path
	block := filepath.Join(t.TempDir(), "block")
	if err := os.WriteFile(block, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	e.path = filepath.Join(block, "state.json")
	if err := e.ReturnUnsent(claim); err == nil {
		t.Fatal("write failure hidden")
	}
	if event := e.Snapshot().Events[0]; event.Status != "sending" || event.Attempts != 1 {
		t.Fatal("failed write changed memory")
	}
	e.path = originalPath
	if err := e.ReturnUnsent(claim); err != nil {
		t.Fatal(err)
	}
}
