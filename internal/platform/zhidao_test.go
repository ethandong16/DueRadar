package platform

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type zhsRoundTrip func(*http.Request) (*http.Response, error)

func (f zhsRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func zhsResponse(r *http.Request, status int, body string, headers http.Header) *http.Response {
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func TestZhidaoQRSessionExchange(t *testing.T) {
	client, jar := newZhidaoClient()
	polls := 0
	client.Transport = zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/zhsuser/qrCodeLogin/getLoginQrInfo":
			polls++
			if polls == 1 {
				return zhsResponse(r, 200, `{"status":-1}`, nil), nil
			}
			return zhsResponse(r, 200, `{"status":1,"oncePassword":"one time & value"}`, nil), nil
		case "/login":
			if r.URL.Query().Get("pwd") != "one time & value" {
				t.Error("one-time password encoding")
			}
			return zhsResponse(r, 302, "", http.Header{"Location": {r.URL.Query().Get("service")}}), nil
		case "/oauth/login/ssoLogin":
			return zhsResponse(r, 302, "", http.Header{"Location": {zhidaoCenter}, "Set-Cookie": {"jt-cas=test-owned-session; Domain=.polymas.com; Path=/; Secure; HttpOnly"}}), nil
		case "/stu-hike/agent-course-hike/ai-course-center":
			return zhsResponse(r, 200, "page", nil), nil
		case "/user/user/v2/get-current-user-detail":
			if r.Header.Get("Authorization") != "test-owned-session" {
				t.Error("raw authorization contract")
			}
			return zhsResponse(r, 200, `{"code":200,"data":{"userNid":"u","schoolCertificationNid":"s","zhsUid":"42","zhsUuid":"uuid"}}`, nil), nil
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
			return nil, nil
		}
	})
	flow := &ZhidaoLogin{client: client, jar: jar, token: "qr", userBase: "https://user.zhihuishu.com", passportBase: "https://passport.zhihuishu.com", cloudBase: zhidaoCloud, pollInterval: time.Millisecond}
	result, err := flow.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var session ZhidaoSession
	if json.Unmarshal([]byte(result), &session) != nil || session.UserNID != "u" || session.ZhsUUID != "uuid" || len(session.Cookies) == 0 {
		t.Fatal("session was not preserved")
	}
}
func TestZhidaoQRExpiryAndCancellation(t *testing.T) {
	for _, status := range []int{2, -1} {
		client, jar := newZhidaoClient()
		client.Transport = zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
			body := `{"status":2}`
			if status == -1 {
				body = `{"status":-1}`
			}
			return zhsResponse(r, 200, body, nil), nil
		})
		flow := &ZhidaoLogin{client: client, jar: jar, userBase: "https://user.zhihuishu.com", pollInterval: time.Hour}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_, err := flow.Wait(ctx)
		cancel()
		if err == nil {
			t.Fatal("expected expiry/cancellation")
		}
	}
}
func TestZhidaoLoginRejectsExternalRedirect(t *testing.T) {
	client, _ := newZhidaoClient()
	client.Transport = zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
		return zhsResponse(r, 302, "", http.Header{"Location": {"https://unexpected.example/"}}), nil
	})
	if _, err := client.Get("https://passport.zhihuishu.com/login"); err == nil {
		t.Fatal("external redirect accepted")
	}
}
func TestZhidaoPaginatedActivitiesAndUnknownValues(t *testing.T) {
	z := &Zhidao{base: zhidaoCloud, session: ZhidaoSession{Token: "test", SchoolNID: "school"}, client: &http.Client{}}
	z.client.Transport = zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
		var input map[string]any
		json.NewDecoder(r.Body).Decode(&input)
		if r.Header.Get("Authorization") != "test" {
			t.Error("missing token")
		}
		if r.URL.Path == "/student-course/student/index/queryStudentCourse" {
			return zhsResponse(r, 200, `{"code":200,"data":{"total":"1","list":[{"courseId":"c","courseName":"course","courseType":"hikeAiCourse","terms":[20271]}]}}`, nil), nil
		}
		if input["completed"] != "" {
			t.Error("must include completed tasks")
		}
		if input["pageNum"] == float64(1) {
			return zhsResponse(r, 200, `{"code":200,"data":{"totalCount":2,"activityTaskList":[{"activityTaskId":"a","title":"exam","type":4,"isCompleted":true,"startTime":"2026-11-01 07:00:00","endTime":"2026-11-01 08:00:00"}]}}`, nil), nil
		}
		return zhsResponse(r, 200, `{"code":200,"data":{"totalCount":2,"activityTaskList":[{"activityTaskId":"b","title":"work","type":5,"endTime":"bad"}]}}`, nil), nil
	})
	tasks, err := z.Tasks(context.Background())
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks %d: %v", len(tasks), err)
	}
	if tasks[0].Status != "completed" || tasks[0].StartAt.UTC().Hour() != 23 || tasks[1].Status != "unknown" || tasks[1].EndAt != nil || tasks[1].RawTime != "endTime=bad" {
		t.Fatal("mapping invented missing fields")
	}
}
func TestZhidaoRejectsPartialTaskSnapshot(t *testing.T) {
	z := &Zhidao{base: zhidaoCloud, client: &http.Client{Transport: zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
		return zhsResponse(r, 200, `{"code":200,"data":{"totalCount":2,"activityTaskList":[]}}`, nil), nil
	})}}
	if _, err := z.aiTasks(context.Background(), zhidaoCourse{ID: "c", Terms: []int{1}}); err == nil {
		t.Fatal("partial page accepted")
	}
}
func TestZhidaoVideoCompletionAndUnknownDeadline(t *testing.T) {
	total, studied := 100.0, 80.0
	required := 1
	rows := []zhidaoResource{{ID: "a", Type: 1, DataType: 3, Name: "video", MustLearn: &required, Total: &total, Studied: &studied}, {ID: "b", Type: 1, DataType: 3, Name: "unknown", MustLearn: &required}}
	tasks, err := mapZhidaoResources(zhidaoCourse{ID: "c", Name: "course"}, rows)
	if err != nil || len(tasks) != 2 {
		t.Fatal(err)
	}
	if tasks[0].Status != "completed" || *tasks[0].Progress != 80 || tasks[0].EndAt != nil || tasks[1].Status != "unknown" {
		t.Fatal("video progress/deadline mapping")
	}
}
