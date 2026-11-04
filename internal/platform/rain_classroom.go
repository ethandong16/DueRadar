package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// ClassroomProblem comes only from a live student's problem timeline, never
// logs/learn, quiz/paper, homework or exam APIs. Deadline is derived from the
// server's probleminfo clock, not the device's interpretation of a feed date.
type ClassroomProblem struct {
	LessonID, ID, PresentationID  string
	Title, Body, ImageURL         string
	Type                          string
	Options                       []string
	StartedAt, Deadline           time.Time
	TimingAt                      time.Time
	AnswerKnown, Answered, Closed bool
}

func (p ClassroomProblem) Choice() bool {
	if (p.Type != "MultipleChoice" && p.Type != "MultipleChoiceMA") || len(p.Options) < 2 || len(p.Options) > 26 {
		return false
	}
	seen := map[string]bool{}
	for _, label := range p.Options {
		if len(label) != 1 || label[0] < 'A' || label[0] > 'Z' || seen[label] {
			return false
		}
		seen[label] = true
	}
	return true
}

func (p ClassroomProblem) FallbackReady(now time.Time) bool {
	left := p.Deadline.Sub(now)
	return p.Choice() && p.AnswerKnown && !p.Answered && !p.Closed && !p.StartedAt.IsZero() &&
		!p.TimingAt.IsZero() && !now.Before(p.TimingAt) && !now.Before(p.StartedAt) && now.Sub(p.TimingAt) < 5*time.Second && left > 0 && left <= 10*time.Second
}

// LiveClassroom deliberately uses the legacy read-only student bootstrap. The
// v3 bootstrap calls /lesson/checkin, which changes attendance and is NOT used.
// Unsupported responses fail closed rather than silently checking the user in.
type LiveClassroom struct {
	rain     *Rain
	lesson   LiveLesson
	conn     *websocket.Conn
	mu       sync.Mutex
	problems map[string]ClassroomProblem
	changed  chan struct{}
	finished bool
	data     rainClassroomData // Accessed only by the socket reader after bootstrap.
	timing   map[string]*rainTimingRequest
	revision map[string]uint64
	timingID int64
}

type rainTimingRequest struct {
	id        int64
	sentAt    time.Time
	revision  uint64
	requireID bool
	done      chan struct{}
	valid     bool
}

type rainClassroomSlide struct {
	ID      json.Number     `json:"lessonSlideID"`
	Cover   string          `json:"Cover"`
	Result  json.RawMessage `json:"Result"`
	Problem *struct {
		ID      json.Number     `json:"ProblemID"`
		Type    string          `json:"Type"`
		Body    string          `json:"Body"`
		Result  json.RawMessage `json:"Result"`
		Bullets []struct {
			Label string `json:"Label"`
		} `json:"Bullets"`
	} `json:"Problem"`
}

type rainClassroomData struct {
	UserID        json.Number     `json:"userID"`
	Auth          json.RawMessage `json:"userAuth"`
	ActiveID      json.Number     `json:"activePresentationID"`
	Presentations []struct {
		ID     json.Number          `json:"presentationID"`
		Title  string               `json:"Title"`
		Slides []rainClassroomSlide `json:"Slides"`
	} `json:"presentationList"`
}

func (r *Rain) classroomData(ctx context.Context, lessonID string) (rainClassroomData, error) {
	var data rainClassroomData
	if !rainPositiveID(json.Number(lessonID)) {
		return data, errors.New("课堂课次标识无效")
	}
	env, err := r.get(ctx, "/v/lesson/lesson_info_v2?lesson_id="+url.QueryEscape(lessonID))
	if err != nil {
		return data, err
	}
	if !rainLegacyOK(env) || json.Unmarshal(env.Data, &data) != nil || data.Presentations == nil || !rainPositiveID(data.UserID) {
		return data, errors.New("课堂学生只读响应不受支持（不会自动签到）")
	}
	if len(data.Presentations) > 100 {
		return data, errors.New("课堂课件数量超限")
	}
	return data, nil
}

func rainLegacyOK(env rainEnvelope) bool {
	return (env.Success != nil && *env.Success) || (env.Status != nil && *env.Status == 0)
}

