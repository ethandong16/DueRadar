package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func classroomFixture(result, kind string) string {
	return fmt.Sprintf(`{"status":0,"data":{"userID":7,"userAuth":"fixture-auth","activePresentationID":20,"presentationList":[{"presentationID":20,"Title":"Fixture lesson","Slides":[{"lessonSlideID":30,"Problem":{"ProblemID":40,"Type":%q,"Body":"1+1? A:2 B:3","Result":%s,"Bullets":[{"Label":"A"},{"Label":"B"}]}}]}]}}`, kind, result)
}

func classroomFixtureData(t *testing.T, result, kind string) rainClassroomData {
	t.Helper()
	return classroomRawData(t, classroomFixture(result, kind))
}

func classroomRawData(t *testing.T, raw string) rainClassroomData {
	t.Helper()
	var env rainEnvelope
	var data rainClassroomData
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func classroomTestProblem() ClassroomProblem {
	now := time.Now()
	return ClassroomProblem{LessonID: "10", ID: "40", PresentationID: "20", Type: "MultipleChoice", Body: "1+1? A:2 B:3", Options: []string{"A", "B"}, AnswerKnown: true, StartedAt: now.Add(-time.Minute), TimingAt: now, Deadline: now.Add(8 * time.Second)}
}

func TestClassroomChoiceAndFallbackGuards(t *testing.T) {
	now := time.Now()
	base := classroomTestProblem()
	base.TimingAt = now
	base.Deadline = now.Add(10 * time.Second)
	for _, tc := range []struct {
		name string
		edit func(*ClassroomProblem)
		want bool
	}{
		{"final ten seconds", func(*ClassroomProblem) {}, true},
		{"multi answer selects one", func(p *ClassroomProblem) { p.Type = "MultipleChoiceMA" }, true},
		{"too early", func(p *ClassroomProblem) { p.Deadline = now.Add(10*time.Second + 1) }, false},
		{"closed deadline", func(p *ClassroomProblem) { p.Deadline = now }, false},
		{"unknown deadline", func(p *ClassroomProblem) { p.Deadline = time.Time{} }, false},
		{"missing result", func(p *ClassroomProblem) { p.AnswerKnown = false }, false},
		{"manually answered", func(p *ClassroomProblem) { p.Answered = true }, false},
		{"finished", func(p *ClassroomProblem) { p.Closed = true }, false},
		{"old timing", func(p *ClassroomProblem) { p.TimingAt = now.Add(-5 * time.Second) }, false},
		{"future timing", func(p *ClassroomProblem) { p.TimingAt = now.Add(time.Second) }, false},
		{"future start", func(p *ClassroomProblem) { p.StartedAt = now.Add(time.Second) }, false},
		{"unknown start", func(p *ClassroomProblem) { p.StartedAt = time.Time{} }, false},
		{"poll", func(p *ClassroomProblem) { p.Type = "Polling" }, false},
		{"anonymous poll", func(p *ClassroomProblem) { p.Type = "AnonymousPolling" }, false},
		{"paper", func(p *ClassroomProblem) { p.Type = "Paper" }, false},
		{"duplicate label", func(p *ClassroomProblem) { p.Options = []string{"A", "A"} }, false},
		{"unknown labels", func(p *ClassroomProblem) { p.Options = []string{"A", "AB"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.edit(&p)
			if got := p.FallbackReady(now); got != tc.want {
				t.Fatalf("ready=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestClassroomProblemExactIdentityAndResult(t *testing.T) {
	for _, tc := range []struct {
		name, result    string
		known, answered bool
	}{
		{"null", "null", true, false},
		{"empty", `""`, true, false},
		{"answered", `"AB"`, true, true},
		{"object", `{}`, false, false},
		{"number", `0`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := classroomFixtureData(t, tc.result, "MultipleChoice")
			event := rainTimelineProblem{Prob: "40", Pres: "20", SID: "30", DT: time.Now().UnixMilli()}
			p, ok := classroomProblem(data, "10", event)
			if !ok || p.AnswerKnown != tc.known || p.Answered != tc.answered || p.ID != "40" || p.StartedAt.IsZero() {
				t.Fatalf("unexpected parsed problem: %+v, found=%v", p, ok)
			}
			event.Prob = "41"
			if _, ok := classroomProblem(data, "10", event); ok {
				t.Fatal("wrong problem ID matched")
			}
		})
	}
	data := classroomFixtureData(t, "null", "MultipleChoice")
	event := rainTimelineProblem{Prob: "40", Pres: "20", Index: 1}
	data.Presentations[0].Slides[0].Result = json.RawMessage(`"B"`)
	p, ok := classroomProblem(data, "10", event)
	if !ok || !p.Answered {
		t.Fatal("slide-specific student result was ignored")
	}
	data.Presentations[0].Slides[0].Result = nil
	data.Presentations[0].Slides[0].Problem.Result = nil
	p, ok = classroomProblem(data, "10", event)
	if !ok || p.AnswerKnown {
		t.Fatal("missing result guessed as unanswered")
	}
	for _, kind := range []string{"Polling", "AnonymousPolling"} {
		p, ok := classroomProblem(classroomFixtureData(t, "null", kind), "10", event)
		if !ok || !p.IsPoll() || p.Choice() || p.FallbackReady(time.Now()) {
			t.Fatalf("poll type %s was not detected as a non-answerable poll: %+v, found=%v", kind, p, ok)
		}
	}
	for _, kind := range []string{"Subjective", "Exam", "Quiz"} {
		if _, ok := classroomProblem(classroomFixtureData(t, "null", kind), "10", event); ok {
			t.Fatalf("accepted unsupported type %s", kind)
		}
	}
}

// A poll is a first-class detected activity but never an answerable question:
// its option text is kept for the notification, while every submission path
// stays closed.
func TestClassroomPollDetectionIsReadOnly(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, kind string
		anonymous  bool
	}{
		{"poll", "Polling", false},
		{"anonymous poll", "AnonymousPolling", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"status":0,"data":{"activePresentationID":20,"presentationList":[{"presentationID":20,"Title":"第 3 讲","Slides":[{"lessonSlideID":30,"Problem":{"ProblemID":40,"Type":%q,"Body":"你更认同哪种解释？","Result":"A","Bullets":[{"Label":"赞成"},{"Label":"反对"}]}}]}]}}`, tc.kind)
			event := rainTimelineProblem{Prob: "40", Pres: "20", SID: "30", DT: now.Add(-time.Minute).UnixMilli()}
			p, ok := classroomProblem(classroomRawData(t, raw), "10", event)
			if !ok || !p.IsPoll() || p.AnonymousPoll() != tc.anonymous || p.Type != tc.kind {
				t.Fatalf("poll not detected: %+v, found=%v", p, ok)
			}
			if p.Choice() || p.FallbackReady(now) {
				t.Fatal("poll exposed an automatic answer path")
			}
			if len(p.Options) != 2 || p.Options[0] != "赞成" || p.Options[1] != "反对" {
				t.Fatalf("poll display options lost: %v", p.Options)
			}
			if p.Title != "第 3 讲" || p.Body != "你更认同哪种解释？" {
				t.Fatalf("poll text lost: %+v", p)
			}
		})
	}
	for _, typ := range []int{3, 4, 10} {
		p := detectedProblem("10", rainTimelineProblem{Prob: "40", Pres: "20", ProblemType: &typ})
		if p.ProblemType != typ || p.IsPoll() != (typ == 3) {
			t.Fatalf("numeric type %d misclassified: %+v", typ, p)
		}
	}
}

func TestClassroomProblemsV3ReadsOnlyPresentationMetadata(t *testing.T) {
	calls := 0
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		calls++
		if req.Method != "GET" || req.URL.Path != "/api/v3/lesson-summary/student/presentation" {
			t.Errorf("unexpected presentation request: %s %s", req.Method, req.URL.Path)
		}
		if req.URL.Query().Get("lesson_id") != "10" || req.URL.Query().Get("presentation_id") != "20" {
			t.Errorf("unexpected presentation query: %s", req.URL.RawQuery)
		}
		fmt.Fprint(w, `{"code":0,"data":{"title":"课程课件","presentation":{"title":"第二讲"},"slides":[
			{"problem":{"problemId":"40","problemType":3,"body":"你选哪一个？"}},
			{"problem":{"content":{"id":"41","type":"MultipleChoice","problemType":2,"body":"1+1?"}}},
			{"problem":{"problemId":"42","problemType":2}},
			{"problem":{"problemId":"43","problemType":3,"anonymous":true}},
			{"problem":{"content":{"id":"44","problemType":3,"anonymous":true}}},
			{"problem":null},
			{"problem":{"problemId":"bad"}}
		]}}`)
	})
	problems, err := r.classroomProblemsV3(context.Background(), "10", "20")
	if err != nil || calls != 1 {
		t.Fatalf("presentation metadata not read: %v, calls=%d", err, calls)
	}
	if p := problems["40"]; p.Type != "Polling" || p.ProblemType != 3 || p.Title != "课程课件" || p.Body != "你选哪一个？" {
		t.Fatalf("numeric poll not labelled: %+v", p)
	}
	if p := problems["41"]; p.Type != "MultipleChoice" || p.ProblemType != 2 || p.Body != "1+1?" {
		t.Fatalf("nested content not read: %+v", p)
	}
	// A non-poll numeric type stays metadata; it must not be translated into an
	// answerable type or an automatic submission path.
	if p := problems["42"]; p.Type != "" || p.ProblemType != 2 || p.Body != "" {
		t.Fatalf("non-poll numeric type was interpreted: %+v", p)
	}
	// The v3 API marks anonymity with a boolean rather than a distinct type.
	if p := problems["43"]; p.Type != "AnonymousPolling" || !p.Anonymous || p.ProblemType != 3 {
		t.Fatalf("anonymous poll not labelled: %+v", p)
	}
	if p := problems["44"]; p.Type != "AnonymousPolling" || !p.Anonymous {
		t.Fatalf("nested anonymous poll not labelled: %+v", p)
	}
	if len(problems) != 5 {
		t.Fatalf("unexpected problem count: %v", problems)
	}
}

func TestClassroomTimingUsesServerClockAndConservativeRTT(t *testing.T) {
	p := classroomTestProblem()
	c := &LiveClassroom{problems: map[string]ClassroomProblem{p.ID: p}, changed: make(chan struct{})}
	// Server is deliberately a year away from this device's clock.
	dt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	limit := int64(60)
	sentAt := time.Now().Add(-2 * time.Second)
	c.updateTimingAt(p.ID, dt, dt+50_000, &limit, sentAt, func(p ClassroomProblem) {
		if left := time.Until(p.Deadline); left < 7*time.Second || left > 8*time.Second || p.Closed {
			t.Fatalf("wrong server-relative deadline: %v", left)
		}
	})
	for _, limit := range []int64{-1, 0, 100000} {
		c.updateTimingAt(p.ID, dt, dt+50_000, &limit, time.Now(), func(p ClassroomProblem) {
			if !p.Deadline.IsZero() || p.FallbackReady(time.Now()) {
				t.Fatal("unknown/unlimited/closed period permits submission")
			}
		})
	}
}

func TestOpenClassroomRejectsNonStudentsAndKeepsReadOnlyHandshake(t *testing.T) {
	var infoCalls atomic.Int32
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v/lesson/lesson_info_v2":
			// v3 lessons no longer serve the legacy presentation bootstrap.
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errorcode":500,"errormsg":"'NoneType' object has no attribute 'set_cookie'"}`)
		case "/api/v3/user/basic-info":
			infoCalls.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"id":"7"}}`)
		case "/wsapp/":
			conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			var hello map[string]json.RawMessage
			if err := wsjson.Read(req.Context(), conn, &hello); err != nil {
				t.Error(err)
				return
			}
			if string(hello["op"]) != `"hello"` || string(hello["role"]) != `"student"` {
				t.Errorf("wrong student handshake: %v", hello)
			}
			// A numeric lesson id loses precision server-side and is answered as
			// lessonfinished. The read-only handshake must send the string form and
			// must not carry a lesson token.
			if string(hello["lessonid"]) != `"1811530394936651392"` || string(hello["auth"]) != "0" {
				t.Errorf("unsafe handshake: %v", hello)
			}
			if string(hello["userid"]) != `"7"` {
				t.Errorf("missing read-only user id: %v", hello)
			}
		default:
			t.Errorf("unexpected write/endpoint: %s %s", req.Method, req.URL.Path)
		}
	})
	for _, role := range []int{0, 1, 2, 4} {
		if c, err := r.OpenClassroom(context.Background(), LiveLesson{LessonID: "10", ClassroomID: "100", Role: role}); err == nil || c != nil {
			t.Fatal("opened a nonstudent classroom")
		}
	}
	if infoCalls.Load() != 0 {
		t.Fatal("nonstudent guard performed I/O")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := r.OpenClassroom(ctx, LiveLesson{LessonID: "1811530394936651392", ClassroomID: "100", Role: 5})
	if err != nil || c == nil {
		t.Fatalf("read-only live socket did not open: %v", err)
	}
	c.Close()
	if infoCalls.Load() != 1 {
		t.Fatal("missing read-only user id fallback")
	}
}

func TestSubmitClassroomChoiceSingleTransportAndResult(t *testing.T) {
	for _, tc := range []struct {
		name, response, result string
		wantErr                bool
	}{
		{"submitted", `{"status":0,"data":{"status_code":0}}`, "submitted", false},
		{"already answered", `{"success":true,"data":{"status_code":4}}`, "already_answered", false},
		{"rejected", `{"status":0,"data":{"status_code":2}}`, "", true},
		{"unknown status", `{"status":0,"data":{"status_code":99}}`, "", true},
		{"missing result", `{"status":0,"data":{}}`, "", true},
		{"wrong envelope", `{"code":0,"data":{"status_code":0}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				attempts.Add(1)
				if req.Method != "POST" || req.URL.Path != "/v/lesson/answer_lesson_problem" || req.Header.Get("classroom-id") != "100" {
					t.Errorf("unexpected submission endpoint/identity")
				}
				var body struct {
					ID              json.Number `json:"lesson_problem_id"`
					Result          string      `json:"result"`
					Retries         int         `json:"retry_times"`
					Start, Duration int64
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.ID != "40" || body.Result != "B" || body.Retries != 0 {
					t.Errorf("incorrect one-letter submission: %+v, err=%v", body, err)
				}
				fmt.Fprint(w, tc.response)
			})
			p := classroomTestProblem()
			c := &LiveClassroom{rain: r, lesson: LiveLesson{LessonID: "10", ClassroomID: "100", Role: 5}, problems: map[string]ClassroomProblem{p.ID: p}}
			result, err := c.SubmitChoice(context.Background(), p, "B")
			if (err != nil) != tc.wantErr || result != tc.result || attempts.Load() != 1 {
				t.Fatalf("result=%q err=%v attempts=%d", result, err, attempts.Load())
			}
			p.Answered = true
			c.problems[p.ID] = p
			if _, err := c.SubmitChoice(context.Background(), p, "B"); err == nil || attempts.Load() != 1 {
				t.Fatal("manually answered problem was submitted")
			}
		})
	}
}

