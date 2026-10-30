package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LiveLesson is a classroom that currently has a lesson in progress. Rain
// Classroom exposes it on the student home page as "正在上课".
type LiveLesson struct {
	ClassroomID   string
	LessonID      string
	CourseName    string
	ClassroomName string
	Role          int
}

// IsStudent reports whether the signed-in user attends the classroom as a
// student. Rain Classroom uses role 1 for the teacher and 2 for teaching
// assistants; the student quick-answer endpoint is not available to those
// roles and answers with HTTP 500. An absent role is treated as a student so
// older responses keep working.
func (l LiveLesson) IsStudent() bool {
	return l.Role != 1 && l.Role != 2
}

// LiveActivity is one entry of the classroom activity feed. Only the fields the
// in-class monitor needs are kept; unknown activities stay out of the decision.
type LiveActivity struct {
	ID           string
	Type         int
	Title        string
	CoursewareID string
	CreatedAt    time.Time
	Deadline     *time.Time
	ProblemCount int
	TotalScore   float64
}

// LiveQuickAnswer is a real-time classroom answer prompt. It is exposed by
// the lesson-summary API rather than the ordinary logs/learn activity feed.
// The endpoint does not provide a title, so callers may use Title when the
// platform starts returning one and fall back to a generic label otherwise.
type LiveQuickAnswer struct {
	ID     string
	Title  string
	SentAt time.Time
}

// Answerable reports whether the activity expects an answer from the student.
// Observed types: 5 covers in-class quizzes and paper homework, 4 covers native
// homework, 20 covers cloud exams. Courseware, slides, and announcements do not.
func (a LiveActivity) Answerable() bool {
	switch a.Type {
	case 4, 5, 20:
		return true
	}
	return false
}

// LiveLessons reads the independent on-lesson endpoint. An empty list is a
// valid answer: the student simply has no class at the moment.
func (r *Rain) LiveLessons(ctx context.Context) ([]LiveLesson, error) {
	env, err := r.get(ctx, "/api/v3/classroom/on-lesson")
	if err != nil {
		return nil, err
	}
	if env.Code == nil {
		return nil, errors.New("雨课堂在课状态响应格式变化")
	}
	if *env.Code != 0 {
		return nil, ErrSessionExpired
	}
	var data struct {
		Lessons *[]struct {
			ClassroomID   json.Number `json:"classroomId"`
			LessonID      json.Number `json:"lessonId"`
			CourseName    string      `json:"courseName"`
			ClassroomName string      `json:"classroomName"`
			Role          json.Number `json:"role"`
		} `json:"onLessonClassrooms"`
	}
	if json.Unmarshal(env.Data, &data) != nil || data.Lessons == nil {
		return nil, errors.New("雨课堂在课状态格式变化")
	}
	if len(*data.Lessons) > 50 {
		return nil, errors.New("雨课堂在课班级数量超过上限")
	}
	lessons := make([]LiveLesson, 0, len(*data.Lessons))
	seen := make(map[string]bool, len(*data.Lessons))
	for _, item := range *data.Lessons {
		if !rainPositiveID(item.ClassroomID) {
			return nil, errors.New("雨课堂在课班级缺少标识")
		}
		classroomID := item.ClassroomID.String()
		if seen[classroomID] {
			continue
		}
		seen[classroomID] = true
		role := 0
		if n, err := item.Role.Int64(); err == nil && n >= 0 && n <= 100 {
			role = int(n)
		}
		lesson := LiveLesson{ClassroomID: classroomID, CourseName: strings.TrimSpace(item.CourseName), ClassroomName: strings.TrimSpace(item.ClassroomName), Role: role}
		if rainPositiveID(item.LessonID) {
			lesson.LessonID = item.LessonID.String()
		}
		if lesson.CourseName == "" {
			lesson.CourseName = lesson.ClassroomName
		}
		if lesson.CourseName == "" {
			lesson.CourseName = "雨课堂课堂"
		}
		lessons = append(lessons, lesson)
	}
	return lessons, nil
}

