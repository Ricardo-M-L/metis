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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func chatTerminalToolFrame(id, args string, index int) string {
	frame := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": index, "id": id, "type": "function", "function": map[string]any{
			"name": "RecoveryCount", "arguments": args,
		}}},
	}}}}
	encoded, _ := json.Marshal(frame)
	return "data: " + string(encoded) + "\n\n"
}

const chatTerminalFinish = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
const chatTerminalText = "data: {\"choices\":[{\"delta\":{\"content\":\"Finished.\"},\"finish_reason\":\"stop\"}]}\n\n"

func TestOpenAIStream_UnfinishedToolArgumentsAreRetryableStreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		tail string
	}{
		{"partial_bare_eof", `{"value":"one"`, ""},
		{"valid_object_bare_eof", `{"value":"one"}`, ""},
		{"partial_done_without_finish", `{"value":"one"`, "data: [DONE]\n\n"},
		{"valid_object_done_without_finish", `{"value":"one"}`, "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := makeStream(chatTerminalToolFrame("call_unfinished", tc.args, 0) + tc.tail)
			defer s.Close()
			for i := 0; i < 10; i++ {
				ev, err := s.Recv()
				if ev.Type == "tool_use_stop" || ev.Type == "message_stop" {
					t.Fatalf("unfinished tool call was finalized as %s", ev.Type)
				}
				if err != nil {
					if !errors.Is(err, io.ErrUnexpectedEOF) || !transport.IsNetworkError(err) || ev.Type != "error" {
						t.Fatalf("terminal event=%s error=%v, want retryable unexpected EOF", ev.Type, err)
					}
					return
				}
			}
			t.Fatal("stream never reported its missing finish reason")
		})
	}
}

func TestOpenAIStream_FinishedToolCallsPreserveUsageAndOrder(t *testing.T) {
	for _, tail := range []string{"", "data: [DONE]\n\n"} {
		name := "finish_then_eof"
		if tail != "" {
			name = "finish_then_done"
		}
		t.Run(name, func(t *testing.T) {
			payload := chatTerminalToolFrame("call_one", `{"value":"one"}`, 0) +
				chatTerminalToolFrame("call_two", `{"value":"two"}`, 1) + chatTerminalFinish +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":9,\"prompt_tokens_details\":{\"cached_tokens\":10}}}\n\n" + tail
			s := makeStream(payload)
			defer s.Close()
			var stops []string
			var input, output, cache int
			var stopReason string
			for {
				ev, err := s.Recv()
				if err != nil && !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
				if ev.Type == "tool_use_stop" {
					stops = append(stops, ev.ToolUseID+":"+ev.InputDelta)
				}
				if ev.InputTokens != 0 || ev.OutputTokens != 0 || ev.CacheReadInputTokens != 0 {
					input, output, cache = ev.InputTokens, ev.OutputTokens, ev.CacheReadInputTokens
				}
				if ev.StopReason != "" {
					stopReason = ev.StopReason
				}
				if errors.Is(err, io.EOF) {
					break
				}
			}
			if strings.Join(stops, ",") != `call_one:{"value":"one"},call_two:{"value":"two"}` || stopReason != "tool_use" {
				t.Fatalf("stops=%v reason=%s", stops, stopReason)
			}
			if input != 7 || output != 9 || cache != 10 {
				t.Fatalf("usage=%d/%d/%d, want 7/9/10", input, output, cache)
			}
		})
	}
}

func TestChatToolTerminalAgentDoesNotDispatchUnfinishedCalls(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "0")
	for _, tc := range []struct {
		name string
		args string
		tail string
	}{
		{"partial_bare_eof", `{"value":"one"`, ""},
		{"valid_object_bare_eof", `{"value":"one"}`, ""},
		{"partial_done_without_finish", `{"value":"one"`, "data: [DONE]\n\n"},
		{"valid_object_done_without_finish", `{"value":"one"}`, "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, chatTerminalToolFrame("call_unfinished", tc.args, 0)+tc.tail)
			}))
			defer server.Close()
			counter := &recoveryCountingTool{}
			registry := tools.NewRegistry()
			registry.Register(counter)
			loop := agent.NewLoop(New("local-test-key", server.URL, "test-model", 1024, time.Second, 0), registry, nil, nil, "Test tool stream completion.", 2)
			loop.AppendUser("Record the requested value.")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			events := make(chan agent.Event, 128)
			err := loop.Run(ctx, events)
			close(events)
			if !errors.Is(err, io.ErrUnexpectedEOF) || !transport.IsNetworkError(err) {
				t.Errorf("unfinished tool response error=%v, want retryable unexpected EOF", err)
			}
			counter.mu.Lock()
			executions := len(counter.values)
			counter.mu.Unlock()
			if executions != 0 {
				t.Errorf("unfinished tool executed %d times", executions)
			}
			for event := range events {
				if event.Kind == agent.EventToolResult {
					t.Error("unfinished stream was dispatched as a tool result")
				}
			}
			for _, message := range loop.History() {
				for _, block := range message.Content {
					if block.Type == "tool_use" || block.Type == "tool_result" {
						t.Errorf("unfinished tool was persisted as %s", block.Type)
					}
				}
			}
		})
	}
}

