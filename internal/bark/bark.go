package bark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func Validate(server, key string) error {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("Bark 服务地址必须是无账号、查询参数或片段的 HTTPS 地址")
	}
	if strings.TrimSpace(key) == "" || len(key) > 512 {
		return errors.New("请填写有效的 Bark Key")
	}
	return nil
}

func Send(ctx context.Context, server, key, title, body string) error {
	if err := Validate(server, key); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"device_key": key, "title": title, "body": body, "group": "DueRadar"})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(server, "/")+"/push", bytes.NewReader(data))
	if err != nil {
		return errors.New("Bark 请求构造失败")
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("Bark 连接失败，请检查网络与服务地址")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("Bark 返回 HTTP %d", response.StatusCode)
	}
	var result struct {
		Code int `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&result); err != nil {
		return errors.New("Bark 返回无效 JSON")
	}
	if result.Code != 200 {
		return fmt.Errorf("Bark 拒绝推送（代码 %d）", result.Code)
	}
	return nil
}
