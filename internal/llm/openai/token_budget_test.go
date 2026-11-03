package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func budgetRequestBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode request: %v", err)
	}
	return body
}

func budgetRequestField(body map[string]any) (string, int) {
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		if value, ok := body[field].(float64); ok {
			if _, both := body[map[string]string{"max_tokens": "max_completion_tokens", "max_completion_tokens": "max_tokens"}[field]]; both {
				return "both", int(value)
			}
			return field, int(value)
		}
	}
	return "", 0
}

func writeBudgetSuccess(w http.ResponseWriter, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		return
	}
	fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
}

func writeBudgetRejection(w http.ResponseWriter, current, replacement string) {
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("Unsupported parameter: '%s' is not supported with this model. Use '%s' instead.", current, replacement),
		"type":    "invalid_request_error", "param": current, "code": "unsupported_parameter",
	}})
}

func callBudgetProvider(ctx context.Context, p *OpenAI, stream bool, req Request) error {
	if !stream {
		_, err := p.Complete(ctx, req)
		return err
	}
	s, err := p.Stream(ctx, req)
	if err != nil {
		return err
	}
	defer s.Close()
	for {
		_, err := s.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func TestTokenBudgetDefaultsAndRequestOverride(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ model, field string }{
			{"gpt-6.1-sol", "max_completion_tokens"}, {"gpt-5", "max_completion_tokens"},
			{"gpt-5-mini", "max_completion_tokens"}, {"o1", "max_completion_tokens"},
			{"o3-mini", "max_completion_tokens"}, {"o4-mini-2025-04-16", "max_completion_tokens"},
			{"deepseek-v4-pro", "max_tokens"}, {"kimi-k3", "max_tokens"},
			{"gpt-4o", "max_tokens"}, {"my-gpt-6.1-sol", "max_tokens"},
			{"vendor/gpt-6.1-sol", "max_tokens"}, {"gpt-50-local", "max_tokens"},
			{"o3local", "max_tokens"}, {"gpt-6.preview", "max_tokens"},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.model, stream), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body := budgetRequestBody(t, r)
					field, amount := budgetRequestField(body)
					wantAmount := 8192
					if calls > 0 {
						wantAmount = 256
					}
					if field != tc.field || amount != wantAmount {
						t.Errorf("budget=(%s,%d), want (%s,%d)", field, amount, tc.field, wantAmount)
					}
					calls++
					writeBudgetSuccess(w, stream)
				}))
				defer s.Close()
				p := New("test-key", s.URL, tc.model, 8192, time.Second, 0)
				for _, req := range []Request{{}, {MaxTokens: 256}} {
					if err := callBudgetProvider(context.Background(), p, stream, req); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestTokenBudgetCompatibilityAndToolContinuation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, model := range []string{"private-alias", "gpt-6.1-sol"} {
			t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
				initial, alternate := "max_tokens", "max_completion_tokens"
				if model == "gpt-6.1-sol" {
					initial, alternate = alternate, initial
				}
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body := budgetRequestBody(t, r)
					field, amount := budgetRequestField(body)
					want := alternate
					if calls == 0 {
						want = initial
					}
					if field != want || amount != 640 {
						t.Errorf("call %d budget=(%s,%d), want (%s,640)", calls, field, amount, want)
					}
					calls++
					if calls == 1 {
						writeBudgetRejection(w, initial, alternate)
						return
					}
					if calls == 3 && len(body["messages"].([]any)) != 2 {
						t.Error("tool continuation lost message history")
					}
					writeBudgetSuccess(w, stream)
				}))
				defer s.Close()
				p := New("test-key", s.URL, model, 8192, time.Second, 0)
				req := Request{MaxTokens: 640}
				if err := callBudgetProvider(context.Background(), p, stream, req); err != nil {
					t.Fatal(err)
				}
				req.Messages = []Message{
					{Role: RoleAssistant, Content: []ContentBlock{{Type: "tool_use", ToolUseID: "read-1", ToolName: "Read", ToolInput: map[string]any{"path": "TASK.md"}}}},
					{Role: RoleUser, Content: []ContentBlock{{Type: "tool_result", ToolUseID: "read-1", ToolResult: "fixture"}}},
				}
				if err := callBudgetProvider(context.Background(), p, stream, req); err != nil {
					t.Fatal(err)
				}
				if calls != 3 {
					t.Fatalf("calls=%d, want one compatibility retry plus direct continuation", calls)
				}
			})
		}
	}
}

func TestTokenBudgetGenericErrorsDoNotNegotiate(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, body string
			status     int
		}{
			{"generic", `{"error":{"message":"bad request"}}`, 400},
			{"auth", `{"error":{"code":"unsupported_parameter","param":"max_tokens","message":"Use 'max_completion_tokens' instead."}}`, 401},
			{"different_parameter", `{"error":{"code":"unsupported_parameter","param":"temperature","message":"Use 'max_completion_tokens' instead."}}`, 400},
			{"wrong_code", `{"error":{"code":"invalid_request_error","param":"max_tokens","message":"Use 'max_completion_tokens' instead."}}`, 400},
			{"no_replacement", `{"error":{"code":"unsupported_parameter","param":"max_tokens","message":"Unsupported parameter: max_tokens"}}`, 400},
			{"malformed", `not JSON: Use 'max_completion_tokens' instead.`, 400},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}))
				defer s.Close()
				p := New("test-key", s.URL, "private-alias", 1024, time.Second, 0)
				if err := callBudgetProvider(context.Background(), p, stream, Request{}); err == nil || calls != 1 {
					t.Fatalf("err=%v calls=%d, want one failed request", err, calls)
				}
			})
		}
	}
}

