// Package wechat implements channels.Adapter for 企业微信群机器人 webhooks.
//
// Personal-account WeChat (itchat-style) is intentionally not supported —
// it relies on undocumented APIs that Tencent regularly blocks. The work-
// account ("企业微信") group bot is stable and trivial to set up: add a
// bot to a group, copy the webhook URL, POST JSON.
//
// Spec: https://developer.work.weixin.qq.com/document/path/91770
package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

type Adapter struct {
	WebhookURL string
	HTTPClient *http.Client
}

func New(webhookURL string) *Adapter {
	return &Adapter{
		WebhookURL: webhookURL,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *Adapter) Name() string     { return "wechat" }
func (a *Adapter) Configured() bool { return a.WebhookURL != "" }

func (a *Adapter) Send(ctx context.Context, _ string, msg channels.Message) error {
	payload := map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": msg.Text},
	}
	if msg.Markdown {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"content": msg.Text},
		}
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", a.WebhookURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("wechat: HTTP %d", resp.StatusCode)
	}
	var ack struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	if ack.ErrCode != 0 {
		return fmt.Errorf("wechat: errcode=%d errmsg=%q", ack.ErrCode, ack.ErrMsg)
	}
	return nil
}
