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
	ProblemType                   int
	Options                       []string
	StartedAt, Deadline           time.Time
	TimingAt                      time.Time
	AnswerKnown, Answered, Closed bool
}

// IsPoll reports whether the platform marked this activity as a poll. The
// student live client uses the strings Polling and AnonymousPolling; the v3
// lesson payloads carry the same type as the number 3. Polls are detected and
// announced, but never answered automatically by DueRadar.
func (p ClassroomProblem) IsPoll() bool {
	return p.ProblemType == 3 || p.Type == "Polling" || p.Type == "AnonymousPolling"
}

func (p ClassroomProblem) AnonymousPoll() bool {
	return p.Type == "AnonymousPolling"
}

func (p ClassroomProblem) Choice() bool {
	if p.IsPoll() {
		// A poll may reuse a choice-like option list. It is still not a graded
		// question and must never reach the fallback submit or model path.
		return false
	}
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

// LiveClassroom reads the student timeline over the websocket. The handshake is
// read-only: it carries a student role, the lesson id and no lesson token. The
// v3 bootstrap calls /lesson/checkin, which changes attendance and is NOT used.
// Presentation metadata is optional; a missing legacy bootstrap leaves problems
// detectable but without slide text or options.
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
	// v3Problems caches the read-only presentation metadata used to label a live
	// problem when the legacy bootstrap is unavailable. It is enrichment only:
	// it never enables an answer or submission path.
	v3Problems map[string]map[string]rainV3Problem
	v3Tried    map[string]time.Time
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
		ID          json.Number     `json:"ProblemID"`
		Type        string          `json:"Type"`
		ProblemType int             `json:"problemType"`
		Body        string          `json:"Body"`
		Result      json.RawMessage `json:"Result"`
		Bullets     []struct {
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

// rainID accepts both JSON strings and numbers. The live timeline sends
// 19-digit identifiers as strings (a JSON number would lose precision), while
// older presentation responses use numbers.
type rainID string

func (id *rainID) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*id = ""
		return nil
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*id = rainID(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*id = rainID(number.String())
	return nil
}

func (id rainID) String() string { return string(id) }

func (id rainID) positive() bool { return rainPositiveID(json.Number(id)) }

// classroomData reads the legacy presentation bootstrap. It supplies slide
// text and options for older lessons, but newer live lessons reject it. The
// caller treats it as optional metadata, never as permission to open the
// student socket.
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
		return data, errors.New("课堂只读课件元数据不可用")
	}
	if len(data.Presentations) > 100 {
		return data, errors.New("课堂课件数量超限")
	}
	return data, nil
}

