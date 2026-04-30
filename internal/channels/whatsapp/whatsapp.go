// Package whatsapp implements channels.Adapter via a self-hosted
// whatsapp-web.js or wuzapi HTTP bridge — one of the standard
// patterns for getting WhatsApp out of metas business API gating.
// User configures the bridge URL + token; we POST text messages.
//
// Cloud (Meta) WhatsApp Business API also works if the user puts
// graph.facebook.com endpoints in BaseURL — same JSON shape works
// modulo phone number id placement.
package whatsapp

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
	BaseURL    string // e.g. "http://localhost:3000" (whatsapp-web.js bridge)
	Token      string // session/admin token
	From       string // sender phone number (E.164, no +)
	HTTPClient *http.Client
}

func New(baseURL, token, fromPhone string) *Adapter {
	return &Adapter{
		BaseURL:    baseURL,
		Token:      token,
		From:       fromPhone,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *Adapter) Name() string     { return "whatsapp" }
func (a *Adapter) Configured() bool { return a.BaseURL != "" && a.Token != "" }

// Send posts to the bridge's /messages endpoint (whatsapp-web.js
// convention). target is the recipient phone number in E.164 form.
func (a *Adapter) Send(ctx context.Context, target string, msg channels.Message) error {
	text := msg.Text
	if msg.Title != "" {
		text = "*" + msg.Title + "*\n\n" + msg.Text // WhatsApp markdown
	}
	body := map[string]any{
		"from": a.From,
		"to":   target,
		"text": text,
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/messages", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("whatsapp: HTTP %d", resp.StatusCode)
	}
	return nil
}
