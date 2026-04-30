// Package telegram implements channels.Adapter for Telegram bots.
//
// Hermes' adapter retries 429/503 with `2^attempt * base` backoff
// (`tools/send_message_tool.py:_telegram_retry_delay`). We mirror that exactly
// because Telegram is the one platform here that hands out 429s freely.
package telegram

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
	Token         string
	DefaultChatID string
	HTTPClient    *http.Client
	BaseURL       string

	// MaxRetries / BaseBackoff control the exponential backoff. Defaults
	// match Hermes (3 retries, 1s base → 1s, 2s, 4s caps).
	MaxRetries  int
	BaseBackoff time.Duration
}

func New(token, defaultChatID string) *Adapter {
	return &Adapter{
		Token:         token,
		DefaultChatID: defaultChatID,
		HTTPClient:    &http.Client{Timeout: 30 * time.Second},
		BaseURL:       "https://api.telegram.org",
		MaxRetries:    3,
		BaseBackoff:   1 * time.Second,
	}
}

func (a *Adapter) Name() string     { return "telegram" }
func (a *Adapter) Configured() bool { return a.Token != "" }

func (a *Adapter) Send(ctx context.Context, target string, msg channels.Message) error {
	if target == "" {
		target = a.DefaultChatID
	}
	if target == "" {
		return fmt.Errorf("telegram: no chat_id and no default configured")
	}
	body := map[string]any{
		"chat_id": target,
		"text":    msg.Text,
	}
	if msg.Markdown {
		body["parse_mode"] = "MarkdownV2"
	}
	buf, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/bot%s/sendMessage", a.BaseURL, a.Token)

	for attempt := 0; attempt < a.MaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.HTTPClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == 200 {
			resp.Body.Close()
			return nil
		}
		retryable := resp.StatusCode == 429 || resp.StatusCode == 503
		resp.Body.Close()
		if !retryable || attempt == a.MaxRetries-1 {
			return fmt.Errorf("telegram: HTTP %d", resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.BaseBackoff << uint(attempt)):
		}
	}
	return fmt.Errorf("telegram: all retries exhausted")
}