func (r *Rain) OpenClassroom(ctx context.Context, lesson LiveLesson) (*LiveClassroom, error) {
	// Unlike reminder compatibility, an absent role is not sufficient authority
	// to open a student action channel. Only the observed student role is allowed.
	if lesson.Role != 5 || !rainPositiveID(json.Number(lesson.ClassroomID)) {
		return nil, errors.New("不是已确认的学生课堂")
	}
	data, err := r.classroomData(ctx, lesson.LessonID)
	if err != nil {
		return nil, err
	}
	if len(data.Auth) == 0 || string(data.Auth) == "null" || string(data.Auth) == `""` {
		return nil, errors.New("课堂实时授权缺失")
	}
	client := *r.client
	client.Timeout = 0 // Dial context bounds handshake; watch lifetime bounds the socket.
	conn, _, err := websocket.Dial(ctx, "wss://www.yuketang.cn/wsapp/", &websocket.DialOptions{
		HTTPClient: &client,
		HTTPHeader: http.Header{"Origin": {rainOrigin}, "User-Agent": {"DueRadar classroom monitor"}},
	})
	if err != nil {
		return nil, errors.New("课堂实时连接失败")
	}
	conn.SetReadLimit(4 << 20)
	c := &LiveClassroom{rain: r, lesson: lesson, conn: conn, data: data, problems: make(map[string]ClassroomProblem), changed: make(chan struct{}), timing: make(map[string]*rainTimingRequest), revision: make(map[string]uint64)}
	hello := map[string]any{"op": "hello", "userid": data.UserID, "role": "student", "auth": data.Auth, "lessonid": json.Number(lesson.LessonID), "ver": 1}
	if rainPositiveID(data.ActiveID) {
		hello["presentation"] = data.ActiveID
	}
	if err := wsjson.Write(ctx, conn, hello); err != nil {
		conn.CloseNow()
		return nil, errors.New("课堂实时认证发送失败")
	}
	return c, nil
}

func (c *LiveClassroom) Close() { _ = c.conn.CloseNow() }

// Run owns the socket reader. Callbacks must return promptly; AI and Bark must
// run in separate jobs so the reader can process close/extend messages in time.
func (c *LiveClassroom) Run(ctx context.Context, onProblem func(ClassroomProblem)) error {
	ctx, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				beat, stop := context.WithTimeout(ctx, 3*time.Second)
				err := wsjson.Write(beat, c.conn, map[string]any{"op": "xintiao", "lessonid": json.Number(c.lesson.LessonID)})
				stop()
				if err != nil {
					c.Close()
					return
				}
			}
		}
	}()
	defer func() { cancel(); c.Close(); <-heartbeatDone }()
	defer func() {
		c.mu.Lock()
		c.finished = true
		close(c.changed)
		c.changed = make(chan struct{})
		c.mu.Unlock()
		c.Close()
	}()
	for {
		var msg struct {
			Op        string                `json:"op"`
			Timeline  []rainTimelineProblem `json:"timeline"`
			Problem   rainTimelineProblem   `json:"problem"`
			ProblemID json.Number           `json:"problemid"`
			MessageID json.Number           `json:"msgid"`
			Prob      json.Number           `json:"prob"`
			Now       int64                 `json:"now"`
			DT        int64                 `json:"dt"`
			Limit     *int64                `json:"limit"`
		}
		if err := wsjson.Read(ctx, c.conn, &msg); err != nil {
			return errors.New("课堂实时连接已断开")
		}
		switch msg.Op {
		case "hello", "fetchtimeline":
			if len(msg.Timeline) > 1000 {
				return errors.New("课堂时间线超限")
			}
			for _, event := range msg.Timeline {
				if event.Type == "problem" {
					if err := c.loadProblem(ctx, event, onProblem); err != nil {
						return err
					}
				}
			}
		case "unlockproblem":
			if err := c.loadProblem(ctx, msg.Problem, onProblem); err != nil {
				return err
			}
		case "probleminfo":
			id := msg.ProblemID.String()
			c.mu.Lock()
			request := c.timing[id]
			if request == nil || (msg.MessageID == "" && request.requireID) || (msg.MessageID != "" && msg.MessageID.String() != fmt.Sprint(request.id)) {
				// An uncorrelated response may populate no actionable timing. Only
				// the initial discovery query tolerates a missing echo for display.
				c.mu.Unlock()
				continue
			}
			delete(c.timing, id)
			valid := request.revision == c.revision[id]
			c.mu.Unlock()
			if valid {
				c.updateTimingAt(id, msg.DT, msg.Now, msg.Limit, request.sentAt, onProblem)
			}
			c.mu.Lock()
			request.valid = valid
			close(request.done)
			c.mu.Unlock()
		case "extendtime":
			id := msg.Problem.Prob.String()
			c.mu.Lock()
			_, known := c.problems[id]
			if known {
				c.revision[id]++
			}
			c.mu.Unlock()
			if known {
				c.updateTimingAt(id, msg.Problem.DT, msg.Problem.Now, msg.Problem.Limit, time.Now(), onProblem)
			}
		case "problemfinished":
			c.mu.Lock()
			p, ok := c.problems[msg.Prob.String()]
			if ok {
				c.revision[p.ID]++
				p.Closed = true
				p.Deadline = time.Time{}
				c.problems[p.ID] = p
			}
			c.signalLocked()
			c.mu.Unlock()
			if ok {
				onProblem(p)
			}
		case "lessonfinished":
			// Close every known problem before returning, so queued/retrying AI
			// notifications are invalidated just like an individual close message.
			c.mu.Lock()
			closed := make([]ClassroomProblem, 0, len(c.problems))
			for id, p := range c.problems {
				c.revision[id]++
				p.Closed = true
				p.Deadline = time.Time{}
				c.problems[id] = p
				closed = append(closed, p)
			}
			c.signalLocked()
			c.mu.Unlock()
			for _, p := range closed {
				onProblem(p)
			}
			return nil
		}
	}
}

