package core

import (
	"path/filepath"
	"testing"
	"time"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func quizItem(id string) InClassQuiz {
	return InClassQuiz{ClassroomID: "100", ActivityID: id, Course: "离散数学", Title: "随堂测验" + id, Body: "离散数学\n共 2 题", Notify: true}
}

func TestReportInClassNotifiesOnce(t *testing.T) {
	engine := testEngine(t)
	now := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	if err := engine.Sync(PlatformRain, nil, now); err != nil {
		t.Fatal(err)
	}
	added, err := engine.ReportInClass(PlatformRain, []InClassQuiz{quizItem("7")}, now)
	if err != nil || added != 1 {
		t.Fatalf("first report added=%d err=%v", added, err)
	}
	state := engine.Snapshot()
	if len(state.Events) != 1 || state.Events[0].Kind != "in_class" || state.Events[0].Title != "课上答题 · 随堂测验7" {
		t.Fatalf("unexpected events %+v", state.Events)
	}
	if len(state.InClassSeen) != 1 {
		t.Fatalf("seen keys %v", state.InClassSeen)
	}
	// Repeated polls of the same page must stay silent.
	if added, err = engine.ReportInClass(PlatformRain, []InClassQuiz{quizItem("7")}, now.Add(20*time.Second)); err != nil || added != 0 {
		t.Fatalf("repeat added=%d err=%v", added, err)
	}
	if len(engine.Snapshot().Events) != 1 {
		t.Fatal("duplicate notification")
	}
}

func TestReportInClassRecordsQuietBaseline(t *testing.T) {
	engine := testEngine(t)
	now := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	old := quizItem("8")
	old.Notify = false
	added, err := engine.ReportInClass(PlatformRain, []InClassQuiz{old}, now)
	if err != nil || added != 0 {
		t.Fatalf("baseline added=%d err=%v", added, err)
	}
	state := engine.Snapshot()
	if len(state.Events) != 0 {
		t.Fatalf("baseline must not notify: %+v", state.Events)
	}
	if _, seen := state.InClassSeen[PlatformRain+"\x00100\x008"]; !seen {
		t.Fatal("baseline activity must be remembered")
	}
}

func TestInClassBypassesQuietHours(t *testing.T) {
	engine := testEngine(t)
	// 23:30 in Asia/Shanghai, inside the default quiet window.
	now := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)
	if err := engine.Sync(PlatformRain, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReportInClass(PlatformRain, []InClassQuiz{quizItem("9")}, now); err != nil {
		t.Fatal(err)
	}
	pending := engine.Pending(now.Add(time.Second))
	if len(pending) != 1 || pending[0].Kind != "in_class" {
		t.Fatalf("in-class quiz must ignore quiet hours: %+v", pending)
	}
}

func TestInClassExpiresAfterWindow(t *testing.T) {
	engine := testEngine(t)
	now := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	if err := engine.Sync(PlatformRain, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReportInClass(PlatformRain, []InClassQuiz{quizItem("10")}, now); err != nil {
		t.Fatal(err)
	}
	if pending := engine.Pending(now.Add(InClassWindow + time.Minute)); len(pending) != 0 {
		t.Fatalf("expired quiz must not be delivered: %+v", pending)
	}
	if status := engine.Snapshot().Events[0].Status; status != "canceled" {
		t.Fatalf("expired event status %q", status)
	}
}

func TestInClassBlockedWhileAccountNeedsRevalidation(t *testing.T) {
	engine := testEngine(t)
	now := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	if err := engine.Sync(PlatformRain, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReportInClass(PlatformRain, []InClassQuiz{quizItem("11")}, now); err != nil {
		t.Fatal(err)
	}
	if err := engine.RequireRevalidation(); err != nil {
		t.Fatal(err)
	}
	if pending := engine.Pending(now.Add(time.Second)); len(pending) != 0 {
		t.Fatalf("revalidation must pause delivery: %+v", pending)
	}
}

func TestInClassRejectsIncompleteItems(t *testing.T) {
	engine := testEngine(t)
	now := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	for _, item := range []InClassQuiz{
		{ClassroomID: "", ActivityID: "1", Title: "x", Notify: true},
		{ClassroomID: "1", ActivityID: "", Title: "x", Notify: true},
		{ClassroomID: "1", ActivityID: "2", Title: "  ", Notify: true},
	} {
		if _, err := engine.ReportInClass(PlatformRain, []InClassQuiz{item}, now); err == nil {
			t.Fatalf("accepted incomplete item %+v", item)
		}
	}
	if _, err := engine.ReportInClass("unknown", nil, now); err == nil {
		t.Fatal("unknown platform must fail")
	}
}
