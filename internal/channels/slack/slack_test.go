package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

func TestSlack_Configured(t *testing.T) {
	if (&Adapter{}).Configured() {
		t.Error("empty adapter should not be configured")
	}
	if !New("xoxb-x", "#x").Configured() {
		t.Error("token-bearing adapter should be configured")
	}
}

func TestSlack_Send_AuthAndBody(t *testing.T) {
	var got struct {
		auth string
		body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	a := &Adapter{Token: "xoxb-test", DefaultChannel: "#default", HTTPClient: srv.Client(), BaseURL: srv.URL}
	if err := a.Send(context.Background(), "#general", channels.Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.auth, "Bearer ") {
		t.Errorf("missing bearer auth: %q", got.auth)
	}
	if got.body["channel"] != "#general" {
		t.Errorf("channel mismatch: %v", got.body["channel"])
	}
	if got.body["text"] != "hi" {
		t.Errorf("text mismatch: %v", got.body["text"])
	}
}

func TestSlack_SendUsesDefaultChannelWhenTargetEmpty(t *testing.T) {
	var gotChannel any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		gotChannel = m["channel"]
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	a := &Adapter{Token: "xoxb", DefaultChannel: "#fallback", HTTPClient: srv.Client(), BaseURL: srv.URL}
	if err := a.Send(context.Background(), "", channels.Message{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if gotChannel != "#fallback" {
		t.Errorf("expected default channel; got %v", gotChannel)
	}
}

func TestSlack_SendErrorOnNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}))
	defer srv.Close()

	a := &Adapter{Token: "xoxb", HTTPClient: srv.Client(), BaseURL: srv.URL}
	if err := a.Send(context.Background(), "#x", channels.Message{Text: "x"}); err == nil {
		t.Error("expected error when ok=false")
	}
}
