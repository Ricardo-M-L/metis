package transcript

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"testing"
)

func TestDeepSnapshotDetachesRequestBlocksAndMetadata(t *testing.T) {
	original := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", Text: "original", ToolInput: map[string]any{"nested": []any{map[string]any{"number": 7}}}, Presentation: map[string]any{"labels": []string{"before"}}, ProviderHint: map[string]string{"signature": "original"}, ToolResultBlocks: []llm.ContentBlock{{Type: "image", Data: "immutable-image"}}}}}}
	copied := DeepSnapshot(original)
	original[0].Content[0].Text = "changed"
	original[0].Content[0].ToolInput["nested"].([]any)[0].(map[string]any)["number"] = 8
	original[0].Content[0].Presentation["labels"].([]string)[0] = "changed"
	original[0].Content[0].ProviderHint["signature"] = "changed"
	original[0].Content[0].ToolResultBlocks[0].Data = "changed"
	b := copied[0].Content[0]
	if b.Text != "original" || b.ToolInput["nested"].([]any)[0].(map[string]any)["number"] != 7 || b.Presentation["labels"].([]string)[0] != "before" || b.ProviderHint["signature"] != "original" || b.ToolResultBlocks[0].Data != "immutable-image" {
		t.Fatal("request snapshot shares mutable state")
	}
	if DeepSnapshot(nil) != nil {
		t.Fatal("nil history changed")
	}
}
