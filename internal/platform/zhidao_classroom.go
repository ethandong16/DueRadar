package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrZhidaoClassroomSession means the stored Zhidao session has no ai-poly
// classroom credential. The task-sync token (jt-cas) is deliberately not used
// as a fallback: the official student classroom client authenticates with the
// ai-poly cookie, and silently substituting another token would make a format
// error look like an empty classroom.
var ErrZhidaoClassroomSession = errors.New("知到课堂会话凭据不可用")

const (
	zhidaoClassroomOrigin = "https://classroom-h5.polymas.com"
	zhidaoVotePublish     = "IN_CLASS::TOOL-VOTE::PUBLISH"
)

// ZhidaoPollLesson is one live lesson returned by the student classroom API.
// Only read-only identifiers are retained.
type ZhidaoPollLesson struct {
	LessonID string
	CourseID string
	Term     int
	Course   string
}

// ZhidaoPollSignal is a vote announcement found in the lesson waterfall.
// CreatedAt is the waterfall row's creation time when the platform supplies a
// plausible timestamp; it lets a late-starting monitor distinguish a fresh vote
// from one that was already published before monitoring began.
type ZhidaoPollSignal struct {
	ID        string
	Title     string
	CreatedAt time.Time
}

// ZhidaoPoll is the read-only subset of a vote detail. Participant identities
// and result counts are intentionally never parsed.
type ZhidaoPoll struct {
	ID          string
	Title       string
	Options     []string
	Anonymous   bool
	StatusKnown bool
	Finished    bool
}

type zhidaoIdentifier string

func (id *zhidaoIdentifier) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*id = ""
		return nil
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*id = zhidaoIdentifier(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*id = zhidaoIdentifier(number.String())
	return nil
}

func (id zhidaoIdentifier) String() string { return string(id) }

func zhidaoTextID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == '/' || r == '?' || r == '&' || r == '\\' {
			return false
		}
	}
	return true
}

type zhidaoFlexInt int64

func (n *zhidaoFlexInt) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*n = 0
		return nil
	}
	if text[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		text = value
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return err
	}
	*n = zhidaoFlexInt(parsed)
	return nil
}

// h5Token reads only the ai-poly cookie. The cookie is normally stored for
// .polymas.com, so both the API host and the classroom H5 host are checked.
func (z *Zhidao) h5Token() (string, error) {
	if z.initErr != nil {
		return "", z.initErr
	}
	if z.client == nil || z.client.Jar == nil {
		return "", ErrZhidaoClassroomSession
	}
	for _, origin := range []string{z.base, zhidaoClassroomOrigin} {
		u, err := url.Parse(origin)
		if err != nil {
			continue
		}
		for _, cookie := range z.client.Jar.Cookies(u) {
			if cookie.Name == "ai-poly" && cookie.Value != "" {
				if decoded, err := url.PathUnescape(cookie.Value); err == nil {
					return decoded, nil
				}
				return cookie.Value, nil
			}
		}
	}
	return "", ErrZhidaoClassroomSession
}

func (z *Zhidao) h5Request(ctx context.Context, method, path string, query url.Values, body, out any) error {
	token, err := z.h5Token()
	if err != nil {
		return err
	}
	endpoint := z.base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return errors.New("知到课堂请求构造失败")
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Authorization", token)
	req.Header.Set("Origin", zhidaoClassroomOrigin)
	req.Header.Set("Referer", zhidaoClassroomOrigin+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 DueRadar/0.1")
	resp, err := z.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("知到课堂连接失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ErrSessionExpired
	}
	if resp.StatusCode != 200 {
		return errors.New("知到课堂服务暂未接受请求")
	}
	const maxBody = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("知到课堂响应读取失败")
	}
	if len(data) > maxBody || json.Unmarshal(data, out) != nil {
		return errors.New("知到课堂返回的数据格式无效")
	}
	return nil
}

