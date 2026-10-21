package platform

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

const zhidaoCloud = "https://cloudapi.polymas.com"
const zhidaoCenter = "https://hike-teaching-center.polymas.com/stu-hike/agent-course-hike/ai-course-center"

// ZhidaoSession is stored only inside the encrypted vault, never in API output.
type ZhidaoSession struct {
	Token     string         `json:"token"`
	UserNID   string         `json:"user_nid"`
	SchoolNID string         `json:"school_nid"`
	ZhsUID    string         `json:"zhs_uid"`
	ZhsUUID   string         `json:"zhs_uuid"`
	Cookies   []zhidaoCookie `json:"cookies,omitempty"`
}
type zhidaoCookie struct {
	URL    string       `json:"url"`
	Cookie *http.Cookie `json:"cookie"`
}
type zhidaoJar struct {
	http.CookieJar
	mu      sync.Mutex
	records []zhidaoCookie
}

func (j *zhidaoJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.CookieJar.SetCookies(u, cookies)
	for _, c := range cookies {
		clone := *c
		j.records = append(j.records, zhidaoCookie{u.String(), &clone})
	}
}
func newZhidaoClient() (*http.Client, *zhidaoJar) {
	jar, _ := cookiejar.New(nil)
	j := &zhidaoJar{CookieJar: jar}
	return &http.Client{Jar: j, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		h := req.URL.Hostname()
		if len(via) > 12 || req.URL.Scheme != "https" || !(h == "zhihuishu.com" || strings.HasSuffix(h, ".zhihuishu.com") || h == "polymas.com" || strings.HasSuffix(h, ".polymas.com")) {
			return errors.New("知到登录跳转地址无效")
		}
		req.Header.Del("Authorization")
		return nil
	}}, j
}

type ZhidaoLogin struct {
	client       *http.Client
	jar          *zhidaoJar
	token        string
	Image        []byte
	userBase     string
	passportBase string
	cloudBase    string
	pollInterval time.Duration
}

func StartZhidaoLogin(ctx context.Context) (*ZhidaoLogin, error) {
	c, j := newZhidaoClient()
	l := &ZhidaoLogin{client: c, jar: j, userBase: "https://user.zhihuishu.com", passportBase: "https://passport.zhihuishu.com", cloudBase: zhidaoCloud, pollInterval: 3 * time.Second}
	var result struct {
		Img   string `json:"img"`
		Token string `json:"qrToken"`
	}
	if err := zhidaoRequest(ctx, c, "GET", l.userBase+"/zhsuser/qrCodeLogin/getLoginQrImg", "", nil, &result); err != nil {
		return nil, err
	}
	img, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(result.Img, "data:image/png;base64,"))
	if err != nil || len(img) < 8 || !bytes.Equal(img[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) || result.Token == "" {
		return nil, errors.New("知到暂未返回有效二维码")
	}
	l.Image = img
	l.token = result.Token
	return l, nil
}
func (l *ZhidaoLogin) Wait(ctx context.Context) (string, error) {
	for {
		var result struct {
			Status       int    `json:"status"`
			OncePassword string `json:"oncePassword"`
		}
		err := zhidaoRequest(ctx, l.client, "GET", l.userBase+"/zhsuser/qrCodeLogin/getLoginQrInfo?qrToken="+url.QueryEscape(l.token), "", nil, &result)
		if err != nil {
			return "", err
		}
		if result.Status == 2 {
			return "", errors.New("知到二维码已过期，请重新获取")
		}
		if result.Status == 1 {
			if result.OncePassword == "" {
				return "", errors.New("知到扫码确认信息不完整")
			}
			return l.complete(ctx, result.OncePassword)
		}
		timer := time.NewTimer(l.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", errors.New("知到扫码已取消或过期")
		case <-timer.C:
		}
	}
}
func (l *ZhidaoLogin) complete(ctx context.Context, once string) (string, error) {
	service := l.cloudBase + "/oauth/login/ssoLogin?redirect=" + url.QueryEscape(zhidaoCenter)
	endpoint := l.passportBase + "/login?pwd=" + url.QueryEscape(once) + "&service=" + url.QueryEscape(service)
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 DueRadar/0.1")
	resp, err := l.client.Do(req)
	if err != nil {
		return "", errors.New("知到登录确认连接失败")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New("知到登录确认失败")
	}
	session := ZhidaoSession{}
	for _, name := range []string{"jt-cas", "ai-poly", "openapi-token", "cloudapi-token"} {
		for _, origin := range []string{l.cloudBase, zhidaoCenter} {
			u, _ := url.Parse(origin)
			for _, c := range l.client.Jar.Cookies(u) {
				if c.Name == name && c.Value != "" {
					session.Token = c.Value
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
		return "", errors.New("知到扫码已确认，但课程中心会话尚未建立")
	}
	if decoded, err := url.PathUnescape(session.Token); err == nil {
		session.Token = decoded
	}
	if err := validateZhidao(ctx, l.client, l.cloudBase, &session); err != nil {
		return "", err
	}
	l.jar.mu.Lock()
	session.Cookies = append([]zhidaoCookie(nil), l.jar.records...)
	l.jar.mu.Unlock()
	data, err := json.Marshal(session)
	return string(data), err
}
func zhidaoRequest(ctx context.Context, client *http.Client, method, endpoint, token string, body any, out any) error {
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
		return errors.New("知到请求构造失败")
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", "Mozilla/5.0 DueRadar/0.1")
	req.Header.Set("Referer", "https://hike-teaching-center.polymas.com/")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("知到连接失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ErrSessionExpired
	}
	if resp.StatusCode != 200 {
		return errors.New("知到服务暂未接受请求")
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return errors.New("知到返回的数据格式无效")
	}
	return nil
}
func validateZhidao(ctx context.Context, c *http.Client, base string, s *ZhidaoSession) error {
	var result struct {
		Code int `json:"code"`
		Data struct {
			User    string `json:"userNid"`
			School  string `json:"schoolCertificationNid"`
			ZhsUID  string `json:"zhsUid"`
			ZhsUUID string `json:"zhsUuid"`
		} `json:"data"`
	}
	if err := zhidaoRequest(ctx, c, "POST", base+"/user/user/v2/get-current-user-detail", s.Token, map[string]any{}, &result); err != nil {
		return err
	}
	if result.Code == 401 || result.Code == 403 {
		return ErrSessionExpired
	}
	if result.Code != 200 || result.Data.User == "" {
		return errors.New("知到账户身份验证失败")
	}
	s.UserNID = result.Data.User
	s.SchoolNID = result.Data.School
	s.ZhsUID = result.Data.ZhsUID
	s.ZhsUUID = result.Data.ZhsUUID
	return nil
}
