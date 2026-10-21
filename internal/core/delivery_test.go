package core

import (
	"path/filepath"
	"testing"
	"time"
)

func TestClaimInvalidatedBySyncBeforeDelivery(t *testing.T) {
	e, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := DefaultSettings()
	s.QuietEnabled = false
	if err = e.UpdateSettings(s); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	end := now.Add(time.Hour)
	task := Task{ID: "test", Title: "Test", Kind: "homework", Status: "pending", EndAt: &end}
	if err = e.Sync(PlatformRain, []Task{task}, now); err != nil {
		t.Fatal(err)
	}
	events := e.Pending(now)
	if len(events) != 1 || !e.ClaimStillValid(events[0].ID, now) {
		t.Fatal("missing valid claim")
	}
	task.Status = "completed"
	if err = e.Sync(PlatformRain, []Task{task}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if e.ClaimStillValid(events[0].ID, now.Add(time.Second)) {
		t.Fatal("completed task remained deliverable")
	}
}