func TestTokenBudgetCompatibilityIsBoundedAndFailureNotRemembered(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				field, _ := budgetRequestField(budgetRequestBody(t, r))
				want := "max_completion_tokens"
				if calls%2 == 1 {
					want = "max_tokens"
				}
				if field != want {
					t.Errorf("call %d field=%s want %s", calls, field, want)
				}
				calls++
				other := "max_tokens"
				if field == other {
					other = "max_completion_tokens"
				}
				writeBudgetRejection(w, field, other)
			}))
			defer s.Close()
			p := New("test-key", s.URL, "gpt-6.1-sol", 1024, time.Second, 0)
			for i := 0; i < 2; i++ {
				if err := callBudgetProvider(context.Background(), p, stream, Request{}); err == nil {
					t.Fatal("both fields rejected but request succeeded")
				}
			}
			if calls != 4 {
				t.Fatalf("calls=%d want exactly two attempts per logical call", calls)
			}
		})
	}
}

func TestTokenBudgetCancellationStopsCompatibilityRetry(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			p := New("test-key", "https://fixture.invalid", "private-alias", 1024, time.Second, 0)
			p.httpClient = &http.Client{Transport: openAIRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				cancel()
				return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unsupported_parameter","param":"max_tokens","message":"Use 'max_completion_tokens' instead."}}`))}, nil
			})}
			err := callBudgetProvider(ctx, p, stream, Request{})
			if !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("err=%v calls=%d want canceled after first attempt", err, calls)
			}
		})
	}
}

func TestTokenBudgetLearnedRouteIsScopedAndConcurrent(t *testing.T) {
	var calls atomic.Int32
	var wrong atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := budgetRequestBody(t, r)
		field, _ := budgetRequestField(body)
		calls.Add(1)
		if field == "max_tokens" {
			writeBudgetRejection(w, field, "max_completion_tokens")
			return
		}
		if field != "max_completion_tokens" {
			wrong.Add(1)
		}
		writeBudgetSuccess(w, body["stream"] == true)
	}))
	defer s.Close()
	p := New("test-key", s.URL, "private-alias", 1024, 3*time.Second, 0)
	if err := callBudgetProvider(context.Background(), p, false, Request{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			if err := callBudgetProvider(context.Background(), p, true, Request{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 18 || wrong.Load() != 0 {
		t.Fatalf("calls=%d wrong=%d", calls.Load(), wrong.Load())
	}
	// A distinct model, endpoint and provider instance must not inherit the
	// capability learned for the original route.
	var isolatedCalls atomic.Int32
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := budgetRequestBody(t, r)
		field, _ := budgetRequestField(body)
		if field != "max_tokens" {
			t.Errorf("learned field leaked to another route: %s", field)
		}
		isolatedCalls.Add(1)
		writeBudgetSuccess(w, false)
	}))
	defer s2.Close()
	p.BaseURL = s2.URL
	if err := callBudgetProvider(context.Background(), p, false, Request{}); err != nil {
		t.Fatal(err)
	}
	p.BaseURL = s.URL
	p.Model = "another-private-alias"
	// This original endpoint rejects max_tokens. Two calls prove the new
	// model started from its own default rather than the prior learned field.
	before := calls.Load()
	if err := callBudgetProvider(context.Background(), p, false, Request{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load()-before != 2 || isolatedCalls.Load() != 1 {
		t.Fatalf("model negotiation calls=%d, endpoint calls=%d", calls.Load()-before, isolatedCalls.Load())
	}
	other := New("test-key", s.URL, "private-alias", 1024, time.Second, 0)
	before = calls.Load()
	if err := callBudgetProvider(context.Background(), other, false, Request{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load()-before != 2 {
		t.Fatal("learned field leaked between provider instances")
	}
}

func TestTokenBudgetCompatibilityPreservesOverflowRecovery(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, compatibilityFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/compatibilityFirst=%t", stream, compatibilityFirst), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					field, amount := budgetRequestField(budgetRequestBody(t, r))
					calls++
					wantAmount := 8000
					if calls == 3 || (calls == 2 && !compatibilityFirst) {
						wantAmount = 2000
					}
					if amount != wantAmount {
						t.Errorf("call %d amount=%d want %d", calls, amount, wantAmount)
					}
					if (calls == 1 && compatibilityFirst) || (calls == 2 && !compatibilityFirst) {
						writeBudgetRejection(w, field, "max_completion_tokens")
						return
					}
					if calls < 3 {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 10000 tokens. However, you requested 15000 tokens (7000 in the messages, 8000 in the completion)."}}`)
						return
					}
					if field != "max_completion_tokens" {
						t.Errorf("negotiated field lost on overflow retry: %s", field)
					}
					writeBudgetSuccess(w, stream)
				}))
				defer s.Close()
				p := New("test-key", s.URL, "private-alias", 12000, time.Second, 0)
				if err := callBudgetProvider(context.Background(), p, stream, Request{MaxTokens: 8000}); err != nil {
					t.Fatal(err)
				}
				if calls != 3 {
					t.Fatalf("calls=%d want 3", calls)
				}
			})
		}
	}
}
