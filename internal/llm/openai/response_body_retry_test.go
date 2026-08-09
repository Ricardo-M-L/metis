package openai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type truncatedOpenAIResponseBody struct {
	sent bool
}

func (b *truncatedOpenAIResponseBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `{"choices":`), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (*truncatedOpenAIResponseBody) Close() error { return nil }

func TestOpenAICompleteRetriesUnexpectedEOFReadingHTTP200(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: openAIRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					Status: "200 OK", StatusCode: http.StatusOK,
					Header: make(http.Header), Body: &truncatedOpenAIResponseBody{},
				}, nil
			}
			return &http.Response{
				Status: "200 OK", StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)),
			}, nil
		})}
		o := &OpenAI{
			APIKey: "key", BaseURL: "https://token.sensenova.cn/v1",
			Model: "deepseek-v4-flash", MaxTokens: 128, httpClient: client,
		}
		resp, err := o.Complete(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 || len(resp.Content) != 1 || resp.Content[0].Text != "ok" {
			t.Fatalf("calls=%d response=%+v", calls, resp)
		}
	})
}

func TestOpenAIStreamRetriesUnexpectedEOFReadingErrorBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: openAIRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					Status: "503 Service Unavailable", StatusCode: http.StatusServiceUnavailable,
					Header: http.Header{"Retry-After": []string{"1"}}, Body: &truncatedOpenAIResponseBody{},
				}, nil
			}
			return &http.Response{
				Status: "200 OK", StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})}
		o := &OpenAI{
			APIKey: "key", BaseURL: "https://token.sensenova.cn/v1",
			Model: "deepseek-v4-flash", MaxTokens: 128, httpClient: client,
		}
		start := time.Now()
		stream, err := o.Stream(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.Close()
		if calls != 2 {
			t.Fatalf("calls = %d, want retry after truncated error body", calls)
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("elapsed = %v, want truncated 503 to honor Retry-After", elapsed)
		}
	})
}
