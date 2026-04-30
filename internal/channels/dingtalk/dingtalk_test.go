package dingtalk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

func TestDingtalk_SignedURLContainsTimestampAndSign(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, Secret: "S3CR3T", HTTPClient: srv.Client(), nowFunc: func() time.Time {
		return time.Unix(1700000000, 0)
	}}
	if err := a.Send(context.Background(), "", channels.Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(gotURL)
	q := u.Query()
	if q.Get("timestamp") == "" {
		t.Error("missing timestamp in signed URL")
	}
	if q.Get("sign") == "" {
		t.Error("missing sign in signed URL")
	}
}

func TestDingtalk_NoSecretSkipsSigning(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := &Adapter{WebhookURL: srv.URL, HTTPClient: srv.Client(), nowFunc: time.Now}
	if err := a.Send(context.Background(), "", channels.Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotURL, "sign=") {
		t.Errorf("URL should not contain sign when secret is empty: %s", gotURL)
	}
}

func TestSignedURL_Deterministic(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := signedURL("https://x.example/foo", "secret", now)
	b := signedURL("https://x.example/foo", "secret", now)
	if a != b {
		t.Error("signed URL should be deterministic for same input")
	}
}

func TestSignedURL_AppendsToExistingQuery(t *testing.T) {
	got := signedURL("https://x.example/foo?access_token=abc", "secret", time.Unix(1700000000, 0))
	if !strings.Contains(got, "?access_token=abc&timestamp=") {
		t.Errorf("should append with & when query already exists; got %s", got)
	}
}
