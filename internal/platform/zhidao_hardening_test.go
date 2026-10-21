package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

func TestZhidaoRejectsAbsentAndChangingTotals(t *testing.T) {
	for _, feed := range []string{"courses", "activities", "legacy"} {
		for _, scenario := range []string{"absent", "null", "changed"} {
			t.Run(feed+"/"+scenario, func(t *testing.T) {
				calls := 0
				z := &Zhidao{base: zhidaoCloud, session: ZhidaoSession{SchoolNID: "school"}, client: &http.Client{}}
				z.client.Transport = zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					var body string
					total := ""
					if scenario == "null" {
						total = `"%s":null,`
					} else if scenario == "changed" {
						n := 3
						if calls > 1 {
							n = 2
						}
						total = fmt.Sprintf(`"%%s":%d,`, n)
					}
					list := "[]"
					switch feed {
					case "courses":
						if scenario == "changed" {
							list = fmt.Sprintf(`[{"courseId":"c%d","courseName":"course"}]`, calls)
						}
						body = `{"code":200,"data":{` + optionalTotal(total, "total") + `"list":` + list + `}}`
					case "activities":
						if scenario == "changed" {
							list = fmt.Sprintf(`[{"activityTaskId":"t%d","title":"task","type":4}]`, calls)
						}
						body = `{"code":200,"data":{` + optionalTotal(total, "totalCount") + `"activityTaskList":` + list + `}}`
					case "legacy":
						if strings.Contains(r.URL.Path, "queryResourceTree") {
							body = `{"status":200,"rt":[]}`
							break
						}
						if scenario == "changed" {
							list = fmt.Sprintf(`[{"homeworkId":%d,"title":"task"}]`, calls)
						}
						body = `{"status":200,"rt":{` + optionalTotal(total, "rowCount") + `"list":` + list + `}}`
					}
					return zhsResponse(r, 200, body, nil), nil
				})
				var err error
				switch feed {
				case "courses":
					_, err = z.courses(context.Background())
				case "activities":
					_, err = z.aiTasks(context.Background(), zhidaoCourse{ID: "c", Terms: []int{1}})
				case "legacy":
					_, err = z.legacyTasks(context.Background(), zhidaoCourse{ID: "c"})
				}
				if err == nil {
					t.Fatal("incomplete snapshot accepted")
				}
			})
		}
	}
}

func optionalTotal(format, name string) string {
	if format == "" {
		return ""
	}
	return fmt.Sprintf(format, name)
}

func TestPlatformJSONResponseBoundaries(t *testing.T) {
	for _, platform := range []string{"zhidao", "rain-login"} {
		for _, body := range []string{`{"code":0} {"code":1}`, `{"code":0}` + strings.Repeat(" ", 8<<20)} {
			t.Run(platform, func(t *testing.T) {
				jar, _ := cookiejar.New(nil)
				client := &http.Client{Jar: jar, Transport: zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
					return zhsResponse(r, 200, body, nil), nil
				})}
				var out map[string]any
				var err error
				if platform == "zhidao" {
					err = zhidaoRequest(context.Background(), client, "GET", zhidaoCloud, "", nil, &out)
				} else {
					err = (&RainLogin{client: client, base: "https://www.yuketang.cn"}).request(context.Background(), "GET", "/", nil, &out)
				}
				if err == nil {
					t.Fatal("trailing or oversized response accepted")
				}
			})
		}
	}
}

type canceledReadCloser struct{ cancel context.CancelFunc }

func (r canceledReadCloser) Read([]byte) (int, error) { r.cancel(); return 0, context.Canceled }
func (r canceledReadCloser) Close() error             { return nil }

func TestPlatformRequestsPreserveCancellation(t *testing.T) {
	for _, platform := range []string{"zhidao", "rain-login", "rain"} {
		for _, phase := range []string{"request", "body"} {
			t.Run(platform+"/"+phase, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				jar, _ := cookiejar.New(nil)
				client := &http.Client{Jar: jar, Transport: zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
					if phase == "request" {
						cancel()
						return nil, ctx.Err()
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: canceledReadCloser{cancel}, Request: r}, nil
				})}
				var out map[string]any
				var err error
				switch platform {
				case "zhidao":
					err = zhidaoRequest(ctx, client, "GET", zhidaoCloud, "", nil, &out)
				case "rain-login":
					err = (&RainLogin{client: client, base: "https://www.yuketang.cn"}).request(ctx, "GET", "/", nil, &out)
				case "rain":
					_, err = (&Rain{client: client, baseURL: "https://www.yuketang.cn", cookie: "sessionid=test"}).get(ctx, "/")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			})
		}
	}
}

func TestZhidaoUnknownLegacyTimes(t *testing.T) {
	for _, value := range []json.Number{"", "0", "-1", "1792600000", "1", "999999999999999"} {
		if got := legacyTime(value); got != nil {
			t.Errorf("unknown/out-of-unit time %q invented %s", value, got)
		}
	}
	if got := legacyTime("1792600000000"); got == nil || got.Year() != 2026 {
		t.Fatal("valid milliseconds were lost")
	}
}

func TestZhidaoResourceMappingRejectsPartialTree(t *testing.T) {
	required := 1
	valid := zhidaoResource{ID: "1", Type: 1, DataType: 3, Name: "video", MustLearn: &required}
	for _, bad := range []zhidaoResource{
		{ID: "2", Type: 1, DataType: 3, Name: "unknown requirement"},
		valid,
	} {
		tasks, err := mapZhidaoResources(zhidaoCourse{ID: "c"}, []zhidaoResource{valid, bad})
		if err == nil || tasks != nil {
			t.Fatal("partial tree escaped as a successful/partial snapshot")
		}
	}
}

func TestZhidaoQRCancellationIsRecognizable(t *testing.T) {
	client, jar := newZhidaoClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Transport = zhsRoundTrip(func(r *http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"status":-1}`)), Request: r}, nil
	})
	flow := &ZhidaoLogin{client: client, jar: jar, userBase: "https://user.zhihuishu.com", pollInterval: time.Hour}
	if _, err := flow.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("QR cancellation lost: %v", err)
	}
}