// LiveActivities reads the newest page of one classroom's activity feed. The
// monitor only needs the first page: a quiz pushed during class is the newest
// entry. Older pages are read by the normal 15-minute task synchronization.
func (r *Rain) LiveActivities(ctx context.Context, classroomID string) ([]LiveActivity, error) {
	if _, err := strconv.ParseUint(classroomID, 10, 64); err != nil {
		return nil, errors.New("雨课堂班级标识无效")
	}
	env, err := r.get(ctx, "/v2/api/web/logs/learn/"+classroomID+"?actype=-1&page=0&offset=20&sort=-1")
	if err != nil {
		return nil, err
	}
	if err = rainCheckListEnvelope(env); err != nil {
		return nil, err
	}
	var data struct {
		Activities *[]struct {
			ID           json.Number  `json:"id"`
			Type         *int         `json:"type"`
			Title        string       `json:"title"`
			CoursewareID json.Number  `json:"courseware_id"`
			CreateTime   *int64       `json:"create_time"`
			Deadline     *int64       `json:"deadline"`
			ProblemCount *int         `json:"problem_count"`
			TotalScore   *json.Number `json:"total_score"`
		} `json:"activities"`
	}
	if json.Unmarshal(env.Data, &data) != nil || data.Activities == nil {
		return nil, errors.New("雨课堂课堂活动格式变化")
	}
	if len(*data.Activities) > 100 {
		return nil, errors.New("雨课堂课堂活动数量超过上限")
	}
	activities := make([]LiveActivity, 0, len(*data.Activities))
	seen := make(map[string]bool, len(*data.Activities))
	for _, item := range *data.Activities {
		if item.Type == nil || !rainPositiveID(item.ID) {
			return nil, errors.New("雨课堂课堂活动缺少类型或标识")
		}
		id := item.ID.String()
		if seen[id] {
			continue
		}
		seen[id] = true
		activity := LiveActivity{ID: id, Type: *item.Type, Title: strings.TrimSpace(item.Title)}
		if rainPositiveID(item.CoursewareID) {
			activity.CoursewareID = item.CoursewareID.String()
		}
		if item.CreateTime != nil && *item.CreateTime != 0 {
			if *item.CreateTime < 946684800000 || *item.CreateTime > 4102444800000 {
				return nil, errors.New("雨课堂课堂活动时间格式变化")
			}
			activity.CreatedAt = time.UnixMilli(*item.CreateTime).UTC()
		}
		if item.Deadline != nil && *item.Deadline != 0 {
			if *item.Deadline < 946684800000 || *item.Deadline > 4102444800000 {
				return nil, errors.New("雨课堂课堂活动截止时间格式变化")
			}
			deadline := time.UnixMilli(*item.Deadline).UTC()
			activity.Deadline = &deadline
		}
		if item.ProblemCount != nil && *item.ProblemCount >= 0 {
			activity.ProblemCount = *item.ProblemCount
		}
		if item.TotalScore != nil {
			if score, err := item.TotalScore.Float64(); err == nil && score >= 0 {
				activity.TotalScore = score
			}
		}
		activities = append(activities, activity)
	}
	return activities, nil
}

// LiveQuickAnswers reads the read-only list used by the Rain Classroom
// student lesson page for "quick answer" prompts. A prompt is represented by
// sendTime even before the student submits anything, which is the distinction
// from the historical activity feed used by LiveActivities.
func (r *Rain) LiveQuickAnswers(ctx context.Context, lessonID string) ([]LiveQuickAnswer, error) {
	if _, err := strconv.ParseUint(strings.TrimSpace(lessonID), 10, 64); err != nil {
		return nil, errors.New("雨课堂课次标识无效")
	}
	q := url.Values{"lesson_id": {lessonID}, "page": {"1"}, "pageSize": {"10"}}
	env, err := r.get(ctx, "/api/v3/lesson-summary/student/quick-answer-list?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if env.Code == nil {
		return nil, errors.New("雨课堂课堂答题响应格式变化")
	}
	if *env.Code != 0 {
		if *env.Code == 50000 {
			return nil, ErrSessionExpired
		}
		return nil, errors.New("雨课堂课堂答题读取失败")
	}
	var data struct {
		List *[]struct {
			ID       json.Number `json:"id"`
			Title    string      `json:"title"`
			SendTime json.Number `json:"sendTime"`
		} `json:"list"`
	}
	if json.Unmarshal(env.Data, &data) != nil || data.List == nil {
		return nil, errors.New("雨课堂课堂答题列表格式变化")
	}
	if len(*data.List) > 100 {
		return nil, errors.New("雨课堂课堂答题数量超过上限")
	}
	answers := make([]LiveQuickAnswer, 0, len(*data.List))
	seen := make(map[string]bool, len(*data.List))
	for _, item := range *data.List {
		sentAt, err := rainLiveTimestamp(item.SendTime)
		if err != nil {
			return nil, errors.New("雨课堂课堂答题时间格式变化")
		}
		id := strings.TrimSpace(item.ID.String())
		if id == "" || id == "0" {
			// The student endpoint has returned rows without id in older
			// deployments. sendTime is still unique for a published prompt and
			// lets the monitor remain useful without guessing an answer state.
			id = strconv.FormatInt(sentAt.UnixMilli(), 10)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		answers = append(answers, LiveQuickAnswer{ID: id, Title: strings.TrimSpace(item.Title), SentAt: sentAt})
	}
	return answers, nil
}

func rainLiveTimestamp(raw json.Number) (time.Time, error) {
	s := strings.TrimSpace(raw.String())
	if s == "" || s == "0" {
		return time.Time{}, errors.New("missing timestamp")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	// The web client currently returns milliseconds; accept seconds too because
	// the mobile endpoint used that unit in older responses.
	if n >= 946684800 && n < 946684800000 {
		n *= 1000
	}
	if n < 946684800000 || n > 4102444800000 {
		return time.Time{}, errors.New("timestamp outside supported range")
	}
	return time.UnixMilli(n).UTC(), nil
}
