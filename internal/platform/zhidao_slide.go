package platform

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ZhidaoCaptchaID is the 网易易盾 captcha the 知到 login center mounts on its
// password form. A validate token produced by this captcha is accepted by the
// account API below, so the page only has to be solved once on a real phone.
const ZhidaoCaptchaID = "75f9f716460a422f89a628f50fd8cc2b"

const (
	zhidaoLoginKey = "bdas85990328dfpl"
	zhidaoLoginIV  = "563216db1e1744c7"

	zhidaoAccountLogin = "https://user.zhihuishu.com/zhsuser/login/account"
	zhidaoLoginOrigin  = "https://login.zhihuishu.com"
)

// ErrCaptchaRejected means the platform did not accept the slider token, usually
// because it was solved elsewhere, replayed, or expired.
var ErrCaptchaRejected = errors.New("知到未接受滑块验证结果")

// SlideLogin signs in with the stored phone and password plus a captcha token
// produced by a real browser session. The password is never sent to a client.
func SlideLogin(ctx context.Context, phone, password, validate string, sms SMSCollector) (string, error) {
	client, jar := newZhidaoClient()
	return slideLogin(ctx, client, jar, phone, password, validate, sms)
}

func slideLogin(ctx context.Context, client *http.Client, jar *zhidaoJar, phone, password, validate string, sms SMSCollector) (string, error) {
	var baseline int64
	if sms != nil {
		var err error
		if baseline, err = sms.Baseline(ctx); err != nil {
			return "", ErrSMSUnavailable
		}
	}
	pwd, _, err := accountLogin(ctx, client, phone, password, validate)
	if err != nil {
		return "", err
	}
	// The login center redeems the one-time password through passport, exactly
	// like the QR flow, and the platform may still demand an SMS code here.
	needSMS, err := loginOnce(ctx, client, pwd)
	if err != nil {
		return "", err
	}
	if needSMS {
		if sms == nil {
			return "", ErrSMSTimeout
		}
		since := time.Now().Add(-15 * time.Second)
		code, waitErr := sms.WaitForCode(ctx, baseline, since)
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
			return "", fmt.Errorf("%w: 换票后没有 jt-cas 会话", ErrLoginIncomplete)
		}
		return "", err
	}
	data, err := json.Marshal(session)
	return string(data), err
}

type accountLoginResult struct {
	Status int    `json:"status"`
	Code   int    `json:"code"`
	Msg    string `json:"msg"`
	RT     *struct {
		Pwd     string `json:"pwd"`
		UUID    string `json:"uuid"`
		CacheID string `json:"cacheId"`
	} `json:"rt"`
}

func accountLogin(ctx context.Context, client *http.Client, phone, password, validate string) (string, string, error) {
	payload, err := json.Marshal(map[string]string{
		"account":  phone,
		"password": password,
		"validate": validate,
	})
	if err != nil {
		return "", "", fmt.Errorf("%w: 登录参数序列化失败", ErrLoginIncomplete)
	}
	body := encryptAccountPayload(payload)
	if body == "" {
		return "", "", fmt.Errorf("%w: 登录参数加密失败", ErrLoginIncomplete)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", zhidaoAccountLogin, strings.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("%w: 登录请求创建失败", ErrLoginIncomplete)
	}
	// The form posts the ciphertext as the raw request body, not as a JSON string.
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", "Mozilla/5.0 DueRadar/0.1")
	req.Header.Set("Referer", zhidaoLoginOrigin+"/")
	req.Header.Set("Origin", zhidaoLoginOrigin)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return "", "", fmt.Errorf("%w: 登录接口请求失败: %v", ErrLoginIncomplete, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("%w: 登录接口响应读取失败", ErrLoginIncomplete)
	}
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("%w: 登录接口 HTTP %d", ErrLoginIncomplete, resp.StatusCode)
	}
	var result accountLoginResult
	if json.Unmarshal(data, &result) != nil {
		return "", "", fmt.Errorf("%w: 登录接口返回的不是 JSON（%d 字节）", ErrLoginIncomplete, len(data))
	}
	return loginCredentials(&result)
}

// loginCredentials extracts the one-time password the login center redeems at
// passport. Without it the SSO hop has no session to work with.
func loginCredentials(result *accountLoginResult) (string, string, error) {
	if result.Status != 1 && result.Code != 200 {
		return "", "", accountLoginError(result)
	}
	if result.RT == nil || result.RT.Pwd == "" {
		return "", "", fmt.Errorf("%w: 登录成功但未返回一次性口令", ErrLoginIncomplete)
	}
	return result.RT.Pwd, result.RT.UUID, nil
}

func accountLoginError(result *accountLoginResult) error {
	status := result.Status
	switch status {
	case -1:
		// Parameter error. The account API rejects the payload before it looks at
		// the captcha, so the caller cannot fix this by solving a new one.
		return ErrLoginIncomplete
	case -2:
		return ErrPasswordRejected
	case -4, -10:
		return ErrCaptchaRejected
	case -3, -6, -9, -100:
		return ErrAccountLocked
	case -5:
		return ErrSimplePassword
	default:
		return &accountStatusError{status: status, msg: result.Msg}
	}
}

// accountStatusError keeps the platform's code for the deployment log while
// still reading as an incomplete login to the API caller.
type accountStatusError struct {
	status int
	msg    string
}

func (e *accountStatusError) Error() string {
	if e.msg != "" {
		return fmt.Sprintf("知到账号登录返回未映射状态 %d（%s）", e.status, e.msg)
	}
	return fmt.Sprintf("知到账号登录返回未映射状态 %d", e.status)
}

func (e *accountStatusError) Unwrap() error { return ErrLoginIncomplete }

// encryptAccountPayload mirrors the browser helper: AES-CBC with PKCS#7 over
// the JSON body, base64 encoded.
func encryptAccountPayload(payload []byte) string {
	block, err := aes.NewCipher([]byte(zhidaoLoginKey))
	if err != nil {
		return ""
	}
	padding := aes.BlockSize - len(payload)%aes.BlockSize
	padded := make([]byte, len(payload), len(payload)+padding)
	copy(padded, payload)
	padded = append(padded, bytes.Repeat([]byte{byte(padding)}, padding)...)
	iv := []byte(zhidaoLoginIV)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(padded, padded)
	return base64.StdEncoding.EncodeToString(padded)
}
