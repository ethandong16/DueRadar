package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeSMS struct {
	baseline int64
	code     string
	err      error
	waits    int
}

func (f *fakeSMS) Baseline(context.Context) (int64, error) { return f.baseline, nil }
func (f *fakeSMS) WaitForCode(_ context.Context, _ int64, _ time.Time) (string, error) {
	f.waits++
	return f.code, f.err
}

type unavailableSMS struct{}

func (unavailableSMS) Baseline(context.Context) (int64, error) {
	return 0, errors.New("sms module down")
}
func (unavailableSMS) WaitForCode(context.Context, int64, time.Time) (string, error) {
	return "", errors.New("sms module down")
}

func TestPasswordLoginSMSUnavailable(t *testing.T) {
	client, jar, _, _ := passwordMock(t, false, 0, 1)
	if _, err := passwordLogin(context.Background(), client, jar, "13800000000", "pw", unavailableSMS{}); !errors.Is(err, ErrSMSUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func passwordMock(t *testing.T, challenge bool, needAuth int, status int) (*http.Client, *zhidaoJar, *int, *string) {
	t.Helper()
	client, jar := newZhidaoClient()
	logins, checked := 0, ""
	client.Transport = zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Hostname() == "passport.zhihuishu.com" && r.URL.Path == "/user/validateAccountAndPassword":
			raw := r.FormValue("secretStr")
			decoded, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				t.Fatalf("secretStr is not base64: %v", err)
			}
			plain, err := url.PathUnescape(string(decoded))
			if err != nil || !strings.Contains(plain, `"account"`) || !strings.Contains(plain, `"password"`) {
				t.Fatalf("secretStr payload: %q (%v)", plain, err)
			}
			return zhsResponse(r, 200, `{"status":`+intString(status)+`,"uuid":"uuid-1","pwd":"one-time & pwd"}`, nil), nil
		case r.URL.Hostname() == "appcomm-user.zhihuishu.com" && strings.HasSuffix(r.URL.Path, "/checkNeedAuth"):
			if r.FormValue("uuid") != "uuid-1" {
				t.Fatalf("checkNeedAuth uuid = %q", r.FormValue("uuid"))
			}
			return zhsResponse(r, 200, `{"rt":{"needAuth":`+intString(needAuth)+`}}`, nil), nil
		case r.URL.Hostname() == "passport.zhihuishu.com" && r.URL.Path == "/login":
			if r.URL.Query().Get("pwd") != "one-time & pwd" {
				t.Fatalf("pwd not URL encoded: %q", r.URL.Query().Get("pwd"))
			}
			logins++
			if challenge && logins == 1 {
				return zhsResponse(r, 200, `<script>showCodeMsgPop('1234',0)</script>`, nil), nil
			}
			return zhsResponse(r, 302, "", http.Header{"Location": {r.URL.Query().Get("service")}}), nil
		case r.URL.Path == "/oauth/login/ssoLogin":
			return zhsResponse(r, 302, "", http.Header{"Location": {zhidaoCenter}, "Set-Cookie": {"jt-cas=test-token; Domain=.polymas.com; Path=/; Secure; HttpOnly"}}), nil
		case r.URL.Path == "/stu-hike/agent-course-hike/ai-course-center":
			return zhsResponse(r, 200, "page", nil), nil
		case r.URL.Path == "/user/user/v2/get-current-user-detail":
			if r.Header.Get("Authorization") != "test-token" {
				t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
			}
			return zhsResponse(r, 200, `{"code":200,"data":{"userNid":"user-1","schoolCertificationNid":"school","zhsUid":"u","zhsUuid":"uu"}}`, nil), nil
		case r.URL.Path == "/user/resendCode":
			if r.FormValue("phone") != "13800000000" {
				t.Fatalf("resend phone = %q", r.FormValue("phone"))
			}
			return zhsResponse(r, 200, `{"status":1}`, nil), nil
		case r.URL.Path == "/user/checkCode":
			checked = r.FormValue("code")
			return zhsResponse(r, 200, `{"status":1}`, nil), nil
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.String())
			return nil, nil
		}
	})
	return client, jar, &logins, &checked
}