// Both ends of this fixture use local sockets; the production origin and cookie
// jar remain intact. No real classroom, check-in, vote or answer is contacted.
func openClassroomSocket(t *testing.T, answered *atomic.Bool) (*LiveClassroom, *websocket.Conn, <-chan map[string]json.RawMessage, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	peers := make(chan *websocket.Conn, 1)
	requests := make(chan map[string]json.RawMessage, 16)
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v/lesson/lesson_info_v2":
			if req.Method != "GET" {
				t.Error("bootstrap must be read-only")
			}
			result := "null"
			if answered.Load() {
				result = `"A"`
			}
			fmt.Fprint(w, classroomFixture(result, "MultipleChoice"))
		case "/wsapp/":
			if req.Header.Get("Origin") != rainOrigin || req.Header.Get("Cookie") == "" {
				t.Error("missing websocket origin/session")
			}
			conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			peers <- conn
			for {
				var msg map[string]json.RawMessage
				if err := wsjson.Read(ctx, conn, &msg); err != nil {
					return
				}
				select {
				case requests <- msg:
				case <-ctx.Done():
					return
				}
			}
		default:
			t.Errorf("unexpected endpoint %s", req.URL.Path)
			http.NotFound(w, req)
		}
	})
	c, err := r.OpenClassroom(ctx, LiveLesson{LessonID: "10", ClassroomID: "100", Role: 5})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var peer *websocket.Conn
	select {
	case peer = <-peers:
	case <-ctx.Done():
		t.Fatal("no socket peer")
	}
	t.Cleanup(func() { cancel(); c.Close(); peer.CloseNow() })
	hello := socketRequest(t, ctx, requests)
	if string(hello["op"]) != `"hello"` || string(hello["role"]) != `"student"` {
		t.Fatal("wrong student handshake")
	}
	if string(hello["lessonid"]) != `"10"` {
		t.Fatal("lesson id must be sent as a string")
	}
	return c, peer, requests, ctx
}

