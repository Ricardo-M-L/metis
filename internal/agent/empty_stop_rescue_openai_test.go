package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestLoopRun_OpenAIResponsesEmptyFinalRescueWire(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), body...))
		call := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-empty\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"rescued summary\"}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-rescued\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider := openai.NewResponses("test-key", server.URL, "glm-5.3", 4096, 5*time.Second, 0)
	registry := tools.NewRegistry()
	registry.Register(lowOutputTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("answer the user")
	events := make(chan Event, 64)
	if err := loop.Run(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	close(events)

	var text, stop string
	for event := range events {
		if event.Kind == EventTextDelta {
			text += event.TextDelta
		}
		if event.Kind == EventLoopDone {
			stop = event.StopReason
		}
	}
	if text != "rescued summary" || stop != "end_turn" {
		t.Fatalf("text=%q stop=%q", text, stop)
	}

	mu.Lock()
	captured := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("HTTP request count = %d, want 2", len(captured))
	}
	var first, second struct {
		Tools []json.RawMessage        `json:"tools"`
		Input []map[string]interface{} `json:"input"`
	}
	if err := json.Unmarshal(captured[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(captured[1], &second); err != nil {
		t.Fatal(err)
	}
	if len(first.Tools) == 0 {
		t.Fatal("initial Responses request unexpectedly omitted tools")
	}
	if len(second.Tools) != 0 {
		t.Fatalf("rescue Responses request exposed %d tools", len(second.Tools))
	}
	for _, item := range second.Input {
		if item["role"] != "assistant" {
			continue
		}
		content, present := item["content"]
		if !present || content == nil {
			t.Fatalf("rescue wire contains empty assistant item: %s", captured[1])
		}
		if parts, ok := content.([]interface{}); ok && len(parts) == 0 {
			t.Fatalf("rescue wire contains empty assistant parts: %s", captured[1])
		}
	}
}
