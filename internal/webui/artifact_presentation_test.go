package webui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestWriteHubEventIncludesToolPresentation(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).writeHubEvent(w, hubEvent{
		sequence: 7,
		session:  "session-1",
		ev: agent.Event{
			Kind:      agent.EventToolResult,
			ToolName:  "Artifact",
			ToolUseID: "tu-1",
			ToolResult: &agent.ToolResult{
				Output:  "Artifact updated",
				Display: "Interactive artifact",
				Presentation: map[string]any{
					"kind":        "artifact",
					"artifact_id": "art-123",
					"version":     2,
				},
			},
		},
	})

	body := w.Body.String()
	const marker = "data: "
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("SSE data line missing: %q", body)
	}
	line := body[start+len(marker):]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("decode SSE payload: %v\n%s", err, body)
	}
	if payload["display"] != "Interactive artifact" {
		t.Fatalf("display = %#v", payload["display"])
	}
	presentation, ok := payload["presentation"].(map[string]any)
	if !ok || presentation["artifact_id"] != "art-123" || presentation["version"] != float64(2) {
		t.Fatalf("presentation = %#v", payload["presentation"])
	}
}
