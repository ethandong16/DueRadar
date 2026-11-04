package core

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClassroomAssistantsDefaultOff(t *testing.T) {
	s := testEngine(t).Snapshot().Settings
	if s.RainFallbackEnabled || s.RainAnalysisEnabled {
		t.Fatal("classroom actions must be opt-in")
	}
}

func TestClassroomReservationConcurrentAndDurable(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			reserved, err := e.ReserveClassroomAction(PlatformRain, "100", "200", "fallback", now)
			if err != nil {
				t.Error(err)
			}
			if reserved {
				winners.Add(1)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("reserved %d times", winners.Load())
	}
	reopened, err := Open(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if reserved, err := reopened.ReserveClassroomAction(PlatformRain, "100", "200", "fallback", now); err != nil || reserved {
		t.Fatalf("restart lost reservation: reserved=%v err=%v", reserved, err)
	}
	if reserved, err := reopened.ReserveClassroomAction(PlatformRain, "100", "200", "analysis", now); err != nil || !reserved {
		t.Fatalf("analysis and fallback must be independent: reserved=%v err=%v", reserved, err)
	}
	snapshot := reopened.Snapshot()
	clear(snapshot.ClassroomActions)
	if len(reopened.Snapshot().ClassroomActions) != 2 {
		t.Fatal("snapshot aliases action reservations")
	}
}

func TestClassroomReservationWriteFailureAndValidation(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	for _, parts := range [][4]string{
		{"unknown", "100", "200", "fallback"},
		{PlatformRain, "", "200", "fallback"},
		{PlatformRain, "100", "bad\x00id", "fallback"},
		{PlatformRain, "100", "200", "submit"},
	} {
		if ok, err := e.ReserveClassroomAction(parts[0], parts[1], parts[2], parts[3], now); ok || err == nil {
			t.Fatal("invalid reservation accepted")
		}
	}
	path := e.path
	block := filepath.Join(t.TempDir(), "block")
	if err := os.WriteFile(block, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	e.path = filepath.Join(block, "state.json")
	if ok, err := e.ReserveClassroomAction(PlatformRain, "100", "200", "fallback", now); ok || err == nil {
		t.Fatal("failed persistence authorized an action")
	}
	if len(e.Snapshot().ClassroomActions) != 0 {
		t.Fatal("failed save changed in-memory reservations")
	}
	e.path = path
	if ok, err := e.ReserveClassroomAction(PlatformRain, "100", "200", "fallback", now); !ok || err != nil {
		t.Fatalf("recovered storage cannot reserve: %v %v", ok, err)
	}
}

func TestClassroomReservationRetentionAndRebind(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	for _, p := range []string{PlatformRain, PlatformZhidaoCampus} {
		if _, err := e.ReserveClassroomAction(p, "100", "200", "analysis", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.ResetPlatformForRebind(PlatformRain); err != nil {
		t.Fatal(err)
	}
	if len(e.Snapshot().ClassroomActions) != 1 {
		t.Fatal("rebind cleared another platform's reservation")
	}
	if err := e.Maintain(now.Add(8 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(e.Snapshot().ClassroomActions) != 0 {
		t.Fatal("expired action history was not pruned")
	}
}

func analysisTestEngine(t *testing.T, now time.Time) *Engine {
	t.Helper()
	e := testEngine(t)
	settings := e.Snapshot().Settings
	settings.RainAnalysisEnabled = true
	if err := e.UpdateSettingsAt(settings, now); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(PlatformRain, nil, now); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestClassroomAnalysisUrgentAndLiveFreshness(t *testing.T) {
	// Inside default quiet hours, with a failed and stale ordinary task sync.
	now := time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC)
	e := analysisTestEngine(t, now.Add(-time.Hour))
	if err := e.Fail(PlatformRain, false, "fixture", now); err != nil {
		t.Fatal(err)
	}
	if err := e.ReportClassroomAnalysis("100", "200", "随堂题", "A: fixture", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	claims := e.Pending(now)
	if len(claims) != 1 || claims[0].Kind != "rain_analysis" {
		t.Fatalf("live analysis did not bypass quiet hours and stale task sync: %+v", claims)
	}
	if !e.ClaimStillValidClaim(claims[0], now) {
		t.Fatal("live analysis claim incorrectly requires a fresh ordinary task sync")
	}
	if err := e.RequireRevalidation(); err != nil {
		t.Fatal(err)
	}
	if e.ClaimStillValidClaim(claims[0], now) {
		t.Fatal("unvalidated account can send an analysis")
	}
}

func TestClassroomAnalysisExpiryAndCancellation(t *testing.T) {
	for _, mode := range []string{"deadline", "maximum age", "disable", "configuration", "problem closed"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			e := analysisTestEngine(t, now)
			if _, err := e.ReserveClassroomAction(PlatformRain, "100", "200", "analysis", now); err != nil {
				t.Fatal(err)
			}
			expires := now.Add(time.Hour)
			if mode == "deadline" {
				expires = now.Add(time.Second)
			}
			if err := e.ReportClassroomAnalysis("100", "200", "fixture", "A", expires, now); err != nil {
				t.Fatal(err)
			}
			claims := e.Pending(now)
			if len(claims) != 1 {
				t.Fatal("missing analysis claim")
			}
			checkAt := now
			switch mode {
			case "deadline":
				checkAt = expires
			case "maximum age":
				checkAt = now.Add(2 * time.Minute)
			case "disable":
				settings := e.Snapshot().Settings
				settings.RainAnalysisEnabled = false
				if err := e.UpdateSettingsAt(settings, now); err != nil {
					t.Fatal(err)
				}
			case "configuration":
				if err := e.CancelClassroomAnalyses(); err != nil {
					t.Fatal(err)
				}
			case "problem closed":
				if err := e.CancelClassroomAnalysis("100", "200"); err != nil {
					t.Fatal(err)
				}
			}
			if e.ClaimStillValidClaim(claims[0], checkAt) {
				t.Fatal("expired/canceled analysis claim still authorized")
			}
			if ok, err := e.ReserveClassroomAction(PlatformRain, "100", "200", "analysis", checkAt); ok || err != nil {
				t.Fatal("expiry/configuration reset the paid-call reservation")
			}
		})
	}
}

func TestClassroomAnalysisDisabledDoesNotEnqueue(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	if err := e.ReportClassroomAnalysis("100", "200", "fixture", "A", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if len(e.Snapshot().Events) != 0 {
		t.Fatal("analysis was enqueued without opt-in")
	}
}