type rainTimelineProblem struct {
	Type  string      `json:"type"`
	Prob  json.Number `json:"prob"`
	Pres  json.Number `json:"pres"`
	SID   json.Number `json:"sid"`
	Index int         `json:"si"`
	DT    int64       `json:"dt"`
	Now   int64       `json:"now"`
	Limit *int64      `json:"limit"`
}

func (c *LiveClassroom) signalLocked() { close(c.changed); c.changed = make(chan struct{}) }

func (c *LiveClassroom) loadProblem(ctx context.Context, event rainTimelineProblem, callback func(ClassroomProblem)) error {
	if !rainPositiveID(event.Prob) || !rainPositiveID(event.Pres) {
		return nil
	}
	c.mu.Lock()
	_, exists := c.problems[event.Prob.String()]
	full := len(c.problems) >= 200
	c.mu.Unlock()
	if !exists && full {
		return errors.New("课堂题目数量超限")
	}
	if !exists {
		p, ok := classroomProblem(c.data, c.lesson.LessonID, event)
		if !ok {
			// Newly added presentations need one bounded read; cached slides avoid
			// doing an HTTP roundtrip for every entry of a reconnect timeline.
			read, stop := context.WithTimeout(ctx, 3*time.Second)
			data, err := c.rain.classroomData(read, c.lesson.LessonID)
			stop()
			if err != nil {
				return err
			}
			c.data = data
			p, ok = classroomProblem(data, c.lesson.LessonID, event)
			if !ok {
				return nil
			}
		}
		c.mu.Lock()
		c.problems[p.ID] = p
		c.signalLocked()
		c.mu.Unlock()
		callback(p) // Starts analysis immediately, before timing lookup.
	}
	_, err := c.askTiming(ctx, event.Prob.String(), false)
	return err
}

