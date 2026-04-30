// Package signal implements channels.Adapter via signal-cli's REST
// daemon (https://github.com/AsamK/signal-cli) — `signal-cli daemon
// --http`. We hit /v1/send so users can wire metis to forward agent
// notifications to their Signal account without writing Python glue.
//
// Why not direct libsignal: the official lib is GPL + heavy native
// deps. signal-cli is the de-facto bridge most users have running
// already (or can apt-install).
package signal

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
	BaseURL    string // e.g. "http://localhost:8080"
	Number     string // sender number registered in signal-cli
	HTTPClient *http.Client
}

func New(baseURL, fromNumber string) *Adapter {
	return &Adapter{
		BaseURL:    baseURL,
		Number:     fromNumber,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *Adapter) Name() string     { return "signal" }
func (a *Adapter) Configured() bool { return a.BaseURL != "" && a.Number != "" }

// Send posts message via signal-cli's /v1/send endpoint. target is
// the Signal recipient number (or group id starting with "group.").
func (a *Adapter) Send(ctx context.Context, target string, msg channels.Message) error {
	body := map[string]any{
		"message":    msg.Text,
		"number":     a.Number,
		"recipients": []string{target},
	}
	if msg.Title != "" {
		body["message"] = msg.Title + "\n\n" + msg.Text
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/v1/send", bytes.NewReader(buf))
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
		return fmt.Errorf("signal: HTTP %d", resp.StatusCode)
	}
	return nil
}
