package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	zhidaoPassport  = "https://passport.zhihuishu.com"
	zhidaoCloudBase = "https://cloudapi.polymas.com"
)

// Password-login failures are distinct so the server can tell the user what
// actually happened without exposing platform payloads or credentials.
var (
	ErrPasswordRejected = errors.New("知到手机号或密码错误")
	ErrCaptchaRequired  = errors.New("知到要求图形验证码，无法自动登录")
	ErrSlideRequired    = errors.New("知到要求滑块验证，无法自动登录")
	ErrAccountLocked    = errors.New("知到账号暂时被限制登录")
	ErrSimplePassword   = errors.New("知到要求先修改过于简单的密码")
	ErrNeedBinding      = errors.New("知到账号需要额外身份验证，无法自动登录")
	ErrSMSTimeout       = errors.New("未收到知到短信验证码")
	ErrSMSUnavailable   = errors.New("短信模块不可用")
	ErrSMSCodeRejected  = errors.New("知到未接受短信验证码")
	ErrLoginIncomplete  = errors.New("知到登录未完成")
	errNoZhidaoSession  = errors.New("课程中心会话尚未建立")
)

// SMSCollector supplies one fresh platform code. Baseline captures the newest
// message id before the login attempt; WaitForCode only accepts messages newer
// than that id and imported after since.
type SMSCollector interface {
	Baseline(ctx context.Context) (int64, error)
	WaitForCode(ctx context.Context, baseline int64, since time.Time) (string, error)
}

// PasswordLogin performs the phone-and-password flow, including the SMS second
// factor when the platform asks for it. It returns the same serialized session
// shape as the QR flow, already validated against the identity endpoint.
func PasswordLogin(ctx context.Context, phone, password string, sms SMSCollector) (string, error) {
	client, jar := newZhidaoClient()
	return passwordLogin(ctx, client, jar, phone, password, sms)
}

func passwordLogin(ctx context.Context, client *http.Client, jar *zhidaoJar, phone, password string, sms SMSCollector) (string, error) {
	var baseline int64
	if sms != nil {
		var err error
		if baseline, err = sms.Baseline(ctx); err != nil {
			return "", ErrSMSUnavailable
		}
	}
	pwd, uuid, err := validateAccountPassword(ctx, client, phone, password)
	if err != nil {
		return "", err
	}
	if err := checkNeedAuth(ctx, client, uuid); err != nil {
		return "", err
	}
	needSMS, err := loginOnce(ctx, client, pwd)
	if err != nil {
		return "", err
	}
	if needSMS {
		if sms == nil {
			return "", ErrSMSTimeout
		}
		// The challenge usually means the platform already sent a code. Give
		// that message a short window first; only request a resend if nothing
		// arrives, so the daily send limit is not spent on every renewal.
		since := time.Now().Add(-15 * time.Second)
		firstCtx, firstCancel := context.WithTimeout(ctx, 30*time.Second)
		code, waitErr := sms.WaitForCode(firstCtx, baseline, since)
		firstCancel()
		if waitErr != nil && ctx.Err() == nil {
			_ = sendSMSCode(ctx, client, phone)
			code, waitErr = sms.WaitForCode(ctx, baseline, since)
		}
		if waitErr != nil {
			return "", ErrSMSTimeout
		}
		if err := verifySMSCode(ctx, client, phone, code); err != nil {
			return "", err
		}
		// The page re-submits the form after a code is accepted; mirror that by
		// validating again so a new one-time pwd is issued.
		pwd, uuid, err = validateAccountPassword(ctx, client, phone, password)
		if err != nil {
			return "", err
		}
		if err := checkNeedAuth(ctx, client, uuid); err != nil {
			return "", err
		}
		needSMS, err = loginOnce(ctx, client, pwd)
		if err != nil {
			return "", err
		}
		if needSMS {
			return "", ErrSMSCodeRejected
		}
	}
	session, err := sessionFromJar(ctx, client, jar, zhidaoCloudBase)
	if err != nil {
		if errors.Is(err, errNoZhidaoSession) {
			return "", ErrLoginIncomplete
		}
		return "", err
	}
	data, err := json.Marshal(session)
	return string(data), err
}

