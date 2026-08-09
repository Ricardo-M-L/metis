package builtin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
)

type webFetchRoundTripFunc func(*http.Request) (*http.Response, error)

func (f webFetchRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type unexpectedEOFBody struct {
	sent bool
}

func (b *unexpectedEOFBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial"), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (*unexpectedEOFBody) Close() error { return nil }

func webFetchOK(body io.ReadCloser) *http.Response {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       body,
	}
}

func TestWebFetchRetriesEOFBeforeHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: webFetchRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, io.EOF
			}
			return webFetchOK(io.NopCloser(strings.NewReader("recovered"))), nil
		})}
		wf := WebFetch{gate: bypassGate(), http: client}
		res, err := wf.Execute(context.Background(), map[string]any{"url": "https://raw.githubusercontent.com/a/b/main/x"})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 || !strings.Contains(res.Output, "recovered") {
			t.Fatalf("calls=%d output=%q", calls, res.Output)
		}
	})
}

func TestWebFetchRetriesUnexpectedEOFWhileReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: webFetchRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return webFetchOK(&unexpectedEOFBody{}), nil
			}
			return webFetchOK(io.NopCloser(strings.NewReader("complete"))), nil
		})}
		wf := WebFetch{gate: bypassGate(), http: client}
		res, err := wf.Execute(context.Background(), map[string]any{"url": "https://github.com/a/b"})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 || !strings.Contains(res.Output, "complete") || strings.Contains(res.Output, "partial") {
			t.Fatalf("calls=%d output=%q", calls, res.Output)
		}
	})
}

func TestWebFetchPersistentEOFFailsAfterBoundedAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: webFetchRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, io.EOF
		})}
		wf := WebFetch{gate: bypassGate(), http: client}
		res, err := wf.Execute(context.Background(), map[string]any{"url": "https://raw.githubusercontent.com/a/b/main/x"})
		if err == nil {
			t.Fatalf("persistent EOF was hidden as result %+v", res)
		}
		if !errors.Is(err, io.EOF) {
			t.Fatalf("final err = %v, want original EOF", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want bounded 3 attempts", calls)
		}
	})
}

func TestWebFetchPersistent503KeepsFinalResponseVisible(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: webFetchRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				Status:     "503 Service Unavailable",
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("upstream overloaded")),
			}, nil
		})}
		wf := WebFetch{gate: bypassGate(), http: client}
		res, err := wf.Execute(context.Background(), map[string]any{"url": "https://github.com/a/b"})
		if err != nil {
			t.Fatalf("final HTTP response should be a visible tool result: %v", err)
		}
		if calls != 3 || res == nil || !res.IsError ||
			!strings.Contains(res.Output, "503 Service Unavailable") ||
			!strings.Contains(res.Output, "upstream overloaded") {
			t.Fatalf("calls=%d result=%+v", calls, res)
		}
	})
}
