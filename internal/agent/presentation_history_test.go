package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestPresentationHistoryRedactsToolArgumentsWithoutMutatingSource(t *testing.T) {
	t.Parallel()

	githubToken := "ghp_" + strings.Repeat("a", 36)
	bearerToken := strings.Repeat("B", 32)
	original := []llm.Message{
		{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				{
					Type:      "tool_use",
					ToolUseID: "tool-1",
					ToolName:  "WebFetch",
					ToolInput: map[string]any{
						"api_key":  "arbitrary-api-secret",
						"password": map[string]any{"value": "hunter2"},
						"command":  "curl -H 'Authorization: Bearer " + bearerToken + "' https://example.test/status",
						"url":      "https://example.test/data?token=" + githubToken,
						"safe":     "keep-me",
						"nested":   []any{map[string]any{"label": "safe-child"}},
					},
					Presentation: map[string]any{
						"artifact": map[string]any{"label": "keep-card"},
						"api_key":  "presentation-secret",
					},
					ProviderHint: map[string]string{"signature": "round-trip"},
				},
			},
		},
	}

	got := PresentationHistory(original)
	if len(got) != 1 || len(got[0].Content) != 1 {
		t.Fatalf("presentation history shape = %#v", got)
	}
	input := got[0].Content[0].ToolInput
	if input["api_key"] != "[REDACTED]" || input["password"] != "[REDACTED]" {
		t.Fatalf("structured credentials not redacted: %#v", input)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)
	for _, secret := range []string{"arbitrary-api-secret", "hunter2", githubToken, bearerToken} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("presentation leaked %q in %s", secret, encoded)
		}
	}
	if got[0].Content[0].Presentation["api_key"] != "[REDACTED]" {
		t.Fatalf("persisted presentation credential not redacted: %#v", got[0].Content[0].Presentation)
	}
	if input["safe"] != "keep-me" || !strings.Contains(input["command"].(string), "curl -H") || !strings.Contains(input["url"].(string), "example.test/data") {
		t.Fatalf("safe argument context was not preserved: %#v", input)
	}

	// The provider/session copy remains canonical and receives no presentation
	// redaction. Mutating either graph must not affect the other.
	sourceInput := original[0].Content[0].ToolInput
	if sourceInput["api_key"] != "arbitrary-api-secret" || sourceInput["url"].(string) != "https://example.test/data?token="+githubToken {
		t.Fatalf("canonical history mutated: %#v", sourceInput)
	}
	input["safe"] = "changed-presentation"
	input["nested"].([]any)[0].(map[string]any)["label"] = "changed-child"
	got[0].Content[0].Presentation["artifact"].(map[string]any)["label"] = "changed-card"
	got[0].Content[0].ProviderHint["signature"] = "changed-signature"
	if sourceInput["safe"] != "keep-me" || sourceInput["nested"].([]any)[0].(map[string]any)["label"] != "safe-child" {
		t.Fatalf("tool input presentation aliases canonical history: %#v", sourceInput)
	}
	if original[0].Content[0].Presentation["artifact"].(map[string]any)["label"] != "keep-card" ||
		original[0].Content[0].Presentation["api_key"] != "presentation-secret" ||
		original[0].Content[0].ProviderHint["signature"] != "round-trip" {
		t.Fatalf("metadata presentation aliases canonical history: %#v", original[0].Content[0])
	}
}

func TestPresentationHistoryPreservesNilAndSafeArguments(t *testing.T) {
	t.Parallel()
	if got := PresentationHistory(nil); got != nil {
		t.Fatalf("nil history became %#v", got)
	}
	original := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "tool_use", ToolName: "Bash", ToolInput: map[string]any{"command": "go test ./...", "timeout": 120}},
		}},
	}
	got := PresentationHistory(original)
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("safe history changed:\n got=%#v\nwant=%#v", got, original)
	}
}