// sessionFromJar extracts the course-center token and cookie snapshot, then
// verifies the identity. Both login flows share this so a token that the
// platform rotated is always persisted with its matching cookie set.
func sessionFromJar(ctx context.Context, client *http.Client, jar *zhidaoJar, base string) (ZhidaoSession, error) {
	session := ZhidaoSession{}
	for _, name := range []string{"jt-cas", "ai-poly", "openapi-token", "cloudapi-token"} {
		for _, origin := range []string{base, zhidaoCenter} {
			u, err := url.Parse(origin)
			if err != nil {
				continue
			}
			for _, cookie := range client.Jar.Cookies(u) {
				if cookie.Name == name && cookie.Value != "" {
					session.Token = cookie.Value
					if decoded, err := url.PathUnescape(cookie.Value); err == nil {
						session.Token = decoded
					}
					break
				}
			}
			if session.Token != "" {
				break
			}
		}
		if session.Token != "" {
			break
		}
	}
	if session.Token == "" {
		return session, errNoZhidaoSession
	}
	if err := validateZhidao(ctx, client, base, &session); err != nil {
		return session, err
	}
	if jar != nil {
		session.Cookies = jar.snapshot()
	}
	return session, nil
}

type accountPasswordResult struct {
	Status int    `json:"status"`
	UUID   string `json:"uuid"`
	Pwd    string `json:"pwd"`
}

func validateAccountPassword(ctx context.Context, client *http.Client, phone, password string) (string, string, error) {
	payload, err := json.Marshal(map[string]string{"account": phone, "password": password, "validate": ""})
	if err != nil {
		return "", "", ErrLoginIncomplete
	}
	secret := base64.StdEncoding.EncodeToString([]byte(jsEncodeURI(string(payload))))
	var result accountPasswordResult
	if err := postZhidaoForm(ctx, client, zhidaoPassport+"/user/validateAccountAndPassword", url.Values{"secretStr": {secret}}, &result); err != nil {
		return "", "", err
	}
	if result.Status != 1 {
		return "", "", passwordStatusError(result.Status)
	}
	if result.Pwd == "" || result.UUID == "" {
		return "", "", ErrLoginIncomplete
	}
	return result.Pwd, result.UUID, nil
}

func passwordStatusError(status int) error {
	switch status {
	case -1:
		return ErrPasswordRejected
	case -4:
		return ErrCaptchaRequired
	case -5:
		return ErrSimplePassword
	case -6, -12:
		return ErrAccountLocked
	case -9, -13:
		return ErrSlideRequired
	default:
		return ErrLoginIncomplete
	}
}

func checkNeedAuth(ctx context.Context, client *http.Client, uuid string) error {
	var result struct {
		RT *struct {
			NeedAuth *int `json:"needAuth"`
		} `json:"rt"`
	}
	endpoint := "https://appcomm-user.zhihuishu.com/app-commserv-user/userInfo/checkNeedAuth"
	if err := postZhidaoForm(ctx, client, endpoint, url.Values{"uuid": {uuid}}, &result); err != nil {
		return err
	}
	if result.RT == nil || result.RT.NeedAuth == nil {
		return ErrLoginIncomplete
	}
	if *result.RT.NeedAuth == 0 {
		return nil
	}
	return ErrNeedBinding
}