func socketRequest(t *testing.T, ctx context.Context, requests <-chan map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	select {
	case msg := <-requests:
		return msg
	case <-ctx.Done():
		t.Fatal("timed out waiting for socket request")
		return nil
	}
}

func socketWrite(t *testing.T, ctx context.Context, peer *websocket.Conn, message any) {
	t.Helper()
	if err := wsjson.Write(ctx, peer, message); err != nil {
		t.Fatal(err)
	}
}

func TestClassroomPreflightRequiresItsOwnTimingReply(t *testing.T) {
	for _, mode := range []string{"new ID only", "missing echo"} {
		t.Run(mode, func(t *testing.T) {
			var answered atomic.Bool
			c, peer, requests, ctx := openClassroomSocket(t, &answered)
			done := make(chan struct{})
			go func() { defer close(done); _ = c.Run(ctx, func(ClassroomProblem) {}) }()
			t.Cleanup(func() { c.Close(); <-done })
			now := time.Now().UnixMilli()
			socketWrite(t, ctx, peer, map[string]any{"op": "unlockproblem", "problem": map[string]any{"prob": 40, "pres": 20, "sid": 30, "dt": now - 50_000}})
			initial := socketRequest(t, ctx, requests) // Deliberately left outstanding.
			readCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			type result struct {
				p   ClassroomProblem
				err error
			}
			preflight := make(chan result, 1)
			go func() { p, err := c.CurrentProblem(readCtx, "40"); preflight <- result{p, err} }()
			query := socketRequest(t, ctx, requests)
			if string(initial["msgid"]) == string(query["msgid"]) {
				t.Fatal("preflight inherited the discovery request")
			}
			// None of these replies can complete the final preflight. Their long
			// periods also make accidental acceptance distinguishable in the result.
			for _, id := range []any{initial["msgid"], nil} {
				reply := map[string]any{"op": "probleminfo", "problemid": 40, "dt": now - 50_000, "now": now, "limit": 120}
				if id != nil {
					reply["msgid"] = id
				}
				socketWrite(t, ctx, peer, reply)
			}
			if mode == "missing echo" {
				// A following close is a reader-order barrier for the rejected replies.
				socketWrite(t, ctx, peer, map[string]any{"op": "lessonfinished"})
			} else {
				socketWrite(t, ctx, peer, map[string]any{"op": "probleminfo", "problemid": 40, "msgid": query["msgid"], "dt": now - 50_000, "now": now, "limit": 60})
			}
			select {
			case got := <-preflight:
				if mode == "missing echo" {
					if got.err == nil {
						t.Fatal("uncorrelated timing authorized a preflight")
					}
				} else if got.err != nil || !got.p.FallbackReady(time.Now()) {
					t.Fatalf("new correlated response not used: %+v, %v", got.p, got.err)
				}
			case <-ctx.Done():
				t.Fatal("preflight did not finish")
			}
		})
	}
}

