package webui

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func historyTraceMessage(role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

// This opt-in check copies two explicitly selected files into a temporary
// store. It never modifies the originals or logs prompt/tool/output content.
func TestRestoreMissingTraceInputsSelectedSample(t *testing.T) {
	sessionPath, tracePath := os.Getenv("METIS_TRACE_SAMPLE_SESSION"), os.Getenv("METIS_TRACE_SAMPLE_EVENTS")
	if sessionPath == "" || tracePath == "" {
		t.Skip("set both explicit sample paths to verify a private session using counts only")
	}
	sid := strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl")
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	traces, err := session.NewTraceStore(filepath.Join(root, "traces"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = traces.Close() })
	for _, selected := range []struct{ source, destination string }{
		{sessionPath, filepath.Join(root, "sessions", sid+".jsonl")},
		{tracePath, filepath.Join(root, "traces", sid+".jsonl")},
	} {
		raw, err := os.ReadFile(selected.source)
		if err != nil {
			t.Fatal("cannot read explicitly selected sample")
		}
		if err := os.WriteFile(selected.destination, raw, 0o600); err != nil {
			t.Fatal("cannot copy selected sample to temporary store")
		}
	}
	_, messages, err := store.Load(sid)
	if err != nil {
		t.Fatal("cannot load selected sample history")
	}
	nodes := traces.Trace(sid)
	got := restoreMissingTraceUserInputs(sid, nodes, messages)
	before, after, restored := 0, 0, 0
	firstTen := make(map[int]bool)
	for _, node := range nodes {
		if node.Event.Kind == "user" {
			before++
		}
	}
	for _, node := range got {
		if node.Event.Kind == "user" {
			after++
		}
		if node.Event.Source == "history-reconstructed" {
			restored++
			if node.Event.Turn >= 1 && node.Event.Turn <= 10 {
				firstTen[node.Event.Turn] = true
			}
		}
	}
	t.Logf("original_events=%d history_messages=%d USER_before=%d USER_after=%d restored=%d first_ten_restored=%d", len(nodes), len(messages), before, after, restored, len(firstTen))
	if len(firstTen) != 10 {
		for turn := 1; turn <= 10; turn++ {
			if firstTen[turn] {
				continue
			}
			counts := make(map[string]int)
			for _, node := range nodes {
				if node.Depth == 0 && node.Event.Turn == turn {
					counts[node.Event.Kind]++
				}
			}
			t.Logf("unmatched_turn=%d top_level_kind_counts=%v", turn, counts)
		}
		t.Fatal("selected regression sample still has missing USER rows in its first ten turns")
	}
}

func TestRestoreMissingTraceInputsRecoversFirstTenTurnsWithoutRenumbering(t *testing.T) {
	const sid = "partial-history"
	var nodes []session.TracedNode
	var messages []llm.Message
	for turn := 1; turn <= 11; turn++ {
		prompt := fmt.Sprintf("human question %d", turn)
		answer := fmt.Sprintf("assistant answer %d", turn)
		toolID := fmt.Sprintf("tool-%d", turn)
		messages = append(messages, historyTraceMessage(llm.RoleUser, prompt), llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: "Read", ToolUseID: toolID}}},
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: toolID, ToolResult: "tool output"}, {Type: "text", Text: "runtime tool metadata"}}},
			historyTraceMessage(llm.RoleUser, "<job_notification>automatic completion</job_notification>"),
			historyTraceMessage(llm.RoleUser, "[user steer mid-turn] keep going"),
			historyTraceMessage(llm.RoleAssistant, answer))
		add := func(kind, text string) {
			nodes = append(nodes, session.TracedNode{Event: session.TraceEvent{ID: fmt.Sprintf("real-%d-%s", turn, kind), SessionID: sid, Turn: turn, Sequence: int64(len(nodes) + 1), TS: time.Unix(int64(100+turn), 0), Kind: kind, Text: text}})
		}
		if turn == 11 {
			add("user", prompt)
		}
		add("tool_start", "{}")
		nodes[len(nodes)-1].Event.ToolName = "Read"
		nodes[len(nodes)-1].Event.ToolUseID = toolID
		add("text", answer)
		add("loop_done", "complete")
	}
	before := append([]session.TracedNode(nil), nodes...)
	got := restoreMissingTraceUserInputs(sid, nodes, messages)
	if len(got) != len(nodes)+10 {
		t.Fatalf("display rows = %d, want original %d plus ten recovered USER rows", len(got), len(nodes))
	}
	var original []session.TracedNode
	userTurns := make(map[int]int)
	for index, node := range got {
		if node.Event.Kind == "user" {
			userTurns[node.Event.Turn]++
		}
		if node.Event.Source != "history-reconstructed" {
			original = append(original, node)
			continue
		}
		if !node.Event.TS.IsZero() || node.Event.Sequence != 0 || node.Event.ID == "" || node.Depth != 0 {
			t.Fatal("reconstructed USER invented timing/sequence or lacks a display identity")
		}
		if node.Event.Text != fmt.Sprintf("human question %d", node.Event.Turn) || got[index+1].Event.Turn != node.Event.Turn {
			t.Fatal("reconstructed USER was attached to the wrong live turn")
		}
	}
	for turn := 1; turn <= 11; turn++ {
		if userTurns[turn] != 1 {
			t.Fatalf("turn %d contains %d USER rows, want 1", turn, userTurns[turn])
		}
	}
	if !reflect.DeepEqual(original, before) || !reflect.DeepEqual(nodes, before) {
		t.Fatal("reconstruction mutated or reordered real events")
	}
	if twice := restoreMissingTraceUserInputs(sid, got, messages); !reflect.DeepEqual(twice, got) {
		t.Fatal("history reconstruction is not idempotent")
	}
}