func intString(v int) string {
	return strconv.Itoa(v)
}

func TestPasswordLoginDirect(t *testing.T) {
	client, jar, _, _ := passwordMock(t, false, 0, 1)
	sms := &fakeSMS{baseline: 10, code: "2468"}
	result, err := passwordLogin(context.Background(), client, jar, "13800000000", "pw", sms)
	if err != nil {
		t.Fatal(err)
	}
	var session ZhidaoSession
	if json.Unmarshal([]byte(result), &session) != nil || session.Token != "test-token" || session.UserNID != "user-1" || session.SchoolNID != "school" {
		t.Fatalf("session = %+v", session)
	}
	if sms.waits != 0 {
		t.Fatal("SMS polled without a challenge")
	}
}

func TestPasswordLoginWithSMSCode(t *testing.T) {
	client, jar, logins, checked := passwordMock(t, true, 0, 1)
	sms := &fakeSMS{baseline: 10, code: "2468"}
	result, err := passwordLogin(context.Background(), client, jar, "13800000000", "pw", sms)
	if err != nil {
		t.Fatal(err)
	}
	if *logins != 2 || sms.waits != 1 || *checked != "2468" {
		t.Fatalf("logins=%d waits=%d code=%q", *logins, sms.waits, *checked)
	}
	var session ZhidaoSession
	if json.Unmarshal([]byte(result), &session) != nil || session.Token != "test-token" {
		t.Fatal("session not returned after SMS verification")
	}
}

func TestPasswordLoginErrors(t *testing.T) {
	cases := []struct {
		name      string
		challenge bool
		needAuth  int
		status    int
		smsErr    error
		want      error
	}{
		{"wrong-password", false, 0, -1, nil, ErrPasswordRejected},
		{"captcha", false, 0, -4, nil, ErrCaptchaRequired},
		{"slide", false, 0, -9, nil, ErrSlideRequired},
		{"locked", false, 0, -6, nil, ErrAccountLocked},
		{"need-binding", false, 2, 1, nil, ErrNeedBinding},
		{"sms-timeout", true, 0, 1, errors.New("timeout"), ErrSMSTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, jar, _, _ := passwordMock(t, tc.challenge, tc.needAuth, tc.status)
			sms := &fakeSMS{baseline: 10, code: "2468", err: tc.smsErr}
			_, err := passwordLogin(context.Background(), client, jar, "13800000000", "pw", sms)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPasswordLoginRejectedCode(t *testing.T) {
	client, jar := newZhidaoClient()
	stage := 0
	client.Transport = zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/user/validateAccountAndPassword":
			return zhsResponse(r, 200, `{"status":1,"uuid":"u","pwd":"p"}`, nil), nil
		case strings.HasSuffix(r.URL.Path, "/checkNeedAuth"):
			return zhsResponse(r, 200, `{"rt":{"needAuth":0}}`, nil), nil
		case r.URL.Path == "/login":
			stage++
			if stage == 1 {
				return zhsResponse(r, 200, `<script>showCodeMsgPop('1234',0)</script>`, nil), nil
			}
			return zhsResponse(r, 200, "still asking", nil), nil
		case r.URL.Path == "/user/resendCode":
			return zhsResponse(r, 200, `{"status":1}`, nil), nil
		case r.URL.Path == "/user/checkCode":
			return zhsResponse(r, 200, `{"status":-1}`, nil), nil
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, nil
		}
	})
	sms := &fakeSMS{code: "0000"}
	_, err := passwordLogin(context.Background(), client, jar, "13800000000", "pw", sms)
	if !errors.Is(err, ErrSMSCodeRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestJSEncodeURI(t *testing.T) {
	got := jsEncodeURI(`a b/c?d=中{`)
	want := `a%20b/c?d=%E4%B8%AD%7B`
	if got != want {
		t.Fatalf("jsEncodeURI = %q, want %q", got, want)
	}
}
