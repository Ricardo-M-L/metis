// Package mattermost implements channels.Adapter via Mattermost's
// incoming webhook URL — the simplest integration path, same model
// as Slack/Discord. User creates a webhook in their Mattermost
// channel admin → integrations → incoming webhooks.
package mattermost

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

func (a *Adapter) Name() string     { return "mattermost" }
func (a *Adapter) Configured() bool { return a.WebhookURL != "" }

func (a *Adapter) Send(ctx context.Context, _ string, msg channels.Message) error {
	body := map[string]any{
		"text": msg.Text,
	}
	if msg.Title != "" {
		// Mattermost incoming webhook attachments — fancier than
		// plain text but still supported on the simple webhook path.
		body["attachments"] = []any{
			map[string]any{
				"title": msg.Title,
				"text":  msg.Text,
			},
		}
		delete(body, "text")
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
		return fmt.Errorf("mattermost: HTTP %d", resp.StatusCode)
	}
	return nil
}
