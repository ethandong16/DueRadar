package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
)

func legacyTime(value json.Number) *time.Time {
	n, err := value.Int64()
	// The observed legacy endpoints use milliseconds. Keep missing values and
	// changed units unknown instead of inventing 1970 or far-future deadlines.
	if err != nil || n < 946684800000 || n >= 4102444800000 {
		return nil
	}
	t := time.UnixMilli(n)
	return &t
}
func (z *Zhidao) legacyTasks(ctx context.Context, course zhidaoCourse) ([]core.Task, error) {
	tasks := []core.Task{}
	for _, source := range []struct{ endpoint, kind string }{{"https://hiexam-server.zhihuishu.com/zhsathome/homeworkUser/List", "homework"}, {"https://hike-examstu.zhihuishu.com/zhsathome/examStu/List", "exam"}} {
		seen := map[string]bool{}
		count := 0
		expectedTotal := -1
		for page := 1; page <= 500; page++ {
			q := url.Values{"courseId": {course.ID}, "pageIndex": {strconv.Itoa(page)}, "pageSize": {"20"}, "key": {""}, "completeTab": {"0"}, "uuid": {z.session.ZhsUUID}, "dateFormate": {strconv.FormatInt(time.Now().UnixMilli(), 10)}}
			var result struct {
				Status json.Number `json:"status"`
				RT     struct {
					Total *int `json:"rowCount"`
					List  []struct {
						ID     json.Number `json:"homeworkId"`
						Title  string      `json:"title"`
						Status *int        `json:"status"`
						Start  json.Number `json:"startTime"`
						End    json.Number `json:"endTime"`
					} `json:"list"`
				} `json:"rt"`
			}
			if err := zhidaoRequest(ctx, z.client, "GET", source.endpoint+"?"+q.Encode(), "", nil, &result); err != nil {
				return nil, err
			}
			if result.Status != "200" || result.RT.List == nil || result.RT.Total == nil || *result.RT.Total < 0 {
				return nil, errors.New("知到翻转课任务读取失败")
			}
			if expectedTotal >= 0 && *result.RT.Total != expectedTotal {
				return nil, errors.New("知到翻转课任务总数在翻页期间变化，请稍后重试")
			}
			expectedTotal = *result.RT.Total
			for _, row := range result.RT.List {
				if row.ID == "" || row.Title == "" || seen[string(row.ID)] {
					return nil, errors.New("知到翻转课任务分页无效")
				}
				seen[string(row.ID)] = true
				status := "unknown"
				if row.Status != nil {
					switch *row.Status {
					case 1, 2:
						status = "pending"
					case 3, 4, 5:
						status = "completed"
					}
				}
				tasks = append(tasks, core.Task{ID: "zhidao:spoc:" + course.ID + ":" + source.kind + ":" + string(row.ID), Platform: core.PlatformZhidaoCampus, Course: course.Name, Title: row.Title, Kind: source.kind, Status: status, StartAt: legacyTime(row.Start), EndAt: legacyTime(row.End), RawTime: zhidaoRawTime(row.Start.String(), row.End.String()), SourceTimezone: "Asia/Shanghai", SourceURL: "https://wenda.zhihuishu.com/stu/courseInfo/homeworkExam/?courseId=" + url.QueryEscape(course.ID)})
			}
			count += len(result.RT.List)
			if count == expectedTotal {
				break
			}
			if len(result.RT.List) == 0 || count > expectedTotal || page == 500 {
				return nil, errors.New("知到翻转课任务分页不完整")
			}
		}
	}
	videos, err := z.legacyVideos(ctx, course)
	if err != nil {
		return nil, err
	}
	return append(tasks, videos...), nil
}

type zhidaoResource struct {
	ID        json.Number      `json:"id"`
	Type      int              `json:"type"`
	DataType  int              `json:"dataType"`
	Name      string           `json:"name"`
	MustLearn *int             `json:"mustLearn"`
	Total     *float64         `json:"totalTime"`
	Studied   *float64         `json:"studyTime"`
	Children  []zhidaoResource `json:"childList"`
}

func (z *Zhidao) legacyVideos(ctx context.Context, course zhidaoCourse) ([]core.Task, error) {
	q := url.Values{"courseId": {course.ID}, "uuid": {z.session.ZhsUUID}, "dateFormate": {strconv.FormatInt(time.Now().UnixMilli(), 10)}}
	var result struct {
		Status json.Number      `json:"status"`
		RT     []zhidaoResource `json:"rt"`
	}
	if err := zhidaoRequest(ctx, z.client, "GET", "https://studyresources.zhihuishu.com/studyResources/stuResouce/queryResourceTree?"+q.Encode(), "", nil, &result); err != nil {
		return nil, err
	}
	if result.Status != "200" || result.RT == nil {
		return nil, errors.New("知到翻转课学习进度读取失败")
	}
	return mapZhidaoResources(course, result.RT)
}
func mapZhidaoResources(course zhidaoCourse, rows []zhidaoResource) ([]core.Task, error) {
	tasks := []core.Task{}
	seen := map[string]bool{}
	count := 0
	var visit func([]zhidaoResource, int) error
	visit = func(rows []zhidaoResource, depth int) error {
		if depth > 32 {
			return errors.New("知到资源目录层级异常")
		}
		for _, r := range rows {
			count++
			if count > 20000 {
				return errors.New("知到资源目录超过上限")
			}
			if len(r.Children) > 0 {
				if err := visit(r.Children, depth+1); err != nil {
					return err
				}
			}
			if r.Type != 1 || r.DataType != 3 {
				continue
			}
			if r.MustLearn == nil || (*r.MustLearn != 0 && *r.MustLearn != 1) {
				return errors.New("知到视频缺少有效的必学状态")
			}
			if *r.MustLearn == 0 {
				continue
			}
			if r.ID == "" || r.Name == "" || seen[string(r.ID)] {
				return errors.New("知到视频标识无效或重复")
			}
			seen[string(r.ID)] = true
			status := "unknown"
			var progress *float64
			if r.Total != nil && r.Studied != nil && *r.Total > 0 && *r.Studied >= 0 {
				p := 100 * *r.Studied / *r.Total
				if p > 100 {
					p = 100
				}
				progress = &p
				status = "pending"
				if p >= 80 {
					status = "completed"
				} // Official resource UI marks >=80% complete.
			}
			tasks = append(tasks, core.Task{ID: "zhidao:spoc:" + course.ID + ":video:" + string(r.ID), Platform: core.PlatformZhidaoCampus, Course: course.Name, Title: r.Name, Kind: "video", Status: status, Progress: progress, SourceTimezone: "Asia/Shanghai", SourceURL: "https://wenda.zhihuishu.com/stu/courseInfo/studyResource?courseId=" + url.QueryEscape(course.ID)})
		}
		return nil
	}
	if err := visit(rows, 0); err != nil {
		return nil, err
	}
	return tasks, nil
}
