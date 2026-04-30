package wechat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

func TestWechat_PlainText(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, HTTPClient: srv.Client()}
	if err := a.Send(context.Background(), "", channels.Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got["msgtype"] != "text" {
		t.Errorf("msgtype = %v", got["msgtype"])
	}
	text, _ := got["text"].(map[string]any)
	if text["content"] != "hi" {
		t.Errorf("content = %v", text)
	}
}

func TestWechat_MarkdownPath(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, HTTPClient: srv.Client()}
	a.Send(context.Background(), "", channels.Message{Text: "**bold**", Markdown: true})
	if got["msgtype"] != "markdown" {
		t.Errorf("expected markdown msgtype; got %v", got["msgtype"])
	}
}

func TestWechat_NonZeroErrcode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"errcode":40001,"errmsg":"invalid token"}`))
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, HTTPClient: srv.Client()}
	if err := a.Send(context.Background(), "", channels.Message{Text: "x"}); err == nil {
		t.Error("non-zero errcode should be error")
	}
}
