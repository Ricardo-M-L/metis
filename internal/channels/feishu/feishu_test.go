package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

func TestFeishu_SignsBodyWhenSecretSet(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, Secret: "S", HTTPClient: srv.Client(),
		nowFunc: func() time.Time { return time.Unix(1700000000, 0) }}
	if err := a.Send(context.Background(), "", channels.Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got["timestamp"] == nil || got["sign"] == nil {
		t.Errorf("expected timestamp + sign in body; got %v", got)
	}
	if got["msg_type"] != "text" {
		t.Errorf("wrong msg_type: %v", got["msg_type"])
	}
}

func TestFeishu_NoSecretSkipsSigning(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, HTTPClient: srv.Client(), nowFunc: time.Now}
	a.Send(context.Background(), "", channels.Message{Text: "hi"})
	if got["timestamp"] != nil || got["sign"] != nil {
		t.Errorf("no secret → no timestamp/sign; got %v", got)
	}
}

func TestFeishu_NonZeroCodeIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"code":1234,"msg":"bad token"}`))
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, HTTPClient: srv.Client(), nowFunc: time.Now}
	if err := a.Send(context.Background(), "", channels.Message{Text: "x"}); err == nil {
		t.Error("non-zero code should be error")
	}
}

func TestFeishuSign_Deterministic(t *testing.T) {
	if feishuSign(1700000000, "secret") != feishuSign(1700000000, "secret") {
		t.Error("feishuSign must be deterministic")
	}
}
