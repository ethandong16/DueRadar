package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	smsPollInterval = 2 * time.Second
	smsCodeWait     = 90 * time.Second
	smsMaxBody      = 1 << 20
)

// smsMessage is one message from the read-only SMS module API. Only the fields
// needed to de-duplicate and extract a code are decoded.
type smsMessage struct {
	ID            int64  `json:"id"`
	ReceivedAtRaw string `json:"received_at_raw"`
	Sender        string `json:"sender"`
	Text          string `json:"text"`
	ImportedAtUTC string `json:"imported_at_utc"`
}

type smsSource interface {
	Recent(context.Context) ([]smsMessage, error)
}

type httpSMSSource struct {
	base   string
	token  string
	client *http.Client
}

// newSMSSourceFromEnv configures the SMS module from the deployment
// environment. Both values are required; without them automatic login stays
// unavailable rather than silently polling a wrong host.
func newSMSSourceFromEnv() smsSource {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("DUERADAR_SMS_API_URL")), "/")
	token := strings.TrimSpace(os.Getenv("DUERADAR_SMS_API_TOKEN"))
	if base == "" || token == "" {
		return nil
	}
	return &httpSMSSource{base: base, token: token, client: &http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (s *httpSMSSource) Recent(ctx context.Context) ([]smsMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/api/sms/recent", nil)
	if err != nil {
		return nil, errors.New("短信模块地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("短信模块连接失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("短信模块暂未接受请求")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, smsMaxBody+1))
	if err != nil {
		return nil, errors.New("短信模块响应读取失败")
	}
	if len(data) > smsMaxBody {
		return nil, errors.New("短信模块响应过大")
	}
	var payload struct {
		Messages []smsMessage `json:"messages"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, errors.New("短信模块返回的数据格式无效")
	}
	return payload.Messages, nil
}

// smsCollector implements platform.SMSCollector. It only considers messages
// newer than the baseline captured before the login request, and either
// matching one of the configured keywords or coming from an allowed sender.
type smsCollector struct {
	source   smsSource
	keywords []string
	senders  []string
	poll     time.Duration
	wait     time.Duration
}

func newSMSCollector(source smsSource) *smsCollector {
	return &smsCollector{
		source:   source,
		keywords: envList("DUERADAR_SMS_KEYWORDS", []string{"智慧树", "知到"}),
		senders:  envList("DUERADAR_SMS_SENDERS", nil),
		poll:     smsPollInterval,
		wait:     smsCodeWait,
	}
}

func envList(name string, fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	out := []string{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '，' || r == ' ' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (c *smsCollector) Baseline(ctx context.Context) (int64, error) {
	messages, err := c.source.Recent(ctx)
	if err != nil {
		return 0, err
	}
	var newest int64
	for _, message := range messages {
		if message.ID > newest {
			newest = message.ID
		}
	}
	return newest, nil
}

func (c *smsCollector) WaitForCode(ctx context.Context, baseline int64, since time.Time) (string, error) {
	deadline := time.Now().Add(c.wait)
	for {
		messages, err := c.source.Recent(ctx)
		if err == nil {
			if code, ok := c.find(messages, baseline, since); ok {
				return code, nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", errors.New("短信验证码等待超时")
		}
		if remaining > c.poll {
			remaining = c.poll
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *smsCollector) find(messages []smsMessage, baseline int64, since time.Time) (string, bool) {
	for _, message := range messages {
		if message.ID <= baseline || c.tooOld(message, since) || !c.accepts(message) {
			continue
		}
		if code, ok := smsCode(message.Text); ok {
			return code, true
		}
	}
	return "", false
}

// tooOld guards against a backlog message that entered the module after the
// baseline read. The tolerance absorbs clock skew between this host and the
// SMS module; the id baseline remains the primary de-duplication boundary.
func (c *smsCollector) tooOld(message smsMessage, since time.Time) bool {
	if message.ImportedAtUTC == "" {
		return false
	}
	imported, err := time.Parse(time.RFC3339, message.ImportedAtUTC)
	if err != nil {
		return false
	}
	return imported.Before(since.Add(-time.Minute))
}

func (c *smsCollector) accepts(message smsMessage) bool {
	for _, sender := range c.senders {
		if strings.Contains(message.Sender, sender) {
			return true
		}
	}
	if len(c.keywords) == 0 {
		return len(c.senders) == 0
	}
	for _, keyword := range c.keywords {
		if strings.Contains(message.Text, keyword) {
			return true
		}
	}
	return false
}

var (
	smsCodeNear   = regexp.MustCompile(`(?:验证码|动态码|校验码)[^0-9]{0,10}(\d{4,6})`)
	smsCodeBefore = regexp.MustCompile(`(\d{4,6})[^0-9]{0,6}(?:是|为)?[^0-9]{0,4}(?:验证码|动态码|校验码)`)
)

// smsCode returns a code that is adjacent to a code label. A bare 4-6 digit
// number is deliberately not accepted: phone tails, dates, and order numbers
// can otherwise be submitted as the verification code.
func smsCode(text string) (string, bool) {
	if match := smsCodeNear.FindStringSubmatch(text); len(match) == 2 {
		return match[1], true
	}
	if match := smsCodeBefore.FindStringSubmatch(text); len(match) == 2 {
		return match[1], true
	}
	return "", false
}