// LivePollLessons lists currently live student lessons and filters out lessons
// the platform explicitly reports as finished. Any incomplete response is an
// error so the caller never treats a partial list as a complete snapshot.
func (z *Zhidao) LivePollLessons(ctx context.Context) ([]ZhidaoPollLesson, error) {
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := z.h5Request(ctx, "GET", "/student-course/lesson/live/list", nil, nil, &env); err != nil {
		return nil, err
	}
	if env.Code != 200 || len(env.Data) == 0 {
		return nil, errors.New("知到课堂列表读取失败")
	}
	if bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		return nil, nil
	}
	var rows []struct {
		LessonID   zhidaoIdentifier `json:"lessonId"`
		CourseID   zhidaoIdentifier `json:"courseId"`
		Term       zhidaoFlexInt    `json:"term"`
		CourseName string           `json:"courseName"`
		Name       string           `json:"name"`
	}
	if json.Unmarshal(env.Data, &rows) != nil {
		return nil, errors.New("知到课堂列表格式变化")
	}
	if len(rows) > 20 {
		return nil, errors.New("知到课堂数量超限")
	}
	lessons := make([]ZhidaoPollLesson, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		lessonID, courseID := row.LessonID.String(), row.CourseID.String()
		if !zhidaoTextID(lessonID) || !zhidaoTextID(courseID) || row.Term <= 0 {
			return nil, errors.New("知到课堂标识不完整")
		}
		if seen[lessonID] {
			continue
		}
		seen[lessonID] = true
		course := strings.TrimSpace(row.CourseName)
		if course == "" {
			course = strings.TrimSpace(row.Name)
		}
		if course == "" {
			course = "知到课堂"
		}
		var status struct {
			Code int   `json:"code"`
			Data *bool `json:"data"`
		}
		query := url.Values{"lessonId": {lessonID}, "courseId": {courseID}, "term": {strconv.FormatInt(int64(row.Term), 10)}}
		if err := z.h5Request(ctx, "GET", "/student-app/interaction/api/lesson/check/finish", query, nil, &status); err != nil {
			return nil, err
		}
		if status.Code != 200 || status.Data == nil {
			return nil, errors.New("知到课堂状态读取失败")
		}
		if *status.Data {
			continue
		}
		lessons = append(lessons, ZhidaoPollLesson{LessonID: lessonID, CourseID: courseID, Term: int(row.Term), Course: course})
	}
	return lessons, nil
}

// LivePollSignals reads the read-only lesson waterfall and extracts vote
// publish signals. Rows of other interaction types are ignored. A malformed
// vote publish row invalidates the snapshot instead of being dropped silently.
func (z *Zhidao) LivePollSignals(ctx context.Context, lesson ZhidaoPollLesson) ([]ZhidaoPollSignal, error) {
	if !zhidaoTextID(lesson.LessonID) || !zhidaoTextID(lesson.CourseID) || lesson.Term <= 0 {
		return nil, errors.New("知到课堂标识无效")
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	body := map[string]any{"lessonId": lesson.LessonID, "queryType": "ALL", "courseId": lesson.CourseID, "term": lesson.Term}
	if err := z.h5Request(ctx, "POST", "/student-app/interaction/waterfall/list-all", nil, body, &env); err != nil {
		return nil, err
	}
	if env.Code != 200 || len(env.Data) == 0 {
		return nil, errors.New("知到课堂互动读取失败")
	}
	if bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		return nil, nil
	}
	var rows []struct {
		SignalCode string          `json:"signalCode"`
		Message    json.RawMessage `json:"message"`
		BizObj     json.RawMessage `json:"bizObj"`
		CreateTime json.RawMessage `json:"createTime"`
	}
	if json.Unmarshal(env.Data, &rows) != nil {
		return nil, errors.New("知到课堂互动格式变化")
	}
	if len(rows) > 5000 {
		return nil, errors.New("知到课堂互动数量超限")
	}
	signals := make([]ZhidaoPollSignal, 0, 8)
	seen := map[string]bool{}
	for _, row := range rows {
		signalCode := strings.TrimSpace(row.SignalCode)
		var message struct {
			MsgTime json.RawMessage `json:"msgTime"`
			Signal  struct {
				SignalCode string          `json:"signalCode"`
				Content    json.RawMessage `json:"content"`
			} `json:"signal"`
		}
		if len(row.Message) > 0 {
			raw := bytes.TrimSpace(row.Message)
			if len(raw) > 64<<10 {
				return nil, errors.New("知到课堂互动内容超限")
			}
			if len(raw) > 0 && raw[0] == '"' {
				var text string
				if json.Unmarshal(raw, &text) != nil {
					continue
				}
				raw = bytes.TrimSpace([]byte(text))
			}
			if len(raw) > 0 && raw[0] == '{' {
				if json.Unmarshal(raw, &message) != nil {
					continue
				}
			}
		}
		if code := strings.TrimSpace(message.Signal.SignalCode); code != "" {
			signalCode = code
		}
		if signalCode != zhidaoVotePublish {
			continue
		}
		merged := map[string]any{}
		if len(message.Signal.Content) > 0 {
			if err := zhidaoObject(message.Signal.Content, merged); err != nil {
				return nil, errors.New("知到投票消息格式变化")
			}
		}
		if len(row.BizObj) > 0 {
			if err := zhidaoObject(row.BizObj, merged); err != nil {
				return nil, errors.New("知到投票数据格式变化")
			}
		}
		id := zhidaoStringField(merged, "bizId", "voteId", "id")
		if !zhidaoTextID(id) {
			return nil, errors.New("知到投票缺少标识")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		createdAt := zhidaoFlexTime(row.CreateTime)
		// The waterfall also carries the room message time in epoch seconds.
		// Prefer it because it is unambiguous across platform date formats.
		if messageTime := zhidaoFlexTime(message.MsgTime); !messageTime.IsZero() {
			createdAt = messageTime
		}
		signals = append(signals, ZhidaoPollSignal{ID: id, Title: strings.TrimSpace(zhidaoStringField(merged, "title", "voteTitle")), CreatedAt: createdAt})
		if len(signals) > 100 {
			return nil, errors.New("知到投票数量超限")
		}
	}
	return signals, nil
}

func zhidaoObject(raw json.RawMessage, out map[string]any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	maps.Copy(out, value)
	return nil
}

func zhidaoStringField(values map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := values[key].(type) {
		case string:
			if text := strings.TrimSpace(value); text != "" {
				return text
			}
		case json.Number:
			if text := value.String(); text != "" {
				return text
			}
		}
	}
	return ""
}

