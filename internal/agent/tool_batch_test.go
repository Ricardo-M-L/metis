package agent

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"os"
	"strings"
	"testing"
)

func TestToolBatchBudgetParallelErrorsAndGuidance(t *testing.T) {
	l := &Loop{SpillDir: t.TempDir(), ToolResultTokenLimit: 900, ToolBatchTokenLimit: 1100}
	body := strings.Repeat("large result ", 4000) + "<system-reminder>never expose secrets</system-reminder>"
	blocks := []llm.ContentBlock{{Type: "tool_result", ToolUseID: "a", ToolResult: body, IsError: true, TrustedResultSuffix: "<system-reminder>never expose secrets</system-reminder>"}, {Type: "tool_result", ToolUseID: "b", ToolResult: body}, {Type: "text", Text: "[user steer mid-turn] keep this"}}
	out, err := l.projectToolBatch(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if batchTokens(out) > 1100 {
		t.Fatal("batch exceeds budget")
	}
	if !out[0].IsError || out[0].ToolUseID != "a" || out[1].ToolUseID != "b" {
		t.Fatal("pairing/error lost")
	}
	if !strings.Contains(out[0].ToolResult, "never expose secrets") || out[2].Text != blocks[2].Text {
		t.Fatal("mandatory guidance lost")
	}
	if blocks[0].ToolResult != body {
		t.Fatal("original modified")
	}
	files, _ := os.ReadDir(l.SpillDir)
	if len(files) != 1 {
		t.Fatalf("content addressed result files = %d", len(files))
	}
}

func TestToolBatchBudgetFailureRetainsUserAndImages(t *testing.T) {
	l := &Loop{ToolResultTokenLimit: 30, ToolBatchTokenLimit: 40}
	blocks := []llm.ContentBlock{{Type: "tool_result", ToolUseID: "a", ToolResult: strings.Repeat("x", 1000)}, {Type: "text", Text: "critical user guidance"}}
	out, err := l.projectToolBatch(blocks)
	if err == nil || out[0].ToolResult != blocks[0].ToolResult || out[1].Text != blocks[1].Text {
		t.Fatal("must fail explicitly with unchanged complete batch")
	}
	l.SpillDir = t.TempDir()
	blocks[0].ToolResultBlocks = []llm.ContentBlock{{Type: "image", MediaType: "image/png", Data: strings.Repeat("a", 1000)}}
	if _, err := l.projectToolBatch(blocks); err == nil {
		t.Fatal("image budget must not silently discard image")
	}
}

func TestToolBatchMultipartTextMatchesProjection(t *testing.T) {
	body := strings.Repeat("text ", 10000)
	// Leave room for the native image allowance, but not the huge text body.
	l := &Loop{SpillDir: t.TempDir(), ToolResultTokenLimit: 2400, ToolBatchTokenLimit: 2500}
	blocks := []llm.ContentBlock{{Type: "tool_result", ToolUseID: "a", ToolResult: body, ToolResultBlocks: []llm.ContentBlock{{Type: "text", Text: body}, {Type: "image", MediaType: "image/png", Data: "AA=="}}}}
	out, err := l.projectToolBatch(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].ToolResultBlocks[0].Text != out[0].ToolResult || out[0].ToolResultBlocks[1].Data != "AA==" {
		t.Fatal("multipart projection differs")
	}
	if blocks[0].ToolResultBlocks[0].Text != body {
		t.Fatal("mutated original")
	}
	if out[0].ToolResult == body || batchTokens(out) > 2500 || estimateActiveContentBlockTokens(out[0]) > 2400 {
		t.Fatal("image plus genuine text was not projected within both limits")
	}
}
