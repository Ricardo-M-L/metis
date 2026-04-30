package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

func TestDiscord_PlainText(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(204)
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, HTTPClient: srv.Client()}
	if err := a.Send(context.Background(), "", channels.Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if gotBody["content"] != "hi" {
		t.Errorf("content = %v", gotBody["content"])
	}
	if _, hasEmbeds := gotBody["embeds"]; hasEmbeds {
		t.Error("plain text should not produce embeds")
	}
}

func TestDiscord_WithTitleProducesEmbed(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(204)
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, HTTPClient: srv.Client()}
	if err := a.Send(context.Background(), "", channels.Message{Text: "body", Title: "alert"}); err != nil {
		t.Fatal(err)
	}
	emb, ok := gotBody["embeds"].([]any)
	if !ok || len(emb) != 1 {
		t.Fatalf("expected embeds slice, got %v", gotBody)
	}
}

func TestDiscord_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
	}))
	defer srv.Close()
	a := &Adapter{WebhookURL: srv.URL, HTTPClient: srv.Client()}
	if err := a.Send(context.Background(), "", channels.Message{Text: "x"}); err == nil {
		t.Error("400 should produce error")
	}
}
