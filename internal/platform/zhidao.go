package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
)

type Zhidao struct {
	client  *http.Client
	session ZhidaoSession
	base    string
	initErr error
}
type zhidaoCourse struct {
	ID    string `json:"courseId"`
	Name  string `json:"courseName"`
	Type  string `json:"courseType"`
	Terms []int  `json:"terms"`
}

func NewZhidao(serialized string) *Zhidao {
	c, j := newZhidaoClient()
	z := &Zhidao{client: c, base: zhidaoCloud}
	if json.Unmarshal([]byte(serialized), &z.session) != nil || z.session.Token == "" {
		z.initErr = ErrSessionExpired
		return z
	}
	for _, record := range z.session.Cookies {
		u, err := url.Parse(record.URL)
		if err != nil || record.Cookie == nil {
			continue
		}
		h := u.Hostname()
		if u.Scheme == "https" && (h == "zhihuishu.com" || h == "polymas.com" || endsWithDomain(h, "zhihuishu.com") || endsWithDomain(h, "polymas.com")) {
			cookie := *record.Cookie
			// Older snapshots stored relative MaxAge without their receipt time.
			// Such cookies cannot be restored safely unless Expires is known.
			if cookie.MaxAge > 0 && cookie.Expires.IsZero() {
				continue
			}
			if cookie.MaxAge > 0 {
				cookie.MaxAge = 0
			}
			j.SetCookies(u, []*http.Cookie{&cookie})
		}
	}
	z.refreshToken()
	return z
}
func endsWithDomain(host, domain string) bool {
	return len(host) > len(domain) && host[len(host)-len(domain)-1:] == "."+domain
}
func (z *Zhidao) Validate(ctx context.Context) error {
	if z.initErr != nil {
		return z.initErr
	}
	z.refreshToken()
	err := validateZhidao(ctx, z.client, z.base, &z.session)
	if err == nil {
		z.refreshToken()
	}
	return err
}
func (z *Zhidao) request(ctx context.Context, method, path string, body, out any) error {
	z.refreshToken()
	err := zhidaoRequest(ctx, z.client, method, z.base+path, z.session.Token, body, out)
	if err == nil {
		z.refreshToken()
	}
	return err
}
func (z *Zhidao) courses(ctx context.Context) ([]zhidaoCourse, error) {
	if z.session.SchoolNID == "" {
		return nil, errors.New("知到未返回学校标识")
	}
	var courses []zhidaoCourse
	seen := map[string]bool{}
	var expectedTotal int64 = -1
	for page := 1; page <= 200; page++ {
		var result struct {
			Code int `json:"code"`
			Data struct {
				List  []zhidaoCourse `json:"list"`
				Total json.Number    `json:"total"`
			} `json:"data"`
		}
		err := z.request(ctx, "POST", "/student-course/student/index/queryStudentCourse", map[string]any{"keyword": "", "term": "", "schoolId": z.session.SchoolNID, "status": 2, "page": page, "size": 16}, &result)
		if err != nil {
			return nil, err
		}
		if result.Code != 200 || result.Data.List == nil {
			return nil, errors.New("知到课程列表读取失败")
		}
		total, err := result.Data.Total.Int64()
		if err != nil || total < 0 {
			return nil, errors.New("知到课程分页信息无效")
		}
		if expectedTotal >= 0 && total != expectedTotal {
			return nil, errors.New("知到课程总数在翻页期间变化，请稍后重试")
		}
		expectedTotal = total
		for _, course := range result.Data.List {
			if course.ID == "" || course.Name == "" || seen[course.ID] {
				return nil, errors.New("知到课程分页不完整或重复")
			}
			seen[course.ID] = true
			courses = append(courses, course)
		}
		if int64(len(courses)) == total {
			return courses, nil
		}
		if len(result.Data.List) == 0 || int64(len(courses)) > total {
			return nil, errors.New("知到课程分页不完整")
		}
	}
	return nil, errors.New("知到课程分页超过上限")
}
func (z *Zhidao) Tasks(ctx context.Context) ([]core.Task, error) {
	courses, err := z.courses(ctx)
	if err != nil {
		return nil, err
	}
	tasks := []core.Task{}
	for _, course := range courses {
		var list []core.Task
		switch course.Type {
		case "hikeAiCourse":
			list, err = z.aiTasks(ctx, course)
		case "spocCourse":
			list, err = z.legacyTasks(ctx, course)
		default:
			return nil, errors.New("知到包含待验证的课程类型，已保留上次同步结果")
		}
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, list...)
	}
	return tasks, nil
}

