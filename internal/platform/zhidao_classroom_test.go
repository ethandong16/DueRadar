package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

func zhidaoPollClient(t *testing.T, handler func(*http.Request) (*http.Response, error)) *Zhidao {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(zhidaoCloud)
	jar.SetCookies(u, []*http.Cookie{{Name: "ai-poly", Value: "classroom-token", Domain: ".polymas.com", Path: "/"}})
	z := &Zhidao{base: zhidaoCloud, client: &http.Client{Jar: jar}, session: ZhidaoSession{Token: "task-token"}}
	z.client.Transport = zhsRoundTrip(handler)
	return z
}

func TestZhidaoLivePollsUseOnlyReadOnlyEndpoints(t *testing.T) {
	var signalsBody map[string]any
	z := zhidaoPollClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "classroom-token" {
			t.Errorf("wrong classroom token on %s", r.URL.Path)
		}
		if r.Header.Get("Origin") != zhidaoClassroomOrigin {
			t.Errorf("missing classroom origin on %s", r.URL.Path)
		}
		for _, forbidden := range []string{"join", "enter", "studentVote", "submit", "sign"} {
			if strings.Contains(strings.ToLower(r.URL.Path), strings.ToLower(forbidden)) {
				t.Errorf("write endpoint contacted: %s", r.URL.Path)
			}
		}
		switch r.URL.Path {
		case "/student-course/lesson/live/list":
			if r.Method != "GET" {
				t.Errorf("live list method %s", r.Method)
			}
			return zhsResponse(r, 200, `{"code":200,"data":[
				{"lessonId":"a1B2c3D4e5","courseId":"course-1","term":20271,"courseName":"概率与统计"},
				{"lessonId":"124","courseId":"course-2","term":20271,"courseName":"已结束课程"}
			]}`, nil), nil
		case "/student-app/interaction/api/lesson/check/finish":
			if r.URL.Query().Get("lessonId") == "a1B2c3D4e5" && r.URL.Query().Get("courseId") == "course-1" && r.URL.Query().Get("term") == "20271" {
				return zhsResponse(r, 200, `{"code":200,"data":false}`, nil), nil
			}
			if r.URL.Query().Get("lessonId") == "124" {
				return zhsResponse(r, 200, `{"code":200,"data":true}`, nil), nil
			}
			t.Errorf("unexpected finish query %s", r.URL.RawQuery)
			return zhsResponse(r, 200, `{"code":500}`, nil), nil
		case "/student-app/interaction/waterfall/list-all":
			if r.Method != "POST" {
				t.Errorf("waterfall method %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&signalsBody); err != nil {
				t.Error(err)
			}
			return zhsResponse(r, 200, `{"code":200,"data":[
				{"signalCode":"IN_CLASS::TOOL-TEST::PUBLISH","message":"{\"msgTime\":1700000005,\"signal\":{\"signalCode\":\"IN_CLASS::TOOL-VOTE::PUBLISH\",\"content\":{\"bizId\":9001}}}","bizObj":{"title":"你支持哪种解法？"},"createTime":"1700000000000"},
				{"signalCode":"IN_CLASS::TOOL-NOTICE::PUBLISH","message":"not-json"},
				{"signalCode":"IN_CLASS::TOOL-VOTE::PUBLISH","message":{"signal":{"content":{"voteId":"9002"}},"title":"匿名投票"},"bizObj":{"title":"匿名投票"},"createTime":1700000001000},
				{"signalCode":"IN_CLASS::TOOL-VOTE::PUBLISH","message":"{\"signal\":{\"content\":{\"bizId\":9001}}}","bizObj":{"title":"重复"}},
				{"signalCode":"IN_CLASS::TOOL-VOTE::PUBLISH","message":"{\"signal\":{\"content\":{\"bizId\":9003}}}","bizObj":{"title":"口述投票"},"createTime":"2026-11-10 10:00:00"},
				{"signalCode":"IN_CLASS::TOOL-VOTE::PUBLISH","message":"{\"signal\":{\"content\":{\"bizId\":9004}}}","bizObj":{"title":"未知时间"},"createTime":"not-a-time"}
			]}`, nil), nil
		case "/student-app/api/vote/getVoteDetail":
			if r.URL.Query().Get("voteId") != "9001" || r.URL.Query().Get("courseId") != "course-1" || r.URL.Query().Get("term") != "20271" {
				t.Errorf("unexpected vote query %s", r.URL.RawQuery)
			}
			return zhsResponse(r, 200, `{"code":200,"data":{"voteId":9001,"title":"你支持哪种解法？","isAnonymous":true,"isFinish":0,"startTime":"2026-11-10 10:00:00","endTime":"2026-11-10 10:05:00","limitSeconds":300,"checkType":"single","options":[
				{"voteOptionId":1,"optionContent":" 赞成 ","selectedStudents":[{"studentId":"classmate"}]},
				{"voteOptionId":2,"optionContent":"反对"},
				{"voteOptionId":3,"optionCode":"C","optionContent":null}
			]}}`, nil), nil
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
			return nil, nil
		}
	})

	lessons, err := z.LivePollLessons(context.Background())
	if err != nil || len(lessons) != 1 || lessons[0].LessonID != "a1B2c3D4e5" || lessons[0].Course != "概率与统计" || lessons[0].Term != 20271 {
		t.Fatalf("lessons=%+v err=%v", lessons, err)
	}
	signals, err := z.LivePollSignals(context.Background(), lessons[0])
	if err != nil || len(signals) != 4 {
		t.Fatalf("signals=%+v err=%v", signals, err)
	}
	if signals[0].ID != "9001" || signals[0].Title != "你支持哪种解法？" || !signals[0].CreatedAt.Equal(time.Unix(1700000005, 0)) {
		t.Fatalf("first signal=%+v", signals[0])
	}
	if signals[1].ID != "9002" || signals[1].Title != "匿名投票" {
		t.Fatalf("second signal=%+v", signals[1])
	}
	// Quick votes carry an A/B/C option list even without optionContent text.
	if signals[2].ID != "9003" || !signals[2].CreatedAt.Equal(time.Date(2026, 11, 10, 10, 0, 0, 0, zhidaoPollZone)) {
		t.Fatalf("third signal=%+v", signals[2])
	}
	// An unrecognized timestamp is unknown, not a reason to reject the snapshot.
	if signals[3].ID != "9004" || !signals[3].CreatedAt.IsZero() {
		t.Fatalf("fourth signal=%+v", signals[3])
	}
	if signalsBody["lessonId"] != "a1B2c3D4e5" || signalsBody["queryType"] != "ALL" || signalsBody["courseId"] != "course-1" || signalsBody["term"] != float64(20271) {
		t.Fatalf("waterfall body=%v", signalsBody)
	}
	poll, err := z.PollDetail(context.Background(), lessons[0], "9001")
	if err != nil {
		t.Fatal(err)
	}
	if poll.ID != "9001" || poll.Title != "你支持哪种解法？" || !poll.Anonymous || !poll.ParticipationKnown || poll.Participated || !poll.EndKnown || !poll.EndAt.Equal(time.Date(2026, 11, 10, 10, 5, 0, 0, zhidaoPollZone)) {
		t.Fatalf("poll=%+v", poll)
	}
	if len(poll.Options) != 3 || poll.Options[0] != "赞成" || poll.Options[1] != "反对" || poll.Options[2] != "C" {
		t.Fatalf("options=%v", poll.Options)
	}
}

