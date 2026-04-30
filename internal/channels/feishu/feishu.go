// Package feishu implements channels.Adapter for 飞书 (Lark) Custom Robot
// webhooks. Signing scheme is similar to dingtalk but: signing string is
// `<timestamp>\n<secret>` and the HMAC key is the secret itself with empty
// payload (the SDK calls hmac.new(key=secret_bytes, msg=string).digest()).
//
// Spec: https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/bot-v2/use-custom-bots-in-a-group
package feishu

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

type Adapter struct {
	WebhookURL string
	Secret     string
	HTTPClient *http.Client
	nowFunc    func() time.Time
}

func New(webhookURL, secret string) *Adapter {
	return &Adapter{
		WebhookURL: webhookURL,
		Secret:     secret,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		nowFunc:    time.Now,
	}
}

func (a *Adapter) Name() string     { return "feishu" }
func (a *Adapter) Configured() bool { return a.WebhookURL != "" }

func (a *Adapter) Send(ctx context.Context, _ string, msg channels.Message) error {
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": msg.Text},
	}
	if a.Secret != "" {
		ts := a.nowFunc().Unix()
		payload["timestamp"] = strconv.FormatInt(ts, 10)
		payload["sign"] = feishuSign(ts, a.Secret)
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
		return fmt.Errorf("feishu: HTTP %d", resp.StatusCode)
	}
	var ack struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	if ack.Code != 0 {
		return fmt.Errorf("feishu: code=%d msg=%q", ack.Code, ack.Msg)
	}
	return nil
}

// feishuSign matches the algorithm from feishu's official docs:
//
//	string_to_sign = f"{timestamp}\n{secret}"
//	sign = base64(hmac_sha256(key=string_to_sign, msg=""))
func feishuSign(ts int64, secret string) string {
	key := strconv.FormatInt(ts, 10) + "\n" + secret
	mac := hmac.New(sha256.New, []byte(key))
	// Note: empty message — feishu uses the signing string as the *key*.
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
