package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func rainTestClient(t *testing.T, handle http.HandlerFunc) *Rain {
	t.Helper()
	s := httptest.NewServer(handle)
	t.Cleanup(s.Close)
	r := NewRain("sessionid=test-only-session")
	r.baseURL = s.URL
	return r
}

func TestRainValidateIndependentAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
		expired    bool
	}{
		{"authenticated", `{"code":0,"data":{"id":123}}`, true, false},
		{"string identifier", `{"code":0,"data":{"id":"123"}}`, true, false},
		{"expired", `{"code":1,"data":null}`, false, true},
		{"missing user", `{"code":0,"data":{}}`, false, false},
		{"wrong envelope", `{"errcode":0,"data":{"id":123}}`, false, false},
		{"login HTML", `<html>login</html>`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/api/v3/user/basic-info" || req.Method != "GET" {
					t.Errorf("unexpected validation request: %s %s", req.Method, req.URL.Path)
				}
				fmt.Fprint(w, tc.body)
			})
			err := r.Validate(context.Background())
			if (err == nil) != tc.want || errors.Is(err, ErrSessionExpired) != tc.expired {
				t.Fatalf("Validate() = %v, want success=%v expired=%v", err, tc.want, tc.expired)
			}
		})
	}
}

func TestRainTasksPagedCompleteSnapshot(t *testing.T) {
	var paths []string
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		paths = append(paths, req.URL.Path)
		if req.Method != "GET" || req.Header.Get("Cookie") != "sessionid=test-only-session" {
			t.Error("request must be authenticated metadata GET")
		}
		switch req.URL.Path {
		case "/v2/api/web/courses/list":
			if req.URL.Query().Get("identity") != "2" {
				t.Error("wrong student identity")
			}
			fmt.Fprint(w, `{"errcode":0,"data":{"list":[{"classroom_id":100,"course":{"name":"Sample course"}},{"classroom_id":"101","course":{"name":"Empty course"}}]}}`)
		case "/v2/api/web/logs/learn/100":
			q := req.URL.Query()
			if q.Get("actype") != "-1" || q.Get("offset") != "20" || q.Get("sort") != "-1" {
				t.Error("unexpected activity query")
			}
			switch q.Get("page") {
			case "0":
				fmt.Fprint(w, `{"errcode":0,"data":{"IS_SIMPLE":false,"activities":[{"type":14,"id":900,"title":"Lecture"},{"type":5,"id":901,"courseware_id":"4681704","title":"Submitted exam","status":3,"deadline":1792717200000}],"has_more":true}}`)
			case "1":
				fmt.Fprint(w, `{"errcode":0,"data":{"IS_SIMPLE":false,"activities":[{"type":4,"id":902,"courseware_id":4681705,"title":"Assignment","status":1,"deadline":0},{"type":5,"id":903,"courseware_id":4681706,"title":"Unknown state","status":99}],"has_more":false}}`)
			default:
				t.Fatal("pagination went beyond final page")
			}
		case "/v2/api/web/logs/learn/101":
			fmt.Fprint(w, `{"errcode":0,"data":{"activities":[],"has_more":false}}`)
		default:
			t.Errorf("unexpected endpoint %s", req.URL.Path)
			http.NotFound(w, req)
		}
	})
	tasks, err := r.Tasks(context.Background())
	if err != nil || len(tasks) != 3 || len(paths) != 4 {
		t.Fatalf("Tasks() count=%d paths=%v err=%v", len(tasks), paths, err)
	}
	if tasks[0].Status != "completed" || tasks[0].Kind != "exam" || tasks[0].ID != "rain:100:901" || tasks[0].EndAt.UnixMilli() != 1792717200000 {
		t.Fatalf("wrong submitted exam mapping: %+v", tasks[0])
	}
	if tasks[1].Status != "pending" || tasks[1].Kind != "homework" || tasks[1].EndAt != nil || tasks[1].StartAt != nil {
		t.Fatalf("wrong assignment mapping: %+v", tasks[1])
	}
	if tasks[2].Status != "unknown" || tasks[2].EndAt != nil || tasks[2].SourceURL != "https://www.yuketang.cn/v2/web/studentLog/100" {
		t.Fatalf("unknown state or navigation mapping: %+v", tasks[2])
	}
}