type zhidaoActivity struct {
	ID        string `json:"activityTaskId"`
	Title     string `json:"title"`
	Type      int    `json:"type"`
	Completed *bool  `json:"isCompleted"`
	Start     string `json:"startTime"`
	End       string `json:"endTime"`
}

func zhidaoTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	t, err := time.ParseInLocation("2006-01-02 15:04:05", value, loc)
	if err != nil {
		return nil
	}
	return &t
}

func zhidaoRawTime(start, end string) string {
	fields := []string{}
	if start != "" {
		fields = append(fields, "startTime="+start)
	}
	if end != "" {
		fields = append(fields, "endTime="+end)
	}
	return strings.Join(fields, ";")
}

func (z *Zhidao) aiTasks(ctx context.Context, course zhidaoCourse) ([]core.Task, error) {
	if len(course.Terms) == 0 {
		return nil, errors.New("知到课程未提供所属学期")
	}
	tasks := []core.Task{}
	seen := map[string]bool{}
	for _, term := range course.Terms {
		count := 0
		expectedTotal := -1
		for page := 1; page <= 500; page++ {
			var result struct {
				Code int `json:"code"`
				Data struct {
					Total *int             `json:"totalCount"`
					List  []zhidaoActivity `json:"activityTaskList"`
				} `json:"data"`
			}
			body := map[string]any{"term": term, "courseId": course.ID, "pageNum": page, "pageSize": 20, "keyword": "", "type": "", "status": []int{}, "completed": "", "filterType": "", "timeSortField": 0, "requestSource": 2}
			if err := z.request(ctx, "POST", "/student-course/activityTask/list", body, &result); err != nil {
				return nil, err
			}
			if result.Code != 200 || result.Data.List == nil || result.Data.Total == nil || *result.Data.Total < 0 {
				return nil, errors.New("知到任务列表读取失败")
			}
			if expectedTotal >= 0 && *result.Data.Total != expectedTotal {
				return nil, errors.New("知到任务总数在翻页期间变化，请稍后重试")
			}
			expectedTotal = *result.Data.Total
			for _, row := range result.Data.List {
				if row.ID == "" || row.Title == "" {
					return nil, errors.New("知到任务缺少标识或标题")
				}
				id := "zhidao:" + course.ID + ":" + strconv.Itoa(term) + ":" + row.ID
				if seen[id] {
					return nil, errors.New("知到任务分页重复")
				}
				seen[id] = true
				kind := ""
				switch row.Type {
				case 4:
					kind = "exam"
				case 5:
					kind = "homework"
				default:
					return nil, errors.New("知到出现待验证的任务类型")
				}
				status := "unknown"
				if row.Completed != nil {
					status = "pending"
					if *row.Completed {
						status = "completed"
					}
				}
				tasks = append(tasks, core.Task{ID: id, Platform: core.PlatformZhidaoCampus, Course: course.Name, Title: row.Title, Kind: kind, Status: status, StartAt: zhidaoTime(row.Start), EndAt: zhidaoTime(row.End), RawTime: zhidaoRawTime(row.Start, row.End), SourceTimezone: "Asia/Shanghai", SourceURL: "https://hike-teaching-center.polymas.com/agent-course-full/" + url.PathEscape(course.ID) + "/stu/task"})
			}
			count += len(result.Data.List)
			if count == expectedTotal {
				break
			}
			if len(result.Data.List) == 0 || count > expectedTotal || page == 500 {
				return nil, errors.New("知到任务分页不完整")
			}
		}
	}
	return tasks, nil
}
