package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

type nativeViewProvider struct {
	requests []llm.Request
	path     string
}

func (*nativeViewProvider) Name() string          { return "native-view-test" }
func (*nativeViewProvider) ModelID() string       { return "native-view-test" }
func (*nativeViewProvider) MaxContextTokens() int { return 200000 }
func (*nativeViewProvider) SupportsVision() bool  { return true }
func (*nativeViewProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, io.EOF
}
func (p *nativeViewProvider) Stream(_ context.Context, r llm.Request) (llm.StreamReader, error) {
	p.requests = append(p.requests, r)
	if len(p.requests) > 1 {
		return &nativeViewStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "image inspected"}, {Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"}}}, nil
	}
	input, _ := json.Marshal(map[string]any{"path": p.path})
	return &nativeViewStream{events: []llm.StreamEvent{{Type: "tool_use_start", ToolUseID: "view", ToolName: "ViewImage"}, {Type: "tool_input_delta", ToolUseID: "view", InputDelta: string(input)}, {Type: "tool_use_stop", ToolUseID: "view"}, {Type: "message_delta", StopReason: "tool_use"}, {Type: "message_stop"}}}, nil
}

type nativeViewStream struct {
	events []llm.StreamEvent
	i      int
}

func (s *nativeViewStream) Recv() (llm.StreamEvent, error) {
	if s.i >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}
func (*nativeViewStream) Close() error { return nil }

// Invoke the real builtin file decoder/preparation path, not a tool-result stub.
// Gate is deliberately bypassed: this regression concerns native-image admission.
type nativeViewTool struct{ builtin.ViewImage }

func (nativeViewTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func TestToolBatchRealBuiltinViewImageContinuation(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	x := uint32(17)
	for i := range img.Pix {
		x = x*1664525 + 1013904223
		img.Pix[i] = byte(x >> 24)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 65000 {
		t.Fatalf("fixture too small: %d", buf.Len())
	}
	path := filepath.Join(t.TempDir(), "native.png")
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	p := &nativeViewProvider{path: path}
	reg := tools.NewRegistry()
	reg.Register(nativeViewTool{})
	l := agent.NewLoop(p, reg, nil, nil, "system", 5)
	l.SpillDir = t.TempDir()
	l.AppendUser("inspect image")
	var saved []llm.Message
	l.HistoryCheckpoint = func(h []llm.Message) error { saved = h; return nil }
	if err := l.Run(context.Background(), make(chan agent.Event, 100)); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 2 {
		t.Fatalf("native ViewImage continuation calls=%d", len(p.requests))
	}
	found := false
	for _, m := range p.requests[1].Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID == "view" {
				if b.IsError || len(b.ToolResultBlocks) != 2 || b.ToolResultBlocks[1].Type != "image" || b.ToolResultBlocks[1].Data == "" {
					t.Fatalf("real ViewImage missing: %+v", b)
				}
				found = true
			}
		}
	}
	if !found || len(saved) == 0 {
		t.Fatal("real ViewImage/checkpoint absent")
	}
}
