package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHandleToolSearch_SelectLiveBuiltinsReturnsCompactHint(t *testing.T) {
	hugeDescription := strings.Repeat("already-visible built-in description ", 200)
	hugeSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"payload": map[string]any{"type": "string", "description": strings.Repeat("schema details ", 500)},
		},
	}
	l := newLoopWithTools(
		fakeMCPTool{name: "Read", description: hugeDescription, schema: hugeSchema},
		fakeMCPTool{name: "Grep", description: hugeDescription, schema: hugeSchema},
		fakeMCPTool{name: "Bash", description: hugeDescription, schema: hugeSchema},
	)

	parsed, isErr := invokeSearch(t, l, map[string]any{"query": "select:Read,Grep,Bash"})
	if isErr {
		t.Fatalf("live built-ins should return a successful compact hint: %v", parsed)
	}
	raw, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if len(raw) > 512 {
		t.Fatalf("live built-in selection re-embedded descriptions or schemas: %d bytes", len(raw))
	}
	for _, item := range parsed["matches"].([]any) {
		match := item.(map[string]any)
		if match["already_available"] != true {
			t.Fatalf("match %q should tell the model to invoke it directly: %v", match["name"], match)
		}
		if _, ok := match["input_schema"]; ok {
			t.Fatalf("live built-in %q must not duplicate its schema in history", match["name"])
		}
		if _, ok := match["description"]; ok {
			t.Fatalf("live built-in %q must not duplicate its description in history", match["name"])
		}
	}
}