func TestZhidaoLivePollsRejectIncompleteSnapshots(t *testing.T) {
	t.Run("missing classroom credential", func(t *testing.T) {
		jar, _ := cookiejar.New(nil)
		u, _ := url.Parse(zhidaoCloud)
		jar.SetCookies(u, []*http.Cookie{{Name: "jt-cas", Value: "task-token", Domain: ".polymas.com", Path: "/"}})
		z := &Zhidao{base: zhidaoCloud, client: &http.Client{Jar: jar}}
		if _, err := z.LivePollLessons(context.Background()); !errors.Is(err, ErrZhidaoClassroomSession) {
			t.Fatalf("task token was used for classroom access: %v", err)
		}
	})
	t.Run("unknown lesson state", func(t *testing.T) {
		z := zhidaoPollClient(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/student-course/lesson/live/list" {
				return zhsResponse(r, 200, `{"code":200,"data":[{"lessonId":"123","courseId":"c","term":20271}]}`, nil), nil
			}
			return zhsResponse(r, 200, `{"code":200,"data":null}`, nil), nil
		})
		if _, err := z.LivePollLessons(context.Background()); err == nil {
			t.Fatal("unknown lesson state accepted")
		}
	})
	t.Run("vote without identity", func(t *testing.T) {
		z := zhidaoPollClient(t, func(r *http.Request) (*http.Response, error) {
			return zhsResponse(r, 200, `{"code":200,"data":[{"signalCode":"IN_CLASS::TOOL-VOTE::PUBLISH","message":"{\"signal\":{\"content\":{}}}"}]}`, nil), nil
		})
		lesson := ZhidaoPollLesson{LessonID: "123", CourseID: "c", Term: 20271}
		if _, err := z.LivePollSignals(context.Background(), lesson); err == nil {
			t.Fatal("vote without identity accepted")
		}
	})
	t.Run("mismatched detail identity", func(t *testing.T) {
		z := zhidaoPollClient(t, func(r *http.Request) (*http.Response, error) {
			return zhsResponse(r, 200, `{"code":200,"data":{"voteId":9999,"title":"other"}}`, nil), nil
		})
		lesson := ZhidaoPollLesson{LessonID: "123", CourseID: "c", Term: 20271}
		if _, err := z.PollDetail(context.Background(), lesson, "9001"); err == nil {
			t.Fatal("mismatched vote accepted")
		}
	})
	t.Run("expired session", func(t *testing.T) {
		z := zhidaoPollClient(t, func(r *http.Request) (*http.Response, error) {
			return zhsResponse(r, 401, ``, nil), nil
		})
		if _, err := z.LivePollLessons(context.Background()); !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("401 not reported as expired: %v", err)
		}
	})
	t.Run("unknown participation and timing", func(t *testing.T) {
		z := zhidaoPollClient(t, func(r *http.Request) (*http.Response, error) {
			return zhsResponse(r, 200, `{"code":200,"data":{"voteId":9001,"title":"vote","options":[]}}`, nil), nil
		})
		lesson := ZhidaoPollLesson{LessonID: "123", CourseID: "c", Term: 20271}
		poll, err := z.PollDetail(context.Background(), lesson, "9001")
		if err != nil || poll.ParticipationKnown || poll.EndKnown {
			t.Fatalf("unknown states must stay unknown: %+v %v", poll, err)
		}
	})
	t.Run("isFinish means the account has voted", func(t *testing.T) {
		z := zhidaoPollClient(t, func(r *http.Request) (*http.Response, error) {
			return zhsResponse(r, 200, `{"code":200,"data":{"voteId":9001,"title":"vote","isFinish":1,"startTime":"2026-11-10 10:00:00","limitSeconds":300,"options":[]}}`, nil), nil
		})
		lesson := ZhidaoPollLesson{LessonID: "123", CourseID: "c", Term: 20271}
		poll, err := z.PollDetail(context.Background(), lesson, "9001")
		if err != nil || !poll.ParticipationKnown || !poll.Participated {
			t.Fatalf("isFinish not read as participation: %+v %v", poll, err)
		}
		if !poll.EndKnown || !poll.EndAt.Equal(time.Date(2026, 11, 10, 10, 5, 0, 0, zhidaoPollZone)) {
			t.Fatalf("end time not derived from startTime and limitSeconds: %+v", poll)
		}
	})
}
