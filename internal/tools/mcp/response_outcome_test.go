package mcp_tools

import (
	"strings"
	"testing"
)

func TestParseMCPResponsePreservesActionOutcome(t *testing.T) {
	result, ok := parseMCPResponse([]byte(`{"content":[{"type":"text","text":"input sent"}],"structuredContent":{"outcome":{"dispatch":"sent","verification":"unknown","reason":"page navigated"}},"isError":false}`))
	if !ok || !strings.Contains(result.Output, `"verification":"unknown"`) || !strings.Contains(result.Output, "page navigated") {
		t.Fatalf("model lost action delivery/verification evidence: %+v", result)
	}
}
