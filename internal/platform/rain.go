package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
)

var ErrSessionExpired = errors.New("平台会话已失效，请重新登录")

// Rain reads the ordinary YukeTang student activity feed. It never opens an
// assignment or exam, or calls an answer, submission, or learning-progress API.
type Rain struct {
	cookie  string
	baseURL string
	client  *http.Client
	jar     *rainCookieJar
	once    sync.Once
	initErr error
}

func NewRain(cookie string) *Rain {
	return &Rain{
		cookie:  strings.TrimSpace(cookie),
		baseURL: rainOrigin,
		client: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type rainEnvelope struct {
	Code    *int            `json:"code"`
	ErrCode *int            `json:"errcode"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// Validate checks the independent user endpoint, not whether a course list is
// nonempty. An authenticated student may legitimately have no active courses.
func (r *Rain) Validate(ctx context.Context) error {
	env, err := r.get(ctx, "/api/v3/user/basic-info")
	if err != nil {
		return err
	}
	if env.Code == nil {
		return errors.New("雨课堂登录响应格式变化")
	}
	if *env.Code != 0 {
		return ErrSessionExpired
	}
	var user struct {
		ID json.Number `json:"id"`
	}
	if json.Unmarshal(env.Data, &user) != nil || !rainPositiveID(user.ID) {
		return errors.New("雨课堂登录响应缺少用户标识")
	}
	return nil
}

type rainCourse struct {
	ClassroomID json.Number `json:"classroom_id"`
	Course      struct {
		Name string `json:"name"`
	} `json:"course"`
}

type rainActivity struct {
	ID           json.Number `json:"id"`
	CoursewareID json.Number `json:"courseware_id"`
	Type         *int        `json:"type"`
	Title        string      `json:"title"`
	Status       *int        `json:"status"`
	Deadline     *int64      `json:"deadline"`
	Content      struct {
		LeafID        json.Number `json:"leaf_id"`
		SKUID         json.Number `json:"sku_id"`
		ScoreDeadline *int64      `json:"score_d"`
	} `json:"content"`
}

// Tasks succeeds only after every course and every page has been read. Schema
// errors, pagination loops, and unimplemented nested task feeds fail the whole
// snapshot, preserving the last successfully synchronized task state.
func (r *Rain) Tasks(ctx context.Context) ([]core.Task, error) {
	env, err := r.get(ctx, "/v2/api/web/courses/list?identity=2")
	if err != nil {
		return nil, err
	}
	if err = rainCheckListEnvelope(env); err != nil {
		return nil, err
	}
	var courses struct {
		List *[]rainCourse `json:"list"`
	}
	if json.Unmarshal(env.Data, &courses) != nil || courses.List == nil {
		return nil, errors.New("雨课堂课程列表格式变化")
	}
	if len(*courses.List) > 500 {
		return nil, errors.New("雨课堂课程数量超过单次同步上限")
	}
	tasks := make([]core.Task, 0)
	classrooms := make(map[string]bool)
	for _, course := range *courses.List {
		if !rainPositiveID(course.ClassroomID) || strings.TrimSpace(course.Course.Name) == "" {
			return nil, errors.New("雨课堂课程缺少标识或名称")
		}
		cid := course.ClassroomID.String()
		if classrooms[cid] {
			return nil, errors.New("雨课堂课程列表包含重复班级")
		}
		classrooms[cid] = true
		classTasks, err := r.courseTasks(ctx, cid, course.Course.Name)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, classTasks...)
		if len(tasks) > 10000 {
			return nil, errors.New("雨课堂任务数量超过单次同步上限")
		}
	}
	return tasks, nil
}

func (r *Rain) courseTasks(ctx context.Context, cid, courseName string) ([]core.Task, error) {
	tasks := make([]core.Task, 0)
	cloud := make([]rainActivity, 0)
	seen := make(map[string]bool)
	for page := 0; page < 500; page++ {
		q := url.Values{"actype": {"-1"}, "page": {strconv.Itoa(page)}, "offset": {"20"}, "sort": {"-1"}}
		env, err := r.get(ctx, "/v2/api/web/logs/learn/"+cid+"?"+q.Encode())
		if err != nil {
			return nil, err
		}
		if err = rainCheckListEnvelope(env); err != nil {
			return nil, err
		}
		var data struct {
			Activities *[]rainActivity `json:"activities"`
			HasMore    *bool           `json:"has_more"`
			IsSimple   bool            `json:"IS_SIMPLE"`
		}
		if json.Unmarshal(env.Data, &data) != nil || data.Activities == nil || data.HasMore == nil {
			return nil, errors.New("雨课堂任务列表格式变化")
		}
		if len(*data.Activities) == 0 && *data.HasMore {
			return nil, errors.New("雨课堂任务分页未推进")
		}
		for _, activity := range *data.Activities {
			if activity.Type == nil || !rainPositiveID(activity.ID) {
				return nil, errors.New("雨课堂活动缺少类型或标识")
			}
			id := activity.ID.String()
			if seen[id] {
				return nil, errors.New("雨课堂活动分页发生重复，请稍后重试")
			}
			seen[id] = true
			switch *activity.Type {
			case 4, 5:
				if !rainPositiveID(activity.CoursewareID) || strings.TrimSpace(activity.Title) == "" {
					return nil, errors.New("雨课堂任务缺少标识或标题")
				}
				task, err := rainTask(cid, courseName, activity, data.IsSimple)
				if err != nil {
					return nil, err
				}
				tasks = append(tasks, task)
			case 19, 20:
				if !rainPositiveID(activity.CoursewareID) || !rainPositiveID(activity.Content.LeafID) || !rainPositiveID(activity.Content.SKUID) || strings.TrimSpace(activity.Title) == "" {
					return nil, errors.New("雨课堂云端任务缺少标识或标题")
				}
				cloud = append(cloud, activity)
			case 15:
				// Cloud trees and cloud assignment/exam statuses need additional
				// independently verified endpoints; a partial list is not success.
				return nil, errors.New("此雨课堂课程包含待接入的云端作业或考试，已保留上次同步结果")
			case 2, 3, 6, 8, 9, 13, 14, 16, 17, 18, 30, 100:
				// Observed courseware, lectures, announcements, discussions,
				// media and experiments are outside ordinary rain task kinds.
			default:
				return nil, errors.New("雨课堂出现尚未识别的活动类型，已保留上次同步结果")
			}
		}
		if !*data.HasMore {
			cloudTasks, err := r.cloudTasks(ctx, cid, courseName, cloud)
			if err != nil {
				return nil, err
			}
			return append(tasks, cloudTasks...), nil
		}
	}
	return nil, errors.New("雨课堂任务分页超过单次同步上限")
}

// cloudTasks reads the progress shown alongside cloud assignment/exam cards.
// pub_new_pro is a metadata query using POST; it does not mark work as viewed,
// answered, or submitted. The frontend uses classroom-id and xtbz headers.
func (r *Rain) cloudTasks(ctx context.Context, cid, courseName string, activities []rainActivity) ([]core.Task, error) {
	tasks := make([]core.Task, 0, len(activities))
	if len(activities) == 0 {
		return tasks, nil
	}
	schedules := make(map[string]map[string]json.RawMessage)
	for start := 0; start < len(activities); start += 20 {
		end := min(start+20, len(activities))
		newsIDs := make([]string, 0, end-start)
		for _, a := range activities[start:end] {
			newsIDs = append(newsIDs, a.CoursewareID.String())
		}
		body, _ := json.Marshal(map[string]any{"cid": json.Number(cid), "new_id": newsIDs})
		env, err := r.request(ctx, http.MethodPost, "/mooc-api/v1/lms/learn/course/pub_new_pro", cid, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if err = rainCloudEnvelope(env); err != nil {
			return nil, err
		}
		var page map[string]map[string]json.RawMessage
		if json.Unmarshal(env.Data, &page) != nil || page == nil {
			return nil, errors.New("雨课堂云端进度格式变化")
		}
		for id, value := range page {
			schedules[id] = value
		}
	}
	reset, late := make(map[string]json.Number), make(map[string]json.Number)
	skus := make(map[string]bool)
	for _, activity := range activities {
		sku := activity.Content.SKUID.String()
		if skus[sku] {
			continue
		}
		skus[sku] = true
		for _, kind := range []string{"reset_leaf", "late_submission_leaf"} {
			env, err := r.request(ctx, http.MethodGet, "/mooc-api/v1/lms/learn/course/"+kind+"?sku_id="+sku, cid, nil)
			if err != nil {
				return nil, err
			}
			if err = rainCloudEnvelope(env); err != nil {
				return nil, err
			}
			if kind == "reset_leaf" {
				var entries []struct {
					LeafID   json.Number `json:"leaf_id"`
					Deadline json.Number `json:"reset_deadline"`
				}
				if json.Unmarshal(env.Data, &entries) != nil || entries == nil {
					return nil, errors.New("雨课堂重置截止时间格式变化")
				}
				for _, entry := range entries {
					if !rainPositiveID(entry.LeafID) {
						return nil, errors.New("雨课堂重置截止时间缺少任务标识")
					}
					reset[entry.LeafID.String()] = entry.Deadline
				}
			} else {
				var entries map[string]json.Number
				if json.Unmarshal(env.Data, &entries) != nil || entries == nil {
					return nil, errors.New("雨课堂补交截止时间格式变化")
				}
				for id, deadline := range entries {
					late[id] = deadline
				}
			}
		}
	}
	seenLeaves := make(map[string]bool)
	var classDeadline *int64
	classDeadlineLoaded := false
	for _, activity := range activities {
		leafID := activity.Content.LeafID.String()
		// Re-publication may repeat a leaf. The activity feed is newest first;
		// use its current metadata once and keep a stable task identity.
		if seenLeaves[leafID] {
			continue
		}
		seenLeaves[leafID] = true
		kind := 4
		if *activity.Type == 20 {
			kind = 5
		}
		legacy := activity
		legacy.Type, legacy.Status, legacy.Deadline = &kind, nil, activity.Content.ScoreDeadline
		rawTimeName := "content.score_d_ms"
		override := reset[leafID]
		if kind == 4 && late[leafID] != "" && late[leafID] != "0" {
			override = late[leafID]
			rawTimeName = "late_submission_deadline_s"
		} else if override != "" && override != "0" {
			rawTimeName = "reset_deadline_s"
		}
		if override != "" && override != "0" {
			seconds, err := strconv.ParseInt(override.String(), 10, 64)
			if err != nil || seconds < 946684800 || seconds > 4102444800 {
				return nil, errors.New("雨课堂个别任务截止时间格式变化")
			}
			milliseconds := seconds * 1000
			legacy.Deadline = &milliseconds
		}
		if legacy.Deadline == nil || *legacy.Deadline == 0 {
			if !classDeadlineLoaded {
				env, err := r.request(ctx, http.MethodGet, "/mooc-api/v1/lms/learn/classroom_info/?classroom_id="+cid, cid, nil)
				if err != nil {
					return nil, err
				}
				if err = rainCloudEnvelope(env); err != nil {
					return nil, err
				}
				var classroom struct {
					End *int64 `json:"end"`
				}
				if len(env.Data) == 0 || string(env.Data) == "null" || json.Unmarshal(env.Data, &classroom) != nil {
					return nil, errors.New("雨课堂课程截止时间格式变化")
				}
				classDeadline, classDeadlineLoaded = classroom.End, true
			}
			legacy.Deadline = classDeadline
			rawTimeName = "classroom.end_ms"
		}
		task, err := rainTask(cid, courseName, legacy, false)
		if err != nil {
			return nil, err
		}
		task.ID, task.ExternalID = "rain:"+cid+":leaf:"+leafID, leafID
		if override != "" && override != "0" {
			task.RawTime = rawTimeName + "=" + override.String()
		} else if legacy.Deadline != nil {
			task.RawTime = rawTimeName + "=" + strconv.FormatInt(*legacy.Deadline, 10)
		}
		task.Status = rainCloudStatus(*activity.Type, schedules[activity.CoursewareID.String()][leafID])
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func rainCloudEnvelope(env rainEnvelope) error {
	if env.Success == nil || !*env.Success {
		return errors.New("雨课堂云端任务元数据读取失败")
	}
	return nil
}

func rainCloudStatus(kind int, value json.RawMessage) string {
	if len(value) == 0 || string(value) == "null" {
		return "unknown"
	}
	if string(value) == "0" {
		return "pending"
	}
	var progress struct {
		Done   *int `json:"done"`
		Total  *int `json:"total"`
		Status *int `json:"status"`
	}
	if json.Unmarshal(value, &progress) != nil {
		return "unknown"
	}
	if kind == 19 && progress.Done != nil && progress.Total != nil {
		if *progress.Done > 0 && *progress.Done == *progress.Total {
			return "completed"
		}
		if *progress.Done >= 0 && *progress.Total >= 0 && (*progress.Done == 0 || *progress.Done < *progress.Total) {
			return "pending"
		}
	}
	if kind == 20 && progress.Status != nil {
		switch *progress.Status {
		case 1, 2:
			return "pending"
		case 3, 4, 5:
			return "completed"
		}
	}
	return "unknown"
}

func rainTask(cid, courseName string, activity rainActivity, simple bool) (core.Task, error) {
	kind := "homework"
	if *activity.Type == 5 {
		kind = "exam"
	}
	status := "unknown"
	// The student feed renders 0 as noReply, 1 as continueQuiz, and 2/3
	// as submitted score (pending or available). IS_SIMPLE hides these states.
	if !simple && activity.Status != nil {
		switch *activity.Status {
		case 0, 1:
			status = "pending"
		case 2, 3:
			status = "completed"
		}
	}
	t := core.Task{
		ID: "rain:" + cid + ":" + activity.ID.String(), ExternalID: activity.ID.String(),
		Platform: core.PlatformRain, Course: courseName, Title: activity.Title,
		Kind: kind, Status: status, SourceTimezone: "UTC",
		SourceURL: "https://www.yuketang.cn/v2/web/studentLog/" + cid,
	}
	if activity.Deadline != nil {
		t.RawTime = "deadline_ms=" + strconv.FormatInt(*activity.Deadline, 10)
	}
	if activity.Deadline != nil && *activity.Deadline != 0 {
		// The observed frontend passes deadline directly to new Date(). Do
		// not guess seconds from magnitude if a future response changes units.
		if *activity.Deadline < 946684800000 || *activity.Deadline > 4102444800000 {
			return core.Task{}, errors.New("雨课堂截止时间格式变化")
		}
		deadline := time.UnixMilli(*activity.Deadline).UTC()
		t.EndAt = &deadline
	}
	// Activity start denotes publication; it is not evidence of exam start.
	return t, nil
}

func rainPositiveID(id json.Number) bool {
	n, err := strconv.ParseUint(id.String(), 10, 64)
	return err == nil && n > 0
}

func rainCheckListEnvelope(env rainEnvelope) error {
	if env.ErrCode == nil {
		return errors.New("雨课堂响应格式变化")
	}
	if *env.ErrCode != 0 {
		return errors.New("雨课堂拒绝读取课程或任务列表，请重新验证登录")
	}
	return nil
}

func (r *Rain) get(ctx context.Context, path string) (rainEnvelope, error) {
	return r.request(ctx, http.MethodGet, path, "", nil)
}

func (r *Rain) request(ctx context.Context, method, path, classroomID string, body io.Reader) (rainEnvelope, error) {
	var env rainEnvelope
	if r.baseURL != rainOrigin {
		return env, errors.New("雨课堂请求地址无效")
	}
	if err := r.ensureSession(); err != nil {
		return env, err
	}
	req, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, body)
	if err != nil {
		return env, errors.New("雨课堂请求初始化失败")
	}
	cookies := r.jar.Cookies(req.URL)
	if len(cookies) == 0 {
		return env, ErrSessionExpired
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("xtbz", "ykt")
	req.Header.Set("uv-id", "0")
	req.Header.Set("university-id", "0")
	if classroomID != "" {
		req.Header.Set("classroom-id", classroomID)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			if cookie.Name == "csrftoken" {
				req.Header.Set("X-CSRFToken", cookie.Value)
				break
			}
		}
	}
	req.Header.Set("Referer", "https://www.yuketang.cn/v2/web/index")
	req.Header.Set("User-Agent", "DueRadar/0.1 (personal task reminders)")
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return env, ctx.Err()
		}
		return env, errors.New("雨课堂网络请求失败")
	}
	defer resp.Body.Close()
	if err := r.jar.check(); err != nil {
		return env, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return env, ErrSessionExpired
	}
	if resp.StatusCode != http.StatusOK {
		return env, fmt.Errorf("雨课堂 HTTP 状态 %d", resp.StatusCode)
	}
	const maxBody = 4 << 20
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return env, ctx.Err()
		}
		return env, errors.New("雨课堂响应读取失败")
	}
	if len(responseBody) > maxBody {
		return env, errors.New("雨课堂响应超过大小上限")
	}
	if json.Unmarshal(responseBody, &env) != nil {
		return env, errors.New("雨课堂返回了非预期的响应格式")
	}
	return env, nil
}
