//go:build darwin

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestComputerUseDesktopFinalSessionRejectsLateInvalidEvidence(t *testing.T) {
	for _, mutation := range []string{"second-press", "failed-native", "forbidden-tool", "duplicate-session", "incomplete-json", "replaced-session", "missing-session"} {
		t.Run(mutation, func(t *testing.T) {
			env, filename, verified := newCULiveAXDesktopGuardFixture(t)
			data := append([]byte{}, verified...)
			switch mutation {
			case "second-press":
				data = append(data, encodeCULiveAXMessages(t, []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "late-press", ToolName: cuLiveAXPressTool, ToolInput: map[string]any{"snapshot_id": "snapshot-3", "element_ref": "button-3"}}}}})...)
			case "failed-native":
				data = append(data, encodeCULiveAXMessages(t, []llm.Message{
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "late-snapshot", ToolName: cuLiveAXSnapshotTool}}},
					{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "late-snapshot", IsError: true, ToolResult: "fixture no longer available"}}},
				})...)
			case "forbidden-tool":
				data = append(data, encodeCULiveAXMessages(t, []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "late-other-tool", ToolName: "Bash"}}}})...)
			case "duplicate-session":
				if err := os.WriteFile(filepath.Join(filepath.Dir(filename), "duplicate.jsonl"), verified, 0o600); err != nil {
					t.Fatal(err)
				}
			case "incomplete-json":
				data = append(data, []byte(`{"type":"message"`)...)
			case "replaced-session":
				data = bytes.ReplaceAll(data, []byte("fixture-instance"), []byte("replacement-instance"))
				if _, err := inspectCULiveAXSession(data, env.FixturePID, env.Marker); err != nil {
					t.Fatalf("replacement must independently pass to exercise original-session binding: %v", err)
				}
			case "missing-session":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
			}
			if mutation != "missing-session" {
				if err := os.WriteFile(filename, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := inspectCULiveAXDesktopFinalSession(env, verified); err == nil {
				t.Fatal("Desktop final verification accepted changed or invalid evidence after its passing prefix")
			}
		})
	}
}

func TestComputerUseDesktopFinalSessionAcceptsCompletedConversation(t *testing.T) {
	for _, appendFinal := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "final-prose"}[appendFinal], func(t *testing.T) {
			env, filename, verified := newCULiveAXDesktopGuardFixture(t)
			if appendFinal {
				data := append(append([]byte{}, verified...), encodeCULiveAXMessages(t, []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "The disposable fixture task is complete."}}}})...)
				if err := os.WriteFile(filename, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			evidence, _, err := inspectCULiveAXDesktopFinalSession(env, verified)
			if err != nil || !evidence.FinalVerified || evidence.SetValues != 1 || evidence.Presses != 1 || evidence.Snapshots != 3 {
				t.Fatalf("complete final Desktop session was rejected: evidence=%+v error=%v", evidence, err)
			}
		})
	}
}

func newCULiveAXDesktopGuardFixture(t *testing.T) (cuLiveAXEnvironment, string, []byte) {
	t.Helper()
	env := cuLiveAXEnvironment{MetisHome: t.TempDir(), Prompt: "synthetic Desktop acceptance prompt", FixturePID: 42, Marker: "synthetic-fresh-marker"}
	dir := filepath.Join(env.MetisHome, "sessions")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	messages := append([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: env.Prompt}}}}, syntheticCULiveAXMessages(t)...)
	verified := encodeCULiveAXMessages(t, messages)
	if _, err := inspectCULiveAXSession(verified, env.FixturePID, env.Marker); err != nil {
		t.Fatalf("initial evidence must demonstrate the full AX task: %v", err)
	}
	filename := filepath.Join(dir, "acceptance.jsonl")
	if err := os.WriteFile(filename, verified, 0o600); err != nil {
		t.Fatal(err)
	}
	return env, filename, verified
}
