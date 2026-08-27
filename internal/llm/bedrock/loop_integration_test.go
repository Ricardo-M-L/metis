package bedrock

import (
	"context"
	"errors"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// bedrockLoopProvider keeps this regression on Bedrock's real synthetic
// StreamReader while avoiding an AWS network request.
type bedrockLoopProvider struct {
	response *Response
}

func (*bedrockLoopProvider) Name() string          { return "bedrock" }
func (*bedrockLoopProvider) ModelID() string       { return "anthropic.claude-test-v1:0" }
func (*bedrockLoopProvider) MaxContextTokens() int { return 128_000 }
func (*bedrockLoopProvider) Complete(context.Context, Request) (*Response, error) {
	return nil, errors.New("test expects streaming")
}
func (p *bedrockLoopProvider) Stream(context.Context, Request) (StreamReader, error) {
	return newSyntheticStream(p.response), nil
}

func TestSyntheticStreamUsageFeedsLoopActiveContext(t *testing.T) {
	provider := &bedrockLoopProvider{response: &Response{
		Content:                  []ContentBlock{{Type: "text", Text: "bedrock answer"}},
		StopReason:               "end_turn",
		InputTokens:              10_000,
		OutputTokens:             250,
		CacheCreationInputTokens: 500,
		CacheReadInputTokens:     2_000,
	}}
	loop := agent.NewLoop(
		provider,
		tools.NewRegistry(),
		permission.New(permission.ModeBypass),
		nil,
		"",
		2,
	)
	loop.Model = provider.ModelID()
	loop.AppendUser("answer through the Bedrock synthetic stream")

	events := make(chan agent.Event, 32)
	if err := loop.Run(context.Background(), events); err != nil {
		t.Fatalf("Loop.Run: %v", err)
	}

	var tokenEvent *agent.Event
	for len(events) > 0 {
		ev := <-events
		if ev.Kind == agent.EventTokens {
			copy := ev
			tokenEvent = &copy
		}
	}
	if tokenEvent == nil {
		t.Fatal("Loop.Run emitted no EventTokens")
	}
	if tokenEvent.InputTokens != 10_000 || tokenEvent.OutputTokens != 250 ||
		tokenEvent.CacheCreationInputTokens != 500 || tokenEvent.CacheReadInputTokens != 2_000 {
		t.Fatalf("EventTokens = %+v, want Bedrock terminal usage", *tokenEvent)
	}

	const wantActiveContext = 10_000 + 250 + 500 + 2_000
	if got := loop.EstimateContextTokens(); got != wantActiveContext {
		t.Fatalf("active context = %d, want Bedrock terminal usage %d", got, wantActiveContext)
	}
}