func classroomProblem(data rainClassroomData, lessonID string, event rainTimelineProblem) (ClassroomProblem, bool) {
	for _, pres := range data.Presentations {
		if pres.ID != event.Pres {
			continue
		}
		for index, slide := range pres.Slides {
			if event.SID != "" && event.SID != "0" {
				if slide.ID != event.SID {
					continue
				}
			} else if index+1 != event.Index {
				continue
			}
			problem := slide.Problem
			if problem == nil || problem.ID != event.Prob {
				return ClassroomProblem{}, false
			}
			p := ClassroomProblem{LessonID: lessonID, ID: problem.ID.String(), PresentationID: pres.ID.String(), Title: pres.Title, Body: problem.Body, ImageURL: slide.Cover, Type: problem.Type}
			if event.DT >= 946684800000 && event.DT <= 4102444800000 {
				p.StartedAt = time.UnixMilli(event.DT)
			}
			result := problem.Result
			if len(slide.Result) > 0 {
				result = slide.Result
			}
			// Explicit null/empty result is unanswered; missing or malformed state
			// is unknown. Do not guess on a new provider response shape.
			p.AnswerKnown, p.Answered = classroomResult(result)
			seen := map[string]bool{}
			for _, option := range problem.Bullets {
				label := strings.TrimSpace(option.Label)
				if len(label) != 1 || label[0] < 'A' || label[0] > 'Z' || seen[label] {
					return ClassroomProblem{}, false
				}
				seen[label] = true
				p.Options = append(p.Options, label)
			}
			return p, p.Choice()
		}
	}
	return ClassroomProblem{}, false
}

func classroomResult(raw json.RawMessage) (known, answered bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true, false
	}
	var answer string
	if len(raw) == 0 || json.Unmarshal(raw, &answer) != nil {
		return false, false
	}
	return true, strings.TrimSpace(answer) != ""
}

func (c *LiveClassroom) askTiming(ctx context.Context, id string, fresh bool) (*rainTimingRequest, error) {
	c.mu.Lock()
	p, known := c.problems[id]
	if !known || c.finished || p.Closed {
		c.mu.Unlock()
		return nil, errors.New("课堂题目已失效")
	}
	if pending := c.timing[id]; pending != nil {
		if !fresh {
			c.mu.Unlock()
			return pending, nil
		}
		// A final preflight cannot inherit an older discovery query. Retire its
		// waiters and require the new response to echo this request's unique ID.
		pending.valid = false
		close(pending.done)
	}
	sentAt := time.Now()
	c.timingID = max(c.timingID+1, sentAt.UnixMilli())
	request := &rainTimingRequest{id: c.timingID, sentAt: sentAt, revision: c.revision[id], requireID: fresh, done: make(chan struct{})}
	c.timing[id] = request
	c.mu.Unlock()
	if err := wsjson.Write(ctx, c.conn, map[string]any{"op": "probleminfo", "lessonid": json.Number(c.lesson.LessonID), "problemid": json.Number(id), "msgid": request.id}); err != nil {
		c.Close()
		return nil, errors.New("课堂倒计时查询失败")
	}
	return request, nil
}

func (c *LiveClassroom) updateTimingAt(id string, dt, serverNow int64, limit *int64, sentAt time.Time, callback func(ClassroomProblem)) {
	c.mu.Lock()
	p, ok := c.problems[id]
	if !ok {
		c.mu.Unlock()
		return
	}
	p.Deadline = time.Time{}
	p.TimingAt = time.Now()
	// Server clock difference avoids trusting a skewed device clock. Reject
	// unlimited, closed and implausible periods instead of inventing a deadline.
	if limit != nil && *limit > 0 && *limit <= 86400 && dt >= 946684800000 && serverNow >= dt && serverNow <= 4102444800000 {
		// Subtract the entire query roundtrip, conservatively underestimating
		// the remaining time rather than submitting after the server's deadline.
		remaining := time.Duration(*limit)*time.Second - time.Duration(serverNow-dt)*time.Millisecond - p.TimingAt.Sub(sentAt)
		if remaining > 0 {
			p.Deadline = p.TimingAt.Add(remaining)
			p.Closed = false
		} else {
			p.Closed = true
		}
	} else if limit != nil && *limit == 0 {
		p.Closed = true
	}
	c.problems[id] = p
	c.signalLocked()
	c.mu.Unlock()
	callback(p)
}

