package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenMigratesLegacySettingsBeforeValidating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"version":1,"settings":{"quiet_enabled":true,"quiet_start":"23:00","quiet_end":"08:00","deadline_minutes":[1440],"exam_start_minutes":[60],"video_minutes":[4320]},"tasks":[],"events":[],"accounts":[]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := e.Snapshot().Settings
	if settings.InClassPollSeconds != DefaultInClassPollSeconds || settings.Timezone != "Asia/Shanghai" {
		t.Fatalf("legacy settings not migrated: %+v", settings)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The migration must be durable, not just visible in memory.
	var persisted State
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Settings.InClassPollSeconds != DefaultInClassPollSeconds || persisted.Settings.Timezone != "Asia/Shanghai" {
		t.Fatalf("migration was not persisted: %+v", persisted.Settings)
	}
}