func TestRestoreMissingTraceInputsDeclinesAmbiguousAndConflictingHistory(t *testing.T) {
	for _, test := range []struct {
		name           string
		answers        []string
		historyAnswers []string
	}{
		{name: "repeated answer", answers: []string{"done", "done"}, historyAnswers: []string{"done"}},
		{name: "no matching anchor", answers: []string{"live only"}, historyAnswers: []string{"different answer"}},
		{name: "reversed history", answers: []string{"first", "second"}, historyAnswers: []string{"second", "first"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var nodes []session.TracedNode
			for i, answer := range test.answers {
				nodes = append(nodes, session.TracedNode{Event: session.TraceEvent{SessionID: "sid", Turn: i + 1, Kind: "text", Text: answer}})
			}
			var messages []llm.Message
			for i, answer := range test.historyAnswers {
				messages = append(messages, historyTraceMessage(llm.RoleUser, fmt.Sprintf("request %d", i)), historyTraceMessage(llm.RoleAssistant, answer))
			}
			if got := restoreMissingTraceUserInputs("sid", nodes, messages); !reflect.DeepEqual(got, nodes) {
				t.Fatal("ambiguous or conflicting history fabricated human input")
			}
		})
	}
}

func TestRestoreMissingTraceInputsIgnoresChildAnchorsAndRedactsImagePrompt(t *testing.T) {
	canary := "ghp_" + strings.Repeat("A", 36)
	nodes := []session.TracedNode{
		{Event: session.TraceEvent{SessionID: "sid", Turn: 2, Kind: "text", Text: "child only", SubAgentOf: "parent"}, Depth: 1},
		{Event: session.TraceEvent{SessionID: "sid", Turn: 7, Kind: "text", Text: "owner answer"}},
	}
	messages := []llm.Message{
		historyTraceMessage(llm.RoleUser, "unrelated archived question"), historyTraceMessage(llm.RoleAssistant, "child only"),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "<memory-context>derived</memory-context> inspect " + canary}, {Type: "image", Data: "opaque-image-data"}, {Type: "text", Text: "synthetic knowledge", Synthetic: true}}},
		historyTraceMessage(llm.RoleAssistant, "owner answer"),
	}
	got := restoreMissingTraceUserInputs("sid", nodes, messages)
	if len(got) != 3 || got[1].Event.Kind != "user" || got[1].Event.Turn != 7 {
		t.Fatalf("reconstructed image prompt = %+v", got)
	}
	if !strings.Contains(got[1].Event.Text, "[image attachment]") || !strings.Contains(got[1].Event.Text, "[REDACTED]") {
		t.Fatal("reconstructed input lost image or secret redaction")
	}
	for _, forbidden := range []string{canary, "opaque-image-data", "derived", "synthetic knowledge"} {
		if strings.Contains(got[1].Event.Text, forbidden) {
			t.Fatalf("reconstructed input contains excluded %q", forbidden)
		}
	}
}