// classroomUserID reads the signed-in student id from the independent user
// endpoint. It is only used as the websocket hello field; no write endpoint is
// contacted.
func (r *Rain) classroomUserID(ctx context.Context) (json.Number, error) {
	env, err := r.get(ctx, "/api/v3/user/basic-info")
	if err != nil {
		return "", err
	}
	if env.Code == nil || *env.Code != 0 {
		return "", errors.New("课堂用户信息不可用")
	}
	var user struct {
		ID rainID `json:"id"`
	}
	if json.Unmarshal(env.Data, &user) != nil || !user.ID.positive() {
		return "", errors.New("课堂用户信息缺少标识")
	}
	return json.Number(user.ID.String()), nil
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
	// Presentation metadata enriches problems with slide text and options, but
	// newer v3 lessons no longer serve the legacy bootstrap. Its absence must
	// not hide the read-only timeline: the socket below accepts a student
	// hello without a lesson token and never checks the user in.
	metadataCtx, stopMetadata := context.WithTimeout(ctx, 4*time.Second)
	data, metadataErr := r.classroomData(metadataCtx, lesson.LessonID)
	stopMetadata()
	if metadataErr != nil {
		data = rainClassroomData{}
	}
	userID := data.UserID
	if !rainPositiveID(userID) {
		read, stop := context.WithTimeout(ctx, 4*time.Second)
		id, err := r.classroomUserID(read)
		stop()
		if err == nil {
			userID = id
		}
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
	c := &LiveClassroom{rain: r, lesson: lesson, conn: conn, data: data, problems: make(map[string]ClassroomProblem), changed: make(chan struct{}), timing: make(map[string]*rainTimingRequest), revision: make(map[string]uint64), v3Problems: make(map[string]map[string]rainV3Problem), v3Tried: make(map[string]time.Time)}
	// The live timeline keys the hello by string. Sending a JSON number loses
	// precision for 19-digit lesson ids and the server answers lessonfinished.
	hello := map[string]any{"op": "hello", "role": "student", "lessonid": lesson.LessonID, "ver": 1}
	if rainPositiveID(userID) {
		hello["userid"] = userID.String()
	}
	if len(data.Auth) > 0 && string(data.Auth) != "null" && string(data.Auth) != `""` {
		hello["auth"] = data.Auth
	} else {
		// Read-only timeline access does not require a lesson token. Never call
		// /lesson/checkin to obtain one.
		hello["auth"] = 0
	}
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
				err := wsjson.Write(beat, c.conn, map[string]any{"op": "xintiao", "lessonid": c.lesson.LessonID})
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
			ProblemID rainID                `json:"problemid"`
			MessageID json.Number           `json:"msgid"`
			Prob      rainID                `json:"prob"`
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
	Type        string `json:"type"`
	Prob        rainID `json:"prob"`
	Pres        rainID `json:"pres"`
	SID         rainID `json:"sid"`
	Index       int    `json:"si"`
	DT          int64  `json:"dt"`
	Now         int64  `json:"now"`
	Limit       *int64 `json:"limit"`
	ProblemType *int   `json:"problemType"`
}

func (c *LiveClassroom) signalLocked() { close(c.changed); c.changed = make(chan struct{}) }

func (c *LiveClassroom) loadProblem(ctx context.Context, event rainTimelineProblem, callback func(ClassroomProblem)) error {
	if !event.Prob.positive() || !event.Pres.positive() {
		return nil
	}
	c.mu.Lock()
	_, exists := c.problems[event.Prob.String()]
	full := len(c.problems) >= 200
	c.mu.Unlock()
	if !exists && full {
		return errors.New("课堂题目数量超限")
	}
	choice := false
	if !exists {
		p, ok := classroomProblem(c.data, c.lesson.LessonID, event)
		if !ok && len(c.data.Presentations) > 0 {
			// Newly added presentations need one bounded read; cached slides avoid
			// doing an HTTP roundtrip for every entry of a reconnect timeline.
			read, stop := context.WithTimeout(ctx, 3*time.Second)
			data, err := c.rain.classroomData(read, c.lesson.LessonID)
			stop()
			if err == nil {
				c.data = data
				p, ok = classroomProblem(data, c.lesson.LessonID, event)
			}
		}
		if !ok && len(c.data.Presentations) == 0 {
			// Newer v3 lessons reject the legacy bootstrap. The read-only lesson
			// summary presentation still identifies the problem type for some
			// lessons; use it only to label the announcement, never to answer.
			if meta, found := c.v3Problem(ctx, event); found {
				p = detectedProblem(c.lesson.LessonID, event)
				p.Title, p.Body, p.Type, p.ProblemType = meta.Title, meta.Body, meta.Type, meta.ProblemType
				ok = p.ID != ""
			}
		}
		if !ok {
			// The lesson is a v3 live lesson whose presentation API needs a lesson
			// token. Keep the timeline event so it can still be announced, without
			// guessing options or answer state.
			p = detectedProblem(c.lesson.LessonID, event)
			ok = p.ID != ""
		}
		if ok {
			c.mu.Lock()
			c.problems[p.ID] = p
			c.signalLocked()
			c.mu.Unlock()
			callback(p) // Starts analysis immediately, before timing lookup.
			choice = p.Choice()
		}
	} else {
		c.mu.Lock()
		choice = c.problems[event.Prob.String()].Choice()
		c.mu.Unlock()
	}
	if !choice {
		// A detect-only problem has no options and no submission path, so the
		// timing round trip would add no capability.
		return nil
	}
	_, err := c.askTiming(ctx, event.Prob.String(), false)
	return err
}

// detectedProblem preserves the identity and deadline of a live problem whose
// slide payload is unavailable. It is never answerable through this struct.
func detectedProblem(lessonID string, event rainTimelineProblem) ClassroomProblem {
	p := ClassroomProblem{LessonID: lessonID, ID: event.Prob.String(), PresentationID: event.Pres.String()}
	if event.ProblemType != nil && *event.ProblemType > 0 && *event.ProblemType <= 100 {
		p.ProblemType = *event.ProblemType
	}
	if event.DT >= 946684800000 && event.DT <= 4102444800000 {
		p.StartedAt = time.UnixMilli(event.DT)
	}
	if event.Limit != nil && *event.Limit > 0 && !p.StartedAt.IsZero() {
		p.Deadline = p.StartedAt.Add(time.Duration(*event.Limit) * time.Second)
	}
	return p
}

func classroomProblem(data rainClassroomData, lessonID string, event rainTimelineProblem) (ClassroomProblem, bool) {
	for _, pres := range data.Presentations {
		if pres.ID.String() != event.Pres.String() {
			continue
		}
		for index, slide := range pres.Slides {
			if event.SID != "" && event.SID != "0" {
				if slide.ID.String() != event.SID.String() {
					continue
				}
			} else if index+1 != event.Index {
				continue
			}
			problem := slide.Problem
			if problem == nil || problem.ID.String() != event.Prob.String() {
				return ClassroomProblem{}, false
			}
			p := ClassroomProblem{LessonID: lessonID, ID: problem.ID.String(), PresentationID: pres.ID.String(), Title: pres.Title, Body: problem.Body, ImageURL: slide.Cover, Type: problem.Type, ProblemType: problem.ProblemType}
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
				if p.IsPoll() {
					// Poll options are display data only. They are not answer
					// labels, so arbitrary text is allowed and kept for the body.
					if label != "" && !seen[label] {
						seen[label] = true
						p.Options = append(p.Options, label)
					}
					continue
				}
				if len(label) != 1 || label[0] < 'A' || label[0] > 'Z' || seen[label] {
					return ClassroomProblem{}, false
				}
				seen[label] = true
				p.Options = append(p.Options, label)
			}
			if p.IsPoll() {
				// A poll is a first-class detected activity but never a choice
				// question for the answer helpers.
				return p, true
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

// rainV3Problem is the subset of the v3 lesson-summary presentation that labels
// a live timeline problem. It is deliberately read-only metadata: it does not
// carry an answer/submission capability.
type rainV3Problem struct {
	Title       string
	Body        string
	Type        string
	ProblemType int
	Anonymous   bool
}

// classroomProblemsV3 reads the student lesson-summary presentation. Newer live
// lessons reject the legacy bootstrap, but the summary presentation still maps a
// problem id to its type on deployments that expose it. Callers treat any error
// as "metadata unavailable"; the timeline announcement is never suppressed.
func (r *Rain) classroomProblemsV3(ctx context.Context, lessonID, presentationID string) (map[string]rainV3Problem, error) {
	if !rainPositiveID(json.Number(lessonID)) || !rainPositiveID(json.Number(presentationID)) {
		return nil, errors.New("课堂课件标识无效")
	}
	q := url.Values{"lesson_id": {lessonID}, "presentation_id": {presentationID}}
	env, err := r.get(ctx, "/api/v3/lesson-summary/student/presentation?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if env.Code == nil {
		return nil, errors.New("课堂课件响应格式变化")
	}
	if *env.Code != 0 {
		return nil, errors.New("课堂课件读取失败")
	}
	var data struct {
		Title        string `json:"title"`
		Presentation struct {
			Title string `json:"title"`
		} `json:"presentation"`
		Slides *[]struct {
			Problem *struct {
				ProblemID   rainID `json:"problemId"`
				ID          rainID `json:"id"`
				Type        string `json:"type"`
				ProblemType int    `json:"problemType"`
				Anonymous   bool   `json:"anonymous"`
				Body        string `json:"body"`
				Content     struct {
					ProblemID   rainID `json:"problemId"`
					ID          rainID `json:"id"`
					Type        string `json:"type"`
					ProblemType int    `json:"problemType"`
					Anonymous   bool   `json:"anonymous"`
					Body        string `json:"body"`
				} `json:"content"`
			} `json:"problem"`
		} `json:"slides"`
	}
	if json.Unmarshal(env.Data, &data) != nil || data.Slides == nil {
		return nil, errors.New("课堂课件格式变化")
	}
	if len(*data.Slides) > 5000 {
		return nil, errors.New("课堂课件数量超限")
	}
	title := strings.TrimSpace(data.Title)
	if title == "" {
		title = strings.TrimSpace(data.Presentation.Title)
	}
	problems := make(map[string]rainV3Problem, len(*data.Slides))
	for _, slide := range *data.Slides {
		problem := slide.Problem
		if problem == nil {
			continue
		}
		id := ""
		for _, candidate := range []rainID{problem.ProblemID, problem.ID, problem.Content.ProblemID, problem.Content.ID} {
			if candidate.positive() {
				id = candidate.String()
				break
			}
		}
		if id == "" {
			continue
		}
		kind := strings.TrimSpace(problem.Type)
		if kind == "" {
			kind = strings.TrimSpace(problem.Content.Type)
		}
		numeric := problem.ProblemType
		if numeric == 0 {
			numeric = problem.Content.ProblemType
		}
		// Only the poll name is inferred from the numeric type. Other numeric
		// types stay empty so this unverified metadata cannot enable a choice
		// answer, fallback submission, or model request.
		anonymous := problem.Anonymous || problem.Content.Anonymous
		if numeric == 3 && kind == "" {
			kind = "Polling"
		}
		if kind == "Polling" && anonymous {
			kind = "AnonymousPolling"
		}
		body := strings.TrimSpace(problem.Body)
		if body == "" {
			body = strings.TrimSpace(problem.Content.Body)
		}
		if _, exists := problems[id]; exists {
			continue
		}
		problems[id] = rainV3Problem{Title: title, Body: body, Type: kind, ProblemType: numeric, Anonymous: anonymous}
	}
	return problems, nil
}

// v3Problem fetches the presentation metadata for one live problem at most once
// per lesson and caches the result. A failed fetch is retried after five minutes
// so a socket reader is never blocked on the same unavailable endpoint for every
// new problem.
func (c *LiveClassroom) v3Problem(ctx context.Context, event rainTimelineProblem) (rainV3Problem, bool) {
	if c.rain == nil || !event.Pres.positive() || !event.Prob.positive() {
		return rainV3Problem{}, false
	}
	presentationID, problemID := event.Pres.String(), event.Prob.String()
	c.mu.Lock()
	if problems, ok := c.v3Problems[presentationID]; ok {
		meta, found := problems[problemID]
		c.mu.Unlock()
		return meta, found
	}
	if tried, ok := c.v3Tried[presentationID]; ok && time.Since(tried) < 5*time.Minute {
		c.mu.Unlock()
		return rainV3Problem{}, false
	}
	if len(c.v3Problems) >= 8 {
		c.mu.Unlock()
		return rainV3Problem{}, false
	}
	if c.v3Problems == nil {
		c.v3Problems = make(map[string]map[string]rainV3Problem)
	}
	if c.v3Tried == nil {
		c.v3Tried = make(map[string]time.Time)
	}
	c.v3Tried[presentationID] = time.Now()
	c.mu.Unlock()
	fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	problems, err := c.rain.classroomProblemsV3(fetchCtx, c.lesson.LessonID, presentationID)
	cancel()
	if err != nil {
		return rainV3Problem{}, false
	}
	c.mu.Lock()
	if len(c.v3Problems) < 8 {
		c.v3Problems[presentationID] = problems
	}
	meta, found := problems[problemID]
	c.mu.Unlock()
	return meta, found
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
	if err := wsjson.Write(ctx, c.conn, map[string]any{"op": "probleminfo", "lessonid": c.lesson.LessonID, "problemid": id, "msgid": request.id}); err != nil {
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
	event := rainTimelineProblem{Prob: rainID(id), Pres: rainID(p.PresentationID)}
	// Locate the exact problem without relying on its old slide position.
	for _, pres := range data.Presentations {
		if pres.ID.String() == event.Pres.String() {
			for i, slide := range pres.Slides {
				if slide.Problem != nil && slide.Problem.ID.String() == id {
					event.SID = rainID(slide.ID.String())
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
