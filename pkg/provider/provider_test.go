package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/Ricardo-M-L/metis/pkg/llm"
)

// fakeProvider documents the minimum surface a 3rd-party plugin author
// targets. Compile-time check that the public Provider interface is
// implementable from outside the package.
type fakeProvider struct {
	name      string
	maxCtx    int
	gotReq    Request
	streamErr error
}

func (p *fakeProvider) Name() string          { return p.name }
func (p *fakeProvider) ModelID() string       { return "" }
func (p *fakeProvider) MaxContextTokens() int { return p.maxCtx }
func (p *fakeProvider) Complete(_ context.Context, req Request) (*Response, error) {
	p.gotReq = req
	return &Response{StopReason: "end_turn"}, nil
}
func (p *fakeProvider) Stream(_ context.Context, req Request) (StreamReader, error) {
	p.gotReq = req
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	return eofStream{}, nil
}

type eofStream struct{}

func (eofStream) Close() error               { return nil }
func (eofStream) Recv() (StreamEvent, error) { return StreamEvent{}, io.EOF }

func TestProviderInterface_PluginCompiles(t *testing.T) {
	// Static check — failing to compile is the canary for an SDK break.
	var _ Provider = &fakeProvider{}
}

func TestProviderVisionCapabilityUnknownForLegacyProvider(t *testing.T) {
	p := &fakeProvider{}
	if got := ProviderVisionCapability(p); got != VisionUnknown {
		t.Fatalf("ProviderVisionCapability(legacy provider) = %v, want VisionUnknown", got)
	}
}

func TestStreamReaderInterface_PluginCompiles(t *testing.T) {
	var _ StreamReader = eofStream{}
}

func TestRequest_EffortFieldUsesPkgLLM(t *testing.T) {
	// The Effort field is typed as pkg/llm.Effort — pin that the public
	// SDK pieces compose correctly without referencing internal/llm.
	req := Request{
		Model:  "test",
		Effort: llm.EffortHigh,
	}
	if req.Effort.OpenAI() != "high" {
		t.Errorf("Effort.OpenAI() = %q, want high", req.Effort.OpenAI())
	}
}

func TestToolSpecExposureIsNotSerialized(t *testing.T) {
	raw, err := json.Marshal(ToolSpec{
		Name: "Remote", Description: "remote tool", InputSchema: map[string]any{"type": "object"}, Exposure: "deferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"name":"Remote","description":"remote tool","input_schema":{"type":"object"}}` {
		t.Fatalf("provider-only routing metadata leaked into JSON: %s", raw)
	}
}

func TestRoleConstants(t *testing.T) {
	if RoleSystem == RoleUser || RoleAssistant == RoleTool {
		t.Error("role constants must be distinct")
	}
}

func TestProvider_StreamCarriesRequest(t *testing.T) {
	p := &fakeProvider{name: "test", maxCtx: 100_000}
	ctx := context.Background()
	in := Request{Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}}}}
	stream, err := p.Stream(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if p.gotReq.Model != "claude-x" {
		t.Errorf("Stream didn't capture request: got %+v", p.gotReq)
	}
}

func TestProvider_StreamErrorPropagates(t *testing.T) {
	want := errors.New("connect failed")
	p := &fakeProvider{streamErr: want}
	_, err := p.Stream(context.Background(), Request{})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

func TestStreamEvent_ZeroValueOK(t *testing.T) {
	var ev StreamEvent
	if ev.Type != "" || ev.Err != nil {
		t.Errorf("zero StreamEvent has unexpected fields: %+v", ev)
	}
}

func TestContentBlock_ToolUseShape(t *testing.T) {
	// Pin the JSON shape so a future struct field reorder doesn't break
	// providers that hand-write block JSON.
	b := ContentBlock{
		Type:      "tool_use",
		ToolUseID: "t-1",
		ToolName:  "Read",
		ToolInput: map[string]any{"path": "/tmp/x"},
	}
	if b.ToolName != "Read" || b.ToolInput["path"] != "/tmp/x" {
		t.Errorf("ContentBlock fields wrong: %+v", b)
	}
}

func TestContentBlock_ToolResultPresentationJSONRoundTrip(t *testing.T) {
	in := ContentBlock{
		Type:       "tool_result",
		ToolUseID:  "t-2",
		ToolResult: "Artifact updated",
		Display:    "Interactive artifact",
		Presentation: map[string]any{
			"kind":        "artifact",
			"artifact_id": "art-123",
			"version":     2,
		},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out ContentBlock
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Display != in.Display || out.Presentation["kind"] != "artifact" || out.Presentation["artifact_id"] != "art-123" {
		t.Fatalf("presentation did not round-trip: %+v", out)
	}
	if got, ok := out.Presentation["version"].(float64); !ok || got != 2 {
		t.Fatalf("version = %#v, want JSON number 2", out.Presentation["version"])
	}
}