func TestRestoreMissingTraceInputsDeclinesExplicitCronAndReusedToolArguments(t *testing.T) {
	for _, test := range []struct {
		name     string
		nodes    []session.TracedNode
		messages []llm.Message
	}{
		{
			name: "cron provenance",
			nodes: []session.TracedNode{
				{Event: session.TraceEvent{Turn: 1, Kind: "context", Source: "cron", Text: "scheduled poll"}},
				{Event: session.TraceEvent{Turn: 1, Kind: "text", Text: "cron answer"}},
			},
			messages: []llm.Message{historyTraceMessage(llm.RoleUser, "scheduled poll"), historyTraceMessage(llm.RoleAssistant, "cron answer")},
		},
		{
			name:     "reused provider id with different input",
			nodes:    []session.TracedNode{{Event: session.TraceEvent{Turn: 11, Kind: "tool_start", ToolName: "Read", ToolUseID: "reused-id", Text: `{"path":"/B"}`}}},
			messages: []llm.Message{historyTraceMessage(llm.RoleUser, "old question"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: "Read", ToolUseID: "reused-id", ToolInput: map[string]any{"path": "/A"}}}}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := restoreMissingTraceUserInputs("sid", test.nodes, test.messages); !reflect.DeepEqual(got, test.nodes) {
				t.Fatal("reconstruction inferred a human prompt from automatic or unrelated execution evidence")
			}
		})
	}
}

func TestRestoreMissingTraceInputsPreservesOwnerAcrossAutomaticContinuation(t *testing.T) {
	nodes := []session.TracedNode{
		{Event: session.TraceEvent{Turn: 1, Kind: "text", Text: "initial answer"}},
		{Event: session.TraceEvent{Turn: 2, Kind: "text", Text: "automatic answer"}},
	}
	messages := []llm.Message{
		historyTraceMessage(llm.RoleUser, "<subdirectory_hints>\n<project_context source=\"rules.md\">private project hints</project_context>\n</subdirectory_hints>\nactual human question"),
		historyTraceMessage(llm.RoleAssistant, "initial answer"),
		historyTraceMessage(llm.RoleUser, "<job_notification>finished</job_notification>"),
		historyTraceMessage(llm.RoleAssistant, "automatic answer"),
	}
	got := restoreMissingTraceUserInputs("sid", nodes, messages)
	if len(got) != 3 || got[0].Event.Turn != 1 || got[0].Event.Text != "actual human question" {
		t.Fatal("automatic continuation relocated user input or runtime directory hints appeared as human input")
	}
}

func TestRestoreMissingTraceInputsRecoversOnlySingleUncontestedErrorGap(t *testing.T) {
	messages := []llm.Message{
		historyTraceMessage(llm.RoleUser, "first"), historyTraceMessage(llm.RoleAssistant, "first answer"),
		historyTraceMessage(llm.RoleUser, "failed question"),
		historyTraceMessage(llm.RoleUser, "third"), historyTraceMessage(llm.RoleAssistant, "third answer"),
	}
	for _, kind := range []string{"error", "info", "context"} {
		t.Run(kind, func(t *testing.T) {
			nodes := []session.TracedNode{
				{Event: session.TraceEvent{Turn: 1, Kind: "text", Text: "first answer"}},
				{Event: session.TraceEvent{Turn: 2, Kind: kind, Text: "middle event"}},
				{Event: session.TraceEvent{Turn: 3, Kind: "text", Text: "third answer"}},
			}
			got := restoreMissingTraceUserInputs("sid", nodes, messages)
			middleUsers := 0
			for _, node := range got {
				if node.Event.Turn == 2 && node.Event.Kind == "user" {
					middleUsers++
					if node.Event.Text != "failed question" || !node.Event.TS.IsZero() {
						t.Fatal("error gap reconstructed the wrong prompt or fabricated time")
					}
				}
			}
			if (kind == "error" && middleUsers != 1) || (kind != "error" && middleUsers != 0) {
				t.Fatal("gap reconstruction did not require a single failed execution")
			}
		})
	}
}
