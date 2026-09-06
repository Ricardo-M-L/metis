package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type truncatedAnthropicToolProbe struct {
	tools.BaseTool
	calls atomic.Int32
}

func (*truncatedAnthropicToolProbe) Name() string { return "Bash" }
func (*truncatedAnthropicToolProbe) Description() string {
	return "must not run from a truncated stream"
}
func (*truncatedAnthropicToolProbe) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"required":   []string{"command"},
		"properties": map[string]any{"command": map[string]any{"type": "string"}},
	}
}
func (*truncatedAnthropicToolProbe) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (*truncatedAnthropicToolProbe) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (p *truncatedAnthropicToolProbe) Execute(context.Context, map[string]any) (*tools.Result, error) {
	p.calls.Add(1)
	return &tools.Result{Output: "unexpected execution"}, nil
}

func TestLoopDoesNotExecuteAnthropicToolCallWithoutMessageStop(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "open tool block",
			body: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"cut","name":"Bash","input":{}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"echo never\"}"}}

`,
		},
		{
			name: "closed tool block but missing message terminator",
			body: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"cut","name":"Bash","input":{}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"echo never\"}"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)

			provider := anthropic.New("test-key", server.URL, "claude-test", 256, 5*time.Second, "")
			probe := &truncatedAnthropicToolProbe{}
			registry := tools.NewRegistry()
			registry.Register(probe)
			loop := NewLoop(
				provider,
				registry,
				permission.New(permission.ModeBypassPermissions),
				nil,
				"system",
				2,
			)
			loop.AppendUser("run the probe")

			err := loop.Run(context.Background(), make(chan Event, 32))
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("Run() error = %v, want io.ErrUnexpectedEOF", err)
			}
			if got := probe.calls.Load(); got != 0 {
				t.Fatalf("truncated stream executed tool %d time(s)", got)
			}
			for _, message := range loop.History() {
				for _, block := range message.Content {
					if block.Type == "tool_use" {
						t.Fatalf("truncated tool call was persisted as executable history: %+v", block)
					}
				}
			}
		})
	}
}
