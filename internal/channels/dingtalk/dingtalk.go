// Package dingtalk implements channels.Adapter for 钉钉 Custom Robot webhooks.
//
// Authentication: HMAC-SHA256 signature appended as query params.
// Spec: https://open.dingtalk.com/document/robots/custom-robot-access
package dingtalk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

type Adapter struct {
	WebhookURL string
	Secret     string
	HTTPClient *http.Client
	// nowFunc is overridable for tests so we can pin the timestamp.
	nowFunc func() time.Time
}

func New(webhookURL, secret string) *Adapter {
	return &Adapter{
		WebhookURL: webhookURL,
		Secret:     secret,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		nowFunc:    time.Now,
	}
}

func (a *Adapter) Name() string     { return "dingtalk" }
func (a *Adapter) Configured() bool { return a.WebhookURL != "" }

// Send posts msg to the dingtalk webhook. target is ignored — like Discord,
// the webhook URL itself encodes the destination.
func (a *Adapter) Send(ctx context.Context, _ string, msg channels.Message) error {
	endpoint := a.WebhookURL
	if a.Secret != "" {
		endpoint = signedURL(endpoint, a.Secret, a.nowFunc())
	}
	payload := map[string]any{"msgtype": "text", "text": map[string]string{"content": msg.Text}}
	if msg.Markdown {
		title := msg.Title
		if title == "" {
			title = "metis"
		}
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"title": title, "text": msg.Text},
		}
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(buf))
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
		return fmt.Errorf("dingtalk: HTTP %d", resp.StatusCode)
	}
	return nil
}

// signedURL produces the dingtalk-required `&timestamp=...&sign=...` query
// suffix. The signing string is `<timestamp>\n<secret>`, HMAC-SHA256 with
// the secret, base64'd, then percent-encoded.
func signedURL(base, secret string, now time.Time) string {
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	stringToSign := ts + "\n" + secret
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "timestamp=" + ts + "&sign=" + url.QueryEscape(sign)
}