func TestRainDoesNotAcceptPartialSnapshot(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"failed later page", `{"errcode":2,"data":{}}`},
		{"missing pagination", `{"errcode":0,"data":{"activities":[]}}`},
		{"missing list", `{"errcode":0,"data":{"has_more":false}}`},
		{"empty continuing page", `{"errcode":0,"data":{"activities":[],"has_more":true}}`},
		{"duplicate page", `{"errcode":0,"data":{"activities":[{"type":5,"id":1,"courseware_id":2,"title":"Sample"}],"has_more":true}}`},
		{"unverified cloud tree", `{"errcode":0,"data":{"activities":[{"type":15,"id":3,"title":"Sample"}],"has_more":false}}`},
		{"malformed cloud homework", `{"errcode":0,"data":{"activities":[{"type":19,"id":3,"title":"Sample"}],"has_more":false}}`},
		{"unknown activity type", `{"errcode":0,"data":{"activities":[{"type":999,"id":3,"title":"Sample"}],"has_more":false}}`},
		{"changed time unit", `{"errcode":0,"data":{"activities":[{"type":5,"id":3,"courseware_id":3,"title":"Sample","deadline":1792717200}],"has_more":false}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/v2/api/web/courses/list" {
					fmt.Fprint(w, `{"errcode":0,"data":{"list":[{"classroom_id":100,"course":{"name":"Sample"}}]}}`)
				} else if req.URL.Query().Get("page") == "0" {
					fmt.Fprint(w, `{"errcode":0,"data":{"activities":[{"type":5,"id":1,"courseware_id":2,"title":"Sample","status":0}],"has_more":true}}`)
				} else {
					fmt.Fprint(w, tc.body)
				}
			})
			tasks, err := r.Tasks(context.Background())
			if err == nil || tasks != nil {
				t.Fatalf("partial snapshot accepted: tasks=%v err=%v", tasks, err)
			}
		})
	}
}

func TestRainStatusSemanticsAndSimpleMode(t *testing.T) {
	for _, kind := range []int{4, 5} {
		for _, simple := range []bool{false, true} {
			for _, tc := range []struct {
				status *int
				want   string
			}{{nil, "unknown"}, {rainInt(0), "pending"}, {rainInt(1), "pending"}, {rainInt(2), "completed"}, {rainInt(3), "completed"}, {rainInt(-1), "unknown"}, {rainInt(4), "unknown"}} {
				a := rainActivity{ID: json.Number("1"), Type: &kind, Status: tc.status, Title: "Sample"}
				task, err := rainTask("1", "Sample", a, simple)
				want := tc.want
				if simple {
					want = "unknown"
				}
				if err != nil || task.Status != want {
					t.Fatalf("kind=%d simple=%t status=%v got=%s want=%s err=%v", kind, simple, tc.status, task.Status, want, err)
				}
			}
		}
	}
}

func TestRainRejectsRedirectWithoutLeakingCookie(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, target.URL, http.StatusFound) })
	if err := r.Validate(context.Background()); !errors.Is(err, ErrSessionExpired) || called {
		t.Fatalf("redirect followed=%t err=%v", called, err)
	}
}

func TestRainErrorsDoNotEchoResponseOrCredentials(t *testing.T) {
	secret := "private-session-never-echo"
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, `{"code":5,"message":%q,"data":null}`, secret)
	})
	r.cookie = "sessionid=" + secret
	err := r.Validate(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error: %v", err)
	}
	r.cookie += "\r\nInjected: " + secret
	err = r.Validate(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe header error: %v", err)
	}
}

func TestRainCancellation(t *testing.T) {
	r := NewRain("sessionid=test-only-session")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.client.Timeout = time.Millisecond
	if err := r.Validate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func rainInt(v int) *int { return &v }

func TestRainCloudMetadataAndDeadlineOverrides(t *testing.T) {
	requests := make(map[string]int)
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		requests[req.URL.Path]++
		if strings.HasPrefix(req.URL.Path, "/mooc-api/") {
			if req.Header.Get("xtbz") != "ykt" || req.Header.Get("classroom-id") != "100" {
				t.Error("cloud request missing platform/classroom context")
			}
		}
		switch req.URL.Path {
		case "/v2/api/web/courses/list":
			fmt.Fprint(w, `{"errcode":0,"data":{"list":[{"classroom_id":100,"course":{"name":"Sample"}}]}}`)
		case "/v2/api/web/logs/learn/100":
			fmt.Fprint(w, `{"errcode":0,"data":{"activities":[
			{"type":19,"id":1,"courseware_id":"11","title":"Completed homework","content":{"leaf_id":501,"sku_id":901,"score_d":1792717200000}},
			{"type":19,"id":2,"courseware_id":"12","title":"Late homework","content":{"leaf_id":502,"sku_id":901,"score_d":1792717200000}},
			{"type":20,"id":3,"courseware_id":"13","title":"Reset exam","content":{"leaf_id":503,"sku_id":901,"score_d":1792717200000}},
			{"type":19,"id":4,"courseware_id":"14","title":"Missing progress","content":{"leaf_id":504,"sku_id":901,"score_d":0}},
			{"type":19,"id":5,"courseware_id":"15","title":"Older duplicate","content":{"leaf_id":501,"sku_id":901,"score_d":1782717200000}}
			],"has_more":false}}`)
		case "/mooc-api/v1/lms/learn/course/pub_new_pro":
			if req.Method != http.MethodPost || req.Header.Get("X-CSRFToken") != "test-csrf" {
				t.Error("progress query must follow observed POST and CSRF contract")
			}
			var body struct {
				CID int      `json:"cid"`
				IDs []string `json:"new_id"`
			}
			if json.NewDecoder(req.Body).Decode(&body) != nil || body.CID != 100 || len(body.IDs) != 5 {
				t.Error("unexpected progress batch")
			}
			fmt.Fprint(w, `{"success":true,"data":{"11":{"501":{"done":21,"total":21}},"12":{"502":{"done":1,"total":2}},"13":{"503":{"status":2,"done":1,"total":2}},"15":{"501":0}}}`)
		case "/mooc-api/v1/lms/learn/course/reset_leaf":
			fmt.Fprint(w, `{"success":true,"data":[{"leaf_id":502,"reset_deadline":1792803600},{"leaf_id":503,"reset_deadline":"1792803600"}]}`)
		case "/mooc-api/v1/lms/learn/course/late_submission_leaf":
			fmt.Fprint(w, `{"success":true,"data":{"502":1792890000,"503":1792890000}}`)
		case "/mooc-api/v1/lms/learn/classroom_info/":
			fmt.Fprint(w, `{"success":true,"data":{"end":1792717200000}}`)
		default:
			t.Errorf("unexpected endpoint %s", req.URL.Path)
			http.NotFound(w, req)
		}
	})
	r.cookie += "; csrftoken=test-csrf"
	tasks, err := r.Tasks(context.Background())
	if err != nil || len(tasks) != 4 {
		t.Fatalf("cloud collection count=%d err=%v", len(tasks), err)
	}
	if tasks[0].ID != "rain:100:leaf:501" || tasks[0].Status != "completed" || tasks[0].EndAt.UnixMilli() != 1792717200000 {
		t.Fatalf("duplicate leaf or completed progress mismatch: %+v", tasks[0])
	}
	if tasks[1].Status != "pending" || tasks[1].EndAt.Unix() != 1792890000 || tasks[1].RawTime != "late_submission_deadline_s=1792890000" {
		t.Fatalf("late submission must override reset and original deadline: %+v", tasks[1])
	}
	if tasks[2].Status != "pending" || tasks[2].Kind != "exam" || tasks[2].EndAt.Unix() != 1792803600 || tasks[2].StartAt != nil {
		t.Fatalf("exam must use reset deadline but not homework late-submission: %+v", tasks[2])
	}
	if tasks[3].Status != "unknown" || tasks[3].EndAt.UnixMilli() != 1792717200000 || tasks[3].RawTime != "classroom.end_ms=1792717200000" {
		t.Fatalf("classroom fallback or missing-progress status mismatch: %+v", tasks[3])
	}
	if requests["/mooc-api/v1/lms/learn/course/reset_leaf"] != 1 || requests["/mooc-api/v1/lms/learn/classroom_info/"] != 1 {
		t.Fatalf("unnecessary duplicate metadata queries: %v", requests)
	}
}

func TestRainCloudStatusDoesNotGuessCompletion(t *testing.T) {
	for _, tc := range []struct {
		kind       int
		body, want string
	}{
		{19, `{"done":20,"total":20}`, "completed"},
		{19, `{"done":19,"total":20}`, "pending"},
		{19, `{"done":0,"total":0}`, "pending"},
		{19, `{"done":21,"total":20}`, "unknown"},
		{19, `{"done":-1,"total":20}`, "unknown"},
		{19, `{"done":20}`, "unknown"},
		{19, `null`, "unknown"},
		{19, `0`, "pending"},
		{20, `{"status":1}`, "pending"},
		{20, `{"status":2}`, "pending"},
		{20, `{"status":3}`, "completed"},
		{20, `{"status":4}`, "completed"},
		{20, `{"status":5}`, "completed"},
		{20, `{"status":6}`, "unknown"},
		{20, `{"done":20,"total":20}`, "unknown"},
	} {
		if got := rainCloudStatus(tc.kind, json.RawMessage(tc.body)); got != tc.want {
			t.Errorf("kind=%d input=%s got=%s want=%s", tc.kind, tc.body, got, tc.want)
		}
	}
}

func TestRainCloudMetadataFailureRejectsSnapshot(t *testing.T) {
	for _, failAt := range []string{"pub_new_pro", "reset_leaf", "late_submission_leaf", "classroom_info/"} {
		t.Run(failAt, func(t *testing.T) {
			r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if strings.HasSuffix(req.URL.Path, failAt) {
					fmt.Fprint(w, `{"success":false,"msg":"metadata unavailable","data":{}}`)
					return
				}
				switch req.URL.Path {
				case "/v2/api/web/courses/list":
					fmt.Fprint(w, `{"errcode":0,"data":{"list":[{"classroom_id":100,"course":{"name":"Sample"}}]}}`)
				case "/v2/api/web/logs/learn/100":
					fmt.Fprint(w, `{"errcode":0,"data":{"activities":[{"type":5,"id":1,"courseware_id":"11","title":"Legacy","status":0},{"type":19,"id":2,"courseware_id":"12","title":"Cloud","content":{"leaf_id":502,"sku_id":901}}],"has_more":false}}`)
				case "/mooc-api/v1/lms/learn/course/pub_new_pro":
					fmt.Fprint(w, `{"success":true,"data":{"12":{"502":{"done":1,"total":2}}}}`)
				case "/mooc-api/v1/lms/learn/course/reset_leaf":
					fmt.Fprint(w, `{"success":true,"data":[]}`)
				case "/mooc-api/v1/lms/learn/course/late_submission_leaf":
					fmt.Fprint(w, `{"success":true,"data":{}}`)
				default:
					t.Fatalf("unexpected endpoint: %s", req.URL.Path)
				}
			})
			tasks, err := r.Tasks(context.Background())
			if err == nil || tasks != nil {
				t.Fatalf("partial snapshot accepted after %s failure: %v %v", failAt, tasks, err)
			}
		})
	}
}
