package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func assertChatCredentialRedacted(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if strings.Contains(current.Error(), secret) {
			t.Fatalf("error chain leaked the exact request credential: %s", current)
		}
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("redacted error has no marker: %s", err)
	}
}

func TestChatCompletionsRedactsExactAPIKeyFromHTTPFailures(t *testing.T) {
	const apiKey = "opaque-chat-key-17"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"echo `+apiKey+` and Authorization: Bearer `+apiKey+`"}}`)
	}))
	defer server.Close()

	for _, tc := range []struct {
		name string
		call func(*OpenAI) error
	}{
		{
			name: "complete",
			call: func(client *OpenAI) error {
				_, err := client.Complete(context.Background(), provider.Request{})
				return err
			},
		},
		{
			name: "stream",
			call: func(client *OpenAI) error {
				_, err := client.Stream(context.Background(), provider.Request{Stream: true})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := New(apiKey, server.URL, "gpt-test", 64, time.Second, 0)
			assertChatCredentialRedacted(t, tc.call(client), apiKey)
		})
	}
}

type chatErrorRoundTripper func(*http.Request) (*http.Response, error)

func (fn chatErrorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type chatFaultBody struct {
	readErr  error
	closeErr error
}

func (b *chatFaultBody) Read([]byte) (int, error) { return 0, b.readErr }
func (b *chatFaultBody) Close() error             { return b.closeErr }

func TestChatCompletionsRedactsExactAPIKeyFromStreamIOErrors(t *testing.T) {
	const apiKey = "opaque-chat-key-io-23"
	newClient := func(body io.ReadCloser) *OpenAI {
		client := New(apiKey, "https://example.invalid/v1", "gpt-test", 64, time.Second, 0)
		client.httpClient = &http.Client{Transport: chatErrorRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		})}
		return client
	}

	t.Run("recv", func(t *testing.T) {
		stream, err := newClient(&chatFaultBody{readErr: errors.Join(io.ErrUnexpectedEOF, errors.New("read echoed "+apiKey))}).Stream(context.Background(), provider.Request{Stream: true})
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		_, err = stream.Recv()
		assertChatCredentialRedacted(t, err, apiKey)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("redaction lost unexpected EOF classification: %v", err)
		}
	})

	t.Run("close", func(t *testing.T) {
		stream, err := newClient(&chatFaultBody{readErr: io.EOF, closeErr: errors.New("close echoed " + apiKey)}).Stream(context.Background(), provider.Request{Stream: true})
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		assertChatCredentialRedacted(t, stream.Close(), apiKey)
	})
}