// v3 live lessons reject the legacy presentation bootstrap and send every
// timeline identifier as a string. Detection must survive both facts.
func TestClassroomSocketV3StringTimelineWithoutBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	peers := make(chan *websocket.Conn, 1)
	requests := make(chan map[string]json.RawMessage, 8)
	r := rainTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v/lesson/lesson_info_v2":
			fmt.Fprint(w, `{"errorcode":500,"errormsg":"'NoneType' object has no attribute 'set_cookie'"}`)
		case "/api/v3/user/basic-info":
			fmt.Fprint(w, `{"code":0,"data":{"id":"96351415"}}`)
		case "/api/v3/lesson-summary/student/presentation":
			if req.Method != "GET" || req.URL.Query().Get("lesson_id") != "1811530394936651392" || req.URL.Query().Get("presentation_id") != "1811535912082150912" {
				t.Errorf("unsafe presentation request: %s %s", req.Method, req.URL.RawQuery)
			}
			fmt.Fprint(w, `{"code":0,"data":{"title":"概率统计","presentation":{"title":"第三章"},"slides":[{"problem":{"problemId":"1811535912124093961","problemType":3,"body":"你更喜欢哪种推导？"}}]}}`)
		case "/wsapp/":
			conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			peers <- conn
			for {
				var msg map[string]json.RawMessage
				if err := wsjson.Read(ctx, conn, &msg); err != nil {
					return
				}
				select {
				case requests <- msg:
				case <-ctx.Done():
					return
				}
			}
		default:
			t.Errorf("unexpected endpoint %s", req.URL.Path)
			http.NotFound(w, req)
		}
	})
	c, err := r.OpenClassroom(ctx, LiveLesson{LessonID: "1811530394936651392", ClassroomID: "31739239", Role: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	peer := <-peers
	hello := socketRequest(t, ctx, requests)
	if string(hello["lessonid"]) != `"1811530394936651392"` || string(hello["auth"]) != "0" || string(hello["userid"]) != `"96351415"` {
		t.Fatalf("unsafe v3 handshake: %v", hello)
	}
	observed := make(chan ClassroomProblem, 4)
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx, func(p ClassroomProblem) { observed <- p }) }()
	defer func() { c.Close(); <-done }()
	now := time.Now().UnixMilli()
	socketWrite(t, ctx, peer, map[string]any{"op": "hello", "timeline": []any{
		map[string]any{"type": "problem", "prob": "1811535912124093961", "pres": "1811535912082150912", "sid": "1811535912124093961", "dt": now, "limit": 60},
	}})
	p := <-observed
	if p.ID != "1811535912124093961" || p.PresentationID != "1811535912082150912" || p.StartedAt.IsZero() || p.Deadline.Sub(p.StartedAt) != time.Minute || p.Choice() {
		t.Fatalf("unexpected detect-only problem: %+v", p)
	}
	// The v3 summary labelled this timeline problem as a poll. The text is used
	// for the notification, but the problem still has no answer path.
	if !p.IsPoll() || p.ProblemType != 3 || p.Type != "Polling" || p.Title != "概率统计" || p.Body != "你更喜欢哪种推导？" {
		t.Fatalf("v3 poll metadata lost: %+v", p)
	}
	socketWrite(t, ctx, peer, map[string]any{"op": "unlockproblem", "problem": map[string]any{
		"prob": "1811535912124093962", "pres": "1811535912082150912", "sid": "1811535912124093962", "dt": now, "limit": 45,
	}})
	p = <-observed
	if p.ID != "1811535912124093962" || p.Deadline.Sub(p.StartedAt) != 45*time.Second {
		t.Fatalf("unexpected live push problem: %+v", p)
	}
	// No presentation content means no answer submission path; the reader must
	// not send a timing query for a problem it cannot act on.
	select {
	case extra := <-requests:
		t.Fatalf("unexpected socket request: %v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClassroomProblemCollectionIsBounded(t *testing.T) {
	c := &LiveClassroom{problems: make(map[string]ClassroomProblem)}
	for i := range 200 {
		id := fmt.Sprint(i + 1)
		c.problems[id] = ClassroomProblem{ID: id}
	}
	if err := c.loadProblem(context.Background(), rainTimelineProblem{Prob: "201", Pres: "20"}, func(ClassroomProblem) {
		t.Error("overflow question was observed")
	}); err == nil || len(c.problems) != 200 {
		t.Fatal("question collection exceeded the bound")
	}
}

func TestClassroomSocketFreshPreflightAndStateRevisions(t *testing.T) {
	for _, mode := range []string{"unanswered", "manual answer", "extension", "finished", "lesson finished"} {
		t.Run(mode, func(t *testing.T) {
			var answered atomic.Bool
			c, peer, requests, ctx := openClassroomSocket(t, &answered)
			observed := make(chan ClassroomProblem, 16)
			done := make(chan struct{})
			go func() { defer close(done); _ = c.Run(ctx, func(p ClassroomProblem) { observed <- p }) }()
			t.Cleanup(func() { c.Close(); <-done })
			now := time.Now().UnixMilli()
			socketWrite(t, ctx, peer, map[string]any{"op": "hello", "timeline": []any{
				map[string]any{"type": "vote", "prob": 99, "pres": 20, "sid": 30, "dt": now - 50_000},
				map[string]any{"type": "problem", "prob": 40, "pres": 20, "sid": 30, "dt": now - 50_000},
			}})
			initial := socketRequest(t, ctx, requests)
			if string(initial["op"]) != `"probleminfo"` || string(initial["problemid"]) != `"40"` {
				t.Fatal("wrong live problem query")
			}
			socketWrite(t, ctx, peer, map[string]any{"op": "probleminfo", "problemid": 40, "dt": now - 50_000, "now": now, "limit": 60})
			for {
				select {
				case p := <-observed:
					if !p.Deadline.IsZero() {
						goto timed
					}
				case <-ctx.Done():
					t.Fatal("no problem deadline")
				}
			}
		timed:
			if mode == "manual answer" {
				answered.Store(true)
			}
			type preflightResult struct {
				p   ClassroomProblem
				err error
			}
			preflight := make(chan preflightResult, 1)
			go func() { p, err := c.CurrentProblem(ctx, "40"); preflight <- preflightResult{p, err} }()
			query := socketRequest(t, ctx, requests)
			if string(query["problemid"]) != `"40"` {
				t.Fatal("preflight reused an obsolete result")
			}
			switch mode {
			case "extension":
				socketWrite(t, ctx, peer, map[string]any{"op": "extendtime", "problem": map[string]any{"prob": 40, "dt": now - 50_000, "now": now, "limit": 120}})
			case "finished":
				socketWrite(t, ctx, peer, map[string]any{"op": "problemfinished", "prob": 40})
			case "lesson finished":
				socketWrite(t, ctx, peer, map[string]any{"op": "lessonfinished"})
			}
			// An earlier query response must not override a subsequent extension/close.
			if mode != "lesson finished" {
				socketWrite(t, ctx, peer, map[string]any{"op": "probleminfo", "problemid": 40, "msgid": query["msgid"], "dt": now - 50_000, "now": now, "limit": 60})
			}
			select {
			case result := <-preflight:
				if mode == "extension" || mode == "finished" || mode == "lesson finished" {
					if result.err == nil || result.p.FallbackReady(time.Now()) {
						t.Fatal("stale timing response authorized an answer")
					}
				} else if result.err != nil || !result.p.AnswerKnown || result.p.Answered != answered.Load() || result.p.Deadline.IsZero() {
					t.Fatalf("incorrect fresh preflight: %+v, %v", result.p, result.err)
				}
			case <-ctx.Done():
				t.Fatal("preflight did not finish")
			}
			c.mu.Lock()
			count := len(c.problems)
			c.mu.Unlock()
			if count != 1 {
				t.Fatal("non-problem timeline entry was collected")
			}
		})
	}
}
