package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestRainLiveLessons(t *testing.T) {
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v3/classroom/on-lesson" || req.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
		if req.Header.Get("Cookie") != "sessionid=test-only-session" {
			t.Error("in-class check must be authenticated")
		}
		fmt.Fprint(w, `{"code":0,"msg":"OK","data":{"onLessonClassrooms":[{"classroomId":"100","classroomName":"A-01","courseName":"离散数学","lessonId":"9001","role":5},{"classroomId":101,"classroomName":"B-02","courseName":"","lessonId":"9002","role":"1"}]}}`)
	})
	lessons, err := r.LiveLessons(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lessons) != 2 {
		t.Fatalf("lessons=%d", len(lessons))
	}
	if lessons[0].ClassroomID != "100" || lessons[0].LessonID != "9001" || lessons[0].CourseName != "离散数学" || lessons[0].Role != 5 || !lessons[0].IsStudent() {
		t.Fatalf("unexpected lesson %+v", lessons[0])
	}
	if lessons[1].ClassroomID != "101" || lessons[1].CourseName != "B-02" || lessons[1].Role != 1 || lessons[1].IsStudent() {
		t.Fatalf("classroom name must fall back to the course name: %+v", lessons[1])
	}
}

func TestRainLiveLessonsErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		expired bool
	}{
		{"expired session", `{"code":1,"data":null}`, true},
		{"missing list", `{"code":0,"data":{}}`, false},
		{"missing identifier", `{"code":0,"data":{"onLessonClassrooms":[{"classroomId":0,"courseName":"x"}]}}`, false},
		{"wrong envelope", `{"errcode":0,"data":{"onLessonClassrooms":[]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rainTestClient(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.body) })
			_, err := r.LiveLessons(context.Background())
			if err == nil || errors.Is(err, ErrSessionExpired) != tc.expired {
				t.Fatalf("LiveLessons() error=%v want expired=%v", err, tc.expired)
			}
		})
	}
}

func TestLiveLessonIsStudent(t *testing.T) {
	for _, tc := range []struct {
		role    int
		student bool
	}{
		{0, true},
		{1, false},
		{2, false},
		{5, true},
	} {
		if got := (LiveLesson{Role: tc.role}).IsStudent(); got != tc.student {
			t.Errorf("role %d: IsStudent()=%t want %t", tc.role, got, tc.student)
		}
	}
}

func TestRainLiveActivities(t *testing.T) {
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v2/api/web/logs/learn/100" {
			t.Errorf("unexpected path %s", req.URL.Path)
		}
		q := req.URL.Query()
		if q.Get("actype") != "-1" || q.Get("page") != "0" || q.Get("offset") != "20" || q.Get("sort") != "-1" {
			t.Errorf("unexpected activity query %v", q)
		}
		fmt.Fprint(w, `{"errcode":0,"errmsg":"Success","data":{"activities":[{"type":5,"id":77,"courseware_id":88,"title":"随堂测验6","create_time":1792663153000,"deadline":1792671000000,"problem_count":2,"total_score":2},{"type":14,"id":78,"title":"第2章","create_time":1792664163000}]}}`)
	})
	activities, err := r.LiveActivities(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 2 {
		t.Fatalf("activities=%d", len(activities))
	}
	quiz := activities[0]
	if quiz.ID != "77" || quiz.Type != 5 || quiz.Title != "随堂测验6" || quiz.CoursewareID != "88" {
		t.Fatalf("unexpected activity %+v", quiz)
	}
	if !quiz.Answerable() {
		t.Error("type 5 must be answerable")
	}
	if got := quiz.CreatedAt.Format(time.RFC3339); got != "2026-10-22T09:59:13Z" {
		t.Fatalf("created at %s", got)
	}
	if quiz.Deadline == nil || quiz.Deadline.UTC().Format(time.RFC3339) != "2026-10-22T12:10:00Z" {
		t.Fatalf("deadline %v", quiz.Deadline)
	}
	if quiz.ProblemCount != 2 || quiz.TotalScore != 2 {
		t.Fatalf("quiz detail %+v", quiz)
	}
	if activities[1].Answerable() {
		t.Error("a lesson activity is not an answerable task")
	}
}

func TestRainLiveActivitiesErrors(t *testing.T) {
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"errcode":1,"errmsg":"denied","data":null}`)
	})
	if _, err := r.LiveActivities(context.Background(), "100"); err == nil {
		t.Fatal("rejected envelope must fail")
	}
	if _, err := r.LiveActivities(context.Background(), "not-a-number"); err == nil {
		t.Fatal("invalid classroom must fail before any request")
	}
	bad := rainTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"errcode":0,"data":{"activities":[{"type":5,"id":1,"title":"x","create_time":12}]}}`)
	})
	if _, err := bad.LiveActivities(context.Background(), "100"); err == nil {
		t.Fatal("implausible timestamp must fail")
	}
}

func TestRainLiveQuickAnswers(t *testing.T) {
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v3/lesson-summary/student/quick-answer-list" {
			t.Errorf("unexpected path %s", req.URL.Path)
		}
		q := req.URL.Query()
		if q.Get("lesson_id") != "9001" || q.Get("page") != "1" || q.Get("pageSize") != "10" {
			t.Errorf("unexpected quick-answer query %v", q)
		}
		fmt.Fprint(w, `{"code":0,"data":{"list":[{"id":77,"title":"","sendTime":1792663153000},{"id":78,"sendTime":"1792663163"}]}}`)
	})
	answers, err := r.LiveQuickAnswers(context.Background(), "9001")
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 || answers[0].ID != "77" || answers[0].SentAt.UTC().Format(time.RFC3339) != "2026-10-22T09:59:13Z" {
		t.Fatalf("answers=%+v", answers)
	}
	if answers[1].SentAt.UTC().Format(time.RFC3339) != "2026-10-22T09:59:23Z" {
		t.Fatalf("seconds timestamp was not normalized: %+v", answers[1])
	}
}

func TestRainLiveQuickAnswersErrors(t *testing.T) {
	r := rainTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"code":50000,"data":""}`)
	})
	if _, err := r.LiveQuickAnswers(context.Background(), "9001"); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expired quick-answer session error=%v", err)
	}
	if _, err := r.LiveQuickAnswers(context.Background(), "not-a-number"); err == nil {
		t.Fatal("invalid lesson id must fail before request")
	}
}