func TestChatToolTerminalAgentFinishedCallsExecuteAndReplayOnce(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []oaiMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			fmt.Fprint(w, chatTerminalToolFrame("call_one", `{"value":"one"}`, 0)+chatTerminalToolFrame("call_two", `{"value":"two"}`, 1)+chatTerminalFinish+"data: [DONE]\n\n")
			return
		}
		var calls, results int
		for _, message := range request.Messages {
			calls += len(message.ToolCalls)
			if message.Role == "tool" {
				results++
				if message.ToolCallID != "call_one" && message.ToolCallID != "call_two" {
					t.Errorf("unexpected replay ID %s", message.ToolCallID)
				}
			}
		}
		if calls != 2 || results != 2 {
			t.Errorf("replay calls/results=%d/%d, want 2/2", calls, results)
		}
		fmt.Fprint(w, chatTerminalText+"data: [DONE]\n\n")
	}))
	defer server.Close()
	counter := &recoveryCountingTool{}
	registry := tools.NewRegistry()
	registry.Register(counter)
	loop := agent.NewLoop(New("local-test-key", server.URL, "test-model", 1024, time.Second, 0), registry, nil, nil, "Test tool stream completion.", 2)
	loop.AppendUser("Record both requested values, then reply.")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events := make(chan agent.Event, 128)
	err := loop.Run(ctx, events)
	close(events)
	if err != nil {
		t.Fatal(err)
	}
	counter.mu.Lock()
	values := strings.Join(counter.values, ",")
	counter.mu.Unlock()
	if values != "one,two" || requests.Load() != 2 {
		t.Fatalf("values=%s requests=%d, want one,two / 2", values, requests.Load())
	}
}

func TestChatToolTerminalAgentRetriesDraftThenExecutesOnce(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "5")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "3")
	t.Setenv("METIS_RECOVERY_MAX_BACKOFF_SECONDS", "1")
	for _, tc := range []struct {
		name string
		args string
		tail string
	}{
		{"partial_bare_eof", `{"value":"one"`, ""},
		{"valid_object_done_without_finish", `{"value":"one"}`, "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []oaiMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				attempt := requests.Add(1)
				for _, message := range request.Messages {
					for _, call := range message.ToolCalls {
						if call.ID == "call_unfinished" {
							t.Error("unfinished draft entered request replay")
						}
					}
					if message.ToolCallID == "call_unfinished" {
						t.Error("unfinished draft produced a tool result")
					}
				}
				switch attempt {
				case 1:
					fmt.Fprint(w, chatTerminalToolFrame("call_unfinished", tc.args, 0)+tc.tail)
				case 2:
					fmt.Fprint(w, chatTerminalToolFrame("call_complete", `{"value":"one"}`, 0)+chatTerminalFinish+"data: [DONE]\n\n")
				default:
					fmt.Fprint(w, chatTerminalText+"data: [DONE]\n\n")
				}
			}))
			defer server.Close()
			counter := &recoveryCountingTool{}
			registry := tools.NewRegistry()
			registry.Register(counter)
			loop := agent.NewLoop(New("local-test-key", server.URL, "test-model", 1024, time.Second, 0), registry, nil, nil, "Test tool stream retry.", 2)
			loop.AppendUser("Record one value, then reply.")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			events := make(chan agent.Event, 128)
			err := loop.Run(ctx, events)
			close(events)
			if err != nil {
				t.Fatal(err)
			}
			counter.mu.Lock()
			values := strings.Join(counter.values, ",")
			counter.mu.Unlock()
			if values != "one" || requests.Load() != 3 {
				t.Fatalf("values=%s requests=%d, want one / 3", values, requests.Load())
			}
			var calls, results int
			for _, message := range loop.History() {
				for _, block := range message.Content {
					if block.Type == "tool_use" {
						calls++
						if block.ToolUseID != "call_complete" {
							t.Errorf("history contains unfinished call %s", block.ToolUseID)
						}
					}
					if block.Type == "tool_result" {
						results++
					}
				}
			}
			if calls != 1 || results != 1 {
				t.Errorf("history calls/results=%d/%d, want 1/1", calls, results)
			}
		})
	}
}
