// Package slack implements the channels.Adapter for Slack via chat.postMessage.
//
// Bot token authenticates as `Authorization: Bearer xoxb-...`. Send accepts
// targets like "#general" or user IDs ("U12345"). If target is empty we fall
// back to DefaultChannel from config.
package slack

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
	Token          string
	DefaultChannel string
	HTTPClient     *http.Client
	BaseURL        string // override for tests
}

func New(token, defaultChannel string) *Adapter {
	return &Adapter{
		Token:          token,
		DefaultChannel: defaultChannel,
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
		BaseURL:        "https://slack.com/api",
	}
}

func (a *Adapter) Name() string       { return "slack" }
func (a *Adapter) Configured() bool   { return a.Token != "" }

func (a *Adapter) Send(ctx context.Context, target string, msg channels.Message) error {
	if target == "" {
		target = a.DefaultChannel
	}
	if target == "" {
		return fmt.Errorf("slack: no target channel and no default configured")
	}
	body := map[string]any{
		"channel": target,
		"text":    msg.Text,
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/chat.postMessage", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var ack struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	if resp.StatusCode != 200 || !ack.OK {
		return fmt.Errorf("slack: %d ok=%v err=%q", resp.StatusCode, ack.OK, ack.Error)
	}
	return nil
}
