// Package discord implements channels.Adapter via Discord webhook URL.
// We deliberately skip the bot/OAuth path — webhooks are the dramatically
// simpler integration and cover most "agent posts to channel" flows.
package discord

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

func (a *Adapter) Name() string     { return "discord" }
func (a *Adapter) Configured() bool { return a.WebhookURL != "" }

// Send POSTs to the configured webhook. The `target` argument is intentionally
// ignored — a Discord webhook is bound to a specific channel at creation
// time, and rerouting requires a different webhook URL anyway.
func (a *Adapter) Send(ctx context.Context, _ string, msg channels.Message) error {
	body := map[string]any{"content": msg.Text}
	if msg.Title != "" {
		body["embeds"] = []any{map[string]any{"title": msg.Title, "description": msg.Text}}
		delete(body, "content")
	}
	buf, _ := json.Marshal(body)
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
		return fmt.Errorf("discord: HTTP %d", resp.StatusCode)
	}
	return nil
}