// CurrentProblem rereads the student's own answer and requests fresh server
// timing. The socket reader must be running; a disconnect or missing timing is
// an error, never permission to submit using the previously scheduled deadline.
func (c *LiveClassroom) CurrentProblem(ctx context.Context, id string) (ClassroomProblem, error) {
	c.mu.Lock()
	p, ok := c.problems[id]
	finished := c.finished
	c.mu.Unlock()
	if !ok || finished {
		return ClassroomProblem{}, errors.New("课堂题目已失效")
	}
	data, err := c.rain.classroomData(ctx, c.lesson.LessonID)
	if err != nil {
		return ClassroomProblem{}, err
	}
	event := rainTimelineProblem{Prob: json.Number(id), Pres: json.Number(p.PresentationID)}
	// Locate the exact problem without relying on its old slide position.
	for _, pres := range data.Presentations {
		if pres.ID == event.Pres {
			for i, slide := range pres.Slides {
				if slide.Problem != nil && slide.Problem.ID.String() == id {
					event.SID = slide.ID
					event.Index = i + 1
				}
			}
		}
	}
	fresh, found := classroomProblem(data, c.lesson.LessonID, event)
	if !found {
		return ClassroomProblem{}, errors.New("课堂题目状态未知")
	}
	request, err := c.askTiming(ctx, id, true)
	if err != nil {
		return ClassroomProblem{}, err
	}
	for {
		c.mu.Lock()
		current := c.problems[id]
		changed := c.changed
		finished = c.finished
		c.mu.Unlock()
		if finished {
			return ClassroomProblem{}, errors.New("课堂已结束")
		}
		select {
		case <-ctx.Done():
			return ClassroomProblem{}, ctx.Err()
		case <-request.done:
			c.mu.Lock()
			current = c.problems[id]
			valid := request.valid && request.revision == c.revision[id] && !c.finished
			fresh.StartedAt = p.StartedAt
			fresh.Deadline = current.Deadline
			fresh.TimingAt = current.TimingAt
			fresh.Closed = current.Closed
			if fresh.Answered {
				current.Answered = true
				current.AnswerKnown = true
				c.problems[id] = current
			}
			c.mu.Unlock()
			if !valid {
				return ClassroomProblem{}, errors.New("课堂倒计时已变化")
			}
			return fresh, nil
		case <-changed:
		}
	}
}

// SubmitChoice makes exactly one transport attempt. The platform rejects an
// already-answered problem (status_code=4); it is treated as a no-op success.
// The caller must durably reserve this action before calling.
func (c *LiveClassroom) SubmitChoice(ctx context.Context, p ClassroomProblem, label string) (string, error) {
	if p.LessonID != c.lesson.LessonID || !p.FallbackReady(time.Now()) || !slices.Contains(p.Options, label) {
		return "", errors.New("不满足课堂兜底提交条件")
	}
	c.mu.Lock()
	current, ok := c.problems[p.ID]
	finished := c.finished
	c.mu.Unlock()
	if !ok || finished || current.Closed || current.Answered || !current.Deadline.Equal(p.Deadline) {
		return "", errors.New("课堂状态已变化")
	}
	start := (p.StartedAt.UnixMilli() + 999) / 1000
	now := (time.Now().UnixMilli() + 999) / 1000
	body, _ := json.Marshal(map[string]any{"duration": now - start, "startTime": start, "submit_time": now, "lesson_problem_id": json.Number(p.ID), "result": label, "retry_times": 0})
	env, err := c.rain.request(ctx, http.MethodPost, "/v/lesson/answer_lesson_problem", c.lesson.ClassroomID, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	var data struct {
		Status *int `json:"status_code"`
	}
	if !rainLegacyOK(env) || json.Unmarshal(env.Data, &data) != nil || data.Status == nil {
		return "", errors.New("课堂提交结果未知，不自动重试")
	}
	if *data.Status == 4 {
		return "already_answered", nil
	}
	if *data.Status != 0 && *data.Status != 1 {
		return "", fmt.Errorf("课堂提交未确认（状态 %d），不自动重试", *data.Status)
	}
	return "submitted", nil
}
