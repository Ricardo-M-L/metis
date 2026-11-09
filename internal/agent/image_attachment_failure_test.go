package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type failingImageAttachmentStore struct{ err error }

func (s failingImageAttachmentStore) ReadImageAttachment(string, string) ([]byte, error) {
	return nil, s.err
}
func (s failingImageAttachmentStore) RecordImageAttachments(string, []llm.ContentBlock) error {
	return s.err
}

func TestImageAttachmentRegistrationFailureStillCheckpointsReceiptAndStopsProvider(t *testing.T) {
	wantErr := errors.New("image registration unavailable")
	ctx := WithImageAttachmentSession(context.Background(), failingImageAttachmentStore{wantErr}, "owner")
	called := false
	loop := NewLoop(nil, nil, nil, nil, "", 2)
	loop.HistoryCheckpoint = func(history []llm.Message) error {
		called = true
		if continuationResultCount(history) != 1 {
			t.Fatal("registration failure lost executed tool receipt")
		}
		return nil
	}
	if err := loop.commitToolBatchWithContext(ctx, []llm.ContentBlock{{Type: "tool_result", ToolUseID: "executed", ToolResult: "external action completed", ToolResultBlocks: []llm.ContentBlock{{Type: "image", MediaType: "image/png", Data: "eA=="}}}}); !errors.Is(err, wantErr) {
		t.Fatalf("registration error = %v", err)
	}
	if !called {
		t.Fatal("attachment registration failure bypassed receipt checkpoint")
	}
	provider := &batchNativeProvider{planModeStreamProvider: planModeStreamProvider{streams: [][]llm.StreamEvent{toolBatchEvents(llm.ContentBlock{Type: "tool_use", ToolUseID: "shot", ToolName: "Screenshot"})}}}
	registry := tools.NewRegistry()
	registry.Register(batchScreenshotTool{data: base64.StdEncoding.EncodeToString(batchNativePNG(t))})
	loop = NewLoop(provider, registry, nil, nil, "", 4)
	loop.AppendUser("perform the action and inspect its screenshot")
	dir := t.TempDir()
	transcript, err := NewDurableSubAgentTranscript(dir, "agt-receipt", NewSubAgentHeader("agt-receipt", "offline", "owner", "receipt-worker", dir, "default"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	loop.HistoryCheckpoint = transcript.Checkpoint
	if err := loop.Run(ctx, make(chan Event, 128)); !errors.Is(err, wantErr) {
		t.Fatalf("loop registration error = %v", err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := LoadSubAgentSnapshot(dir, "agt-receipt")
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || continuationResultCount(restarted.Messages) != 1 {
		t.Fatalf("failed registration continued or lost durable receipt: requests=%d results=%d", len(provider.requests), continuationResultCount(restarted.Messages))
	}
}
