package core

import (
	"testing"
	"time"
)

func local(clock string) time.Time {
	t, err := time.Parse(time.RFC3339, clock)
	if err != nil {
		panic(err)
	}
	return t
}

func TestSemesterBounds(t *testing.T) {
	for _, tc := range []struct {
		clock string
		start string
		key   int
	}{
		{"2026-09-01T12:00:00+08:00", "2026-09-01", 2026 * 2},
		{"2026-08-31T12:00:00+08:00", "2026-02-15", 2025*2 + 1},
		{"2027-01-10T12:00:00+08:00", "2026-09-01", 2026 * 2},
		{"2027-02-20T12:00:00+08:00", "2027-02-15", 2026*2 + 1},
	} {
		start, key := semesterBounds(local(tc.clock), "Asia/Shanghai")
		if got := start.Format("2006-01-02"); got != tc.start || key != tc.key {
			t.Fatalf("%s: got %s/%d want %s/%d", tc.clock, got, key, tc.start, tc.key)
		}
	}
	if start, _ := semesterBounds(local("2026-10-22T12:00:00Z"), "Not/AZone"); start.IsZero() {
		t.Fatal("invalid timezone must fall back to a usable location")
	}
}

func TestMarkHistoricalByCourse(t *testing.T) {
	now := local("2026-10-22T20:00:00+08:00")
	tasks := []Task{
		{ID: "old-hw", Platform: PlatformZhidaoCampus, Course: "2025-2026-2高等数学", EndAt: at("2026-06-01T23:59:00+08:00")},
		{ID: "old-video", Platform: PlatformZhidaoCampus, Course: "2025-2026-2高等数学"},
		{ID: "new-hw", Platform: PlatformZhidaoCampus, Course: "概率论-202609", EndAt: at("2026-11-08T23:30:00+08:00")},
		{ID: "new-video", Platform: PlatformZhidaoCampus, Course: "概率论-202609"},
		{ID: "named-old", Platform: PlatformZhidaoCampus, Course: "2024-2025-1大学物理"},
		{ID: "retake", Platform: PlatformZhidaoCampus, Course: "2025-2026-2线性代数", EndAt: at("2026-10-30T23:59:00+08:00")},
		{ID: "unknown", Platform: PlatformRain, Course: "毛泽东思想"},
		{ID: "no-course", Platform: PlatformRain},
	}
	markHistorical(tasks, now, "Asia/Shanghai")
	got := map[string]bool{}
	for _, task := range tasks {
		got[task.ID] = task.Historical
	}
	for id, want := range map[string]bool{
		"old-hw": true, "old-video": true, "new-hw": false, "new-video": false,
		"named-old": true, "retake": false, "unknown": false, "no-course": false,
	} {
		if got[id] != want {
			t.Fatalf("%s: historical=%t want %t", id, got[id], want)
		}
	}
}

func TestHistoricalTasksProduceAndKeepNoReminders(t *testing.T) {
	now := local("2026-10-22T20:00:00+08:00")
	e := openTest(t)
	video := Task{ID: "video-1", Title: "课程视频", Course: "公开课", Kind: "video", Status: "pending"}
	mustSync(t, e, []Task{video}, now)
	events := e.Snapshot().Events
	if len(events) != 1 || events[0].Kind != "new_task" || events[0].Status != "pending" {
		t.Fatalf("expected one pending new_task event, got %+v", events)
	}
	video.Course = "2025-2026-2公开课"
	mustSync(t, e, []Task{video}, now)
	snapshot := e.Snapshot()
	if !snapshot.Tasks[0].Historical {
		t.Fatal("previous semester course must be marked historical")
	}
	for _, ev := range snapshot.Events {
		if ev.Status == "pending" || ev.Status == "sending" {
			t.Fatalf("historical task kept an actionable event: %+v", ev)
		}
	}
	for _, ev := range e.Pending(now) {
		if ev.TaskID == "video-1" {
			t.Fatalf("historical task remained deliverable: %+v", ev)
		}
	}
}
