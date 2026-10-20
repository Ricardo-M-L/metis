package runtime

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func learningTraceResult(t *testing.T, enabled, tool, output string) session.TraceEvent {
	t.Helper()
	t.Setenv("METIS_LEARNING_TRACE_FULL_RESULTS", enabled)
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	adapter := NewTraceAdapter(store)
	adapter.SetSession("learning-test")
	adapter.OnEvent(agent.Event{Kind: agent.EventToolResult, ToolName: tool, ToolUseID: "provider-call", TraceCallID: "local-call", ToolResult: &agent.ToolResult{Output: output}})
	if err := adapter.Flush(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "learning-test.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 8*1024*1024)
	var result session.TraceEvent
	for scanner.Scan() {
		if err := json.Unmarshal(scanner.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestLearningTracePrivateFullResultsPreserveCompleteJSON(t *testing.T) {
	for _, tool := range []string{"Skill", "Workflow"} {
		t.Run(tool, func(t *testing.T) {
			data, _ := json.Marshal(map[string]string{"name": "case", "output": strings.Repeat("x", 12000)})
			row := learningTraceResult(t, "1", tool, string(data))
			if row.IsError || row.Text != string(data) {
				t.Fatalf("complete learning result missing: length=%d err=%v", len(row.Text), row.IsError)
			}
			var value map[string]string
			if err := json.Unmarshal([]byte(row.Text), &value); err != nil {
				t.Fatal("partial JSON", err)
			}
		})
	}
}
func TestLearningTraceDoesNotExpandOtherOrDefaultDiagnostics(t *testing.T) {
	for _, tc := range []struct{ flag, tool string }{{"", "Skill"}, {"0", "Workflow"}, {"true", "Skill"}, {"1", "Bash"}} {
		t.Run(tc.flag+tc.tool, func(t *testing.T) {
			output := strings.Repeat("x", 4000)
			row := learningTraceResult(t, tc.flag, tc.tool, output)
			if row.Text == output || !strings.Contains(row.Text, "truncated") {
				t.Fatal("ordinary trace limit changed")
			}
		})
	}
}
func TestLearningTraceFullResultsStillRedactSecrets(t *testing.T) {
	row := learningTraceResult(t, "1", "Workflow", strings.Repeat("safe output\n", 1000)+"Authorization: Bearer sk-live-secret-123456789abcdef\n")
	if strings.Contains(row.Text, "sk-live-secret") || !strings.Contains(row.Text, "[REDACTED]") || strings.Contains(row.Text, "truncated") {
		t.Fatal("full-result mode bypassed redaction")
	}
}
func TestLearningTraceOversizeIsExplicitIncompleteEvidence(t *testing.T) {
	row := learningTraceResult(t, "1", "Workflow", strings.Repeat("x", 2*1024*1024+1))
	if !row.IsError || !strings.Contains(row.Text, "learning evidence incomplete") || strings.Contains(row.Text, "truncated") {
		t.Fatalf("oversize evidence: %+v", row)
	}
}
