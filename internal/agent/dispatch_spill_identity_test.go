package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestDispatchRepeatedProviderIDRecoversCurrentOutputAfterBatchProjection(t *testing.T) {
	l := &Loop{SpillDir: t.TempDir()}
	bodies := []string{strings.Repeat("old output ", 6000), strings.Repeat("new output ", 6000)}
	paths := make([]string, 0, len(bodies))
	for _, body := range bodies {
		reg := tools.NewRegistry()
		reg.Register(bigOutputTool{output: body})
		l.Registry = reg
		result, err := l.executeBatch(context.Background(), []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "same-provider-id", ToolName: "BigStub",
		}}, make(chan Event, 16), HookContext{})
		if err != nil {
			t.Fatal(err)
		}
		projected, err := l.projectToolBatch(result)
		if err != nil {
			t.Fatal(err)
		}
		if len(projected) != 1 {
			t.Fatalf("result count = %d", len(projected))
		}
		_, rest, ok := strings.Cut(projected[0].ToolResult, "Full output saved to: ")
		if !ok {
			t.Fatal("model result lost recovery path")
		}
		path, _, ok := strings.Cut(rest, " — use Read")
		if !ok {
			t.Fatal("invalid recovery path in model result")
		}
		full, err := os.ReadFile(path)
		if err != nil || string(full) != body {
			t.Fatalf("provider ID reuse returned a preview for new content with another full output: current=%v err=%v", string(full) == body, err)
		}
		paths = append(paths, path)
	}
	if paths[0] == paths[1] {
		t.Fatal("different result bodies share a recovery path")
	}
	first, err := os.ReadFile(paths[0])
	if err != nil || string(first) != bodies[0] {
		t.Fatal("second invocation changed the first invocation's recovery content")
	}
}