// loginOnce performs the one-time-password redirect. It reports whether the
// platform replied with the SMS verification prompt.
func loginOnce(ctx context.Context, client *http.Client, pwd string) (bool, error) {
	service := zhidaoCloudBase + "/oauth/login/ssoLogin?redirect=" + url.QueryEscape(zhidaoCenter)
	endpoint := zhidaoPassport + "/login?pwd=" + url.QueryEscape(pwd) + "&service=" + url.QueryEscape(service)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return false, ErrLoginIncomplete
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 DueRadar/0.1")
	req.Header.Set("Referer", zhidaoPassport+"/login")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, ErrLoginIncomplete
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if hasZhidaoToken(client) {
		return false, nil
	}
	text := string(body)
	switch {
	case isSMSChallenge(text):
		return true, nil
	case strings.Contains(text, "手机号或密码错误"):
		return false, ErrPasswordRejected
	case strings.Contains(text, "lpwdErrorShow") && strings.Contains(text, "value=\"1\""):
		return false, ErrCaptchaRequired
	case strings.Contains(text, "\"needAuth\"") && strings.Contains(text, "滑块"):
		return false, ErrSlideRequired
	}
	if resp.Request != nil && resp.Request.URL.Hostname() == "passport.zhihuishu.com" {
		return false, ErrLoginIncomplete
	}
	return false, ErrLoginIncomplete
}

// isSMSChallenge distinguishes the server-injected call from the function
// definition that is always present in the static login page.
func isSMSChallenge(page string) bool {
	if strings.Contains(page, "showCodeMsgPop(") {
		idx := strings.Index(page, "showCodeMsgPop(")
		rest := strings.TrimSpace(page[idx+len("showCodeMsgPop("):])
		if rest != "" && (rest[0] == '\'' || rest[0] == '"' || (rest[0] >= '0' && rest[0] <= '9')) {
			return true
		}
	}
	marker := "VerificationCode-span"
	for {
		idx := strings.Index(page, marker)
		if idx < 0 {
			return false
		}
		rest := page[idx+len(marker):]
		end := strings.IndexByte(rest, '<')
		if end >= 0 {
			rest = rest[:end]
		}
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ">"))
		if len(rest) >= 4 {
			return true
		}
		page = page[idx+len(marker):]
	}
}

func hasZhidaoToken(client *http.Client) bool {
	for _, origin := range []string{zhidaoCloudBase, zhidaoCenter} {
		u, err := url.Parse(origin)
		if err != nil {
			continue
		}
		for _, cookie := range client.Jar.Cookies(u) {
			switch cookie.Name {
			case "jt-cas", "ai-poly", "openapi-token", "cloudapi-token":
				if cookie.Value != "" {
					return true
				}
			}
		}
	}
	return false
}

func sendSMSCode(ctx context.Context, client *http.Client, phone string) error {
	var result struct {
		Status int `json:"status"`
	}
	if err := postZhidaoForm(ctx, client, zhidaoPassport+"/user/resendCode", url.Values{"phone": {phone}}, &result); err != nil {
		return err
	}
	if result.Status == -2 {
		// Daily send limit. The platform may already have sent the code, so the
		// caller still polls the collector.
		return nil
	}
	return nil
}

func verifySMSCode(ctx context.Context, client *http.Client, phone, code string) error {
	var result struct {
		Status int `json:"status"`
	}
	if err := postZhidaoForm(ctx, client, zhidaoPassport+"/user/checkCode", url.Values{"phone": {phone}, "code": {code}}, &result); err != nil {
		return err
	}
	if result.Status == -1 || result.Status == -2 {
		return ErrSMSCodeRejected
	}
	return nil
}

func postZhidaoForm(ctx context.Context, client *http.Client, endpoint string, values url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return ErrLoginIncomplete
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 DueRadar/0.1")
	req.Header.Set("Referer", zhidaoPassport+"/login")
	req.Header.Set("Origin", zhidaoPassport)
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrLoginIncomplete
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ErrLoginIncomplete
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || json.Unmarshal(data, out) != nil {
		return ErrLoginIncomplete
	}
	return nil
}

// jsEncodeURI implements the browser's encodeURI for a UTF-8 string. The login
// page sends base64(encodeURI(JSON)) as secretStr.
func jsEncodeURI(value string) string {
	const keep = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789;,/?:@&=+$-_.!~*'()#"
	var b strings.Builder
	for _, c := range []byte(value) {
		if strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		const hex = "0123456789ABCDEF"
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}