// zhidaoFlexTime accepts the numeric timestamp shapes the client normalizes
// and the platform's "YYYY-MM-DD HH:MM:SS" wall-clock strings (China time). An
// unrecognized timestamp is unknown rather than a reason to reject the whole
// vote snapshot; it only affects the first-snapshot freshness decision.
func zhidaoFlexTime(raw json.RawMessage) time.Time {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return time.Time{}
	}
	if text[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return time.Time{}
		}
		text = strings.TrimSpace(value)
	}
	if value, err := strconv.ParseInt(text, 10, 64); err == nil {
		return zhidaoPollTime(value)
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, text, zhidaoPollZone); err == nil {
			return parsed
		}
	}
	if parsed, err := time.Parse(time.RFC3339, text); err == nil {
		return parsed
	}
	return time.Time{}
}

// Zhidao is a Chinese platform and reports classroom wall-clock times in CST
// (UTC+8, no daylight saving).
var zhidaoPollZone = time.FixedZone("CST", 8*60*60)

func zhidaoPollTime(value int64) time.Time {
	// The waterfall supplies epoch milliseconds. Accept seconds as a fallback
	// only when the value cannot plausibly be milliseconds.
	if value >= 1e12 && value <= 1e13 {
		return time.UnixMilli(value)
	}
	if value >= 1e9 && value < 1e10 {
		return time.Unix(value, 0)
	}
	return time.Time{}
}

// PollDetail reads the vote title, option texts, anonymity, and finish state.
// selectedStudents and other participant fields are not part of the struct, so
// classmate identities never enter memory through this path.
func (z *Zhidao) PollDetail(ctx context.Context, lesson ZhidaoPollLesson, voteID string) (ZhidaoPoll, error) {
	if !zhidaoTextID(lesson.LessonID) || !zhidaoTextID(lesson.CourseID) || lesson.Term <= 0 || !zhidaoTextID(voteID) {
		return ZhidaoPoll{}, errors.New("知到投票标识无效")
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	query := url.Values{"voteId": {voteID}, "courseId": {lesson.CourseID}, "term": {strconv.Itoa(lesson.Term)}}
	if err := z.h5Request(ctx, "GET", "/student-app/api/vote/getVoteDetail", query, nil, &env); err != nil {
		return ZhidaoPoll{}, err
	}
	if env.Code != 200 || len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		return ZhidaoPoll{}, errors.New("知到投票详情读取失败")
	}
	var data struct {
		VoteID      zhidaoIdentifier `json:"voteId"`
		ID          zhidaoIdentifier `json:"id"`
		Title       string           `json:"title"`
		IsAnonymous bool             `json:"isAnonymous"`
		IsFinish    json.RawMessage  `json:"isFinish"`
		Options     []struct {
			OptionContent string `json:"optionContent"`
			OptionCode    string `json:"optionCode"`
		} `json:"options"`
	}
	if json.Unmarshal(env.Data, &data) != nil {
		return ZhidaoPoll{}, errors.New("知到投票详情格式变化")
	}
	id := data.VoteID.String()
	if id == "" {
		id = data.ID.String()
	}
	if id != voteID {
		return ZhidaoPoll{}, errors.New("知到投票详情标识不一致")
	}
	title := strings.TrimSpace(data.Title)
	options := make([]string, 0, min(len(data.Options), 8))
	for _, option := range data.Options {
		text := strings.TrimSpace(option.OptionContent)
		// Quick votes are read aloud by the teacher; the API returns a null
		// optionContent and only an A/B/C code. The code is still a useful label.
		if text == "" {
			text = strings.TrimSpace(option.OptionCode)
		}
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > 80 {
			text = string(runes[:80]) + "…"
		}
		options = append(options, text)
		if len(options) == 8 {
			break
		}
	}
	known, finished := zhidaoFinishState(data.IsFinish)
	return ZhidaoPoll{ID: id, Title: title, Options: options, Anonymous: data.IsAnonymous, StatusKnown: known, Finished: finished}, nil
}

func zhidaoFinishState(raw json.RawMessage) (known, finished bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return false, false
	}
	if text[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return false, false
		}
		text = strings.TrimSpace(value)
	}
	switch text {
	case "1", "true":
		return true, true
	case "0", "false":
		return true, false
	default:
		return false, false
	}
}
