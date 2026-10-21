package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// RainLogin uses the same first-party QR endpoints as the platform's web login.
// Platform tokens and cookies never appear in public responses or error strings.
type RainLogin struct {
	client *http.Client
	base   string
	token  string
	Image  []byte
	jar    *rainCookieJar
	once   sync.Once
}

func StartRainLogin(ctx context.Context) (*RainLogin, error) {
	l := &RainLogin{client: &http.Client{Timeout: 185 * time.Second}, base: rainOrigin}
	var response struct {
		Code *int `json:"code"`
		Data struct {
			QR    string `json:"qrContent"`
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := l.request(ctx, "GET", "/api/v3/user/login/app-web-pre-info", nil, &response); err != nil {
		return nil, err
	}
	if response.Code == nil || *response.Code != 0 || response.Data.QR == "" || response.Data.Token == "" {
		return nil, errors.New("雨课堂暂未返回有效二维码")
	}
	l.token = response.Data.Token
	var err error
	l.Image, err = qrcode.Encode(response.Data.QR, qrcode.Medium, 300)
	if err != nil {
		return nil, errors.New("二维码生成失败")
	}
	return l, nil
}

func (l *RainLogin) request(ctx context.Context, method, path string, body any, out any) error {
	if l.base != rainOrigin {
		return errors.New("雨课堂登录请求地址无效")
	}
	if err := l.ensureSession(); err != nil {
		return err
	}
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.base+path, data)
	if err != nil {
		return errors.New("登录请求构造失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", l.base+"/web")
	req.Header.Set("Origin", l.base)
	req.Header.Set("User-Agent", "Mozilla/5.0 DueRadar/0.1")
	for _, cookie := range l.client.Jar.Cookies(req.URL) {
		if cookie.Name == "csrftoken" {
			req.Header.Set("X-CSRFToken", cookie.Value)
			break
		}
	}
	r, err := l.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("雨课堂登录连接失败或二维码已过期")
	}
	defer r.Body.Close()
	if err := l.jar.check(); err != nil {
		return err
	}
	if r.StatusCode != 200 {
		return errors.New("雨课堂登录服务未接受请求")
	}
	const maxBody = 1 << 20
	responseBody, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("雨课堂登录响应读取失败")
	}
	if len(responseBody) > maxBody || json.Unmarshal(responseBody, out) != nil {
		return errors.New("雨课堂登录响应格式发生变化")
	}
	return nil
}

func (l *RainLogin) Wait(ctx context.Context) (string, error) {
	var response struct {
		Code *int                       `json:"code"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := l.request(ctx, "POST", "/api/v3/user/login/app-web-login", map[string]string{"token": l.token}, &response); err != nil {
		return "", err
	}
	if response.Code == nil || *response.Code != 0 {
		return "", errors.New("二维码已过期或登录未确认，请重新扫码")
	}
	id, auth := response.Data["UserID"], response.Data["Auth"]
	if len(id) == 0 {
		id = response.Data["user_id"]
	}
	if len(auth) == 0 {
		auth = response.Data["auth"]
	}
	if len(id) > 0 && len(auth) > 0 {
		var confirmed struct {
			Success bool `json:"success"`
		}
		if err := l.request(ctx, "POST", "/pc/web_login", map[string]json.RawMessage{"UserID": id, "Auth": auth}, &confirmed); err != nil {
			return "", err
		}
		if !confirmed.Success {
			return "", errors.New("雨课堂未确认网页登录会话")
		}
	}
	session, err := l.jar.snapshot()
	if err != nil {
		return "", err
	}
	if len(session.Cookies) == 0 {
		return "", errors.New("登录响应未建立可复用会话")
	}
	return encodeRainSession(session)
}
