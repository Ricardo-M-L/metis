package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestLoopPersistedToolTraceCallIDsMatchEventsWithReusedProviderID(t *testing.T) {
	for _, test := range []struct {
		name     string
		planMode bool
		calls    []llm.ContentBlock
		want     []string
	}{
		{
			name: "normal batch",
			calls: []llm.ContentBlock{
				{Type: "tool_use", ToolUseID: "duplicate", ToolName: "Read"},
				{Type: "tool_use", ToolUseID: "duplicate", ToolName: "Write"},
			},
			want: []string{"Read ok", "Write ok"},
		},
		{
			name:     "plan split changes execution order",
			planMode: true,
			calls: []llm.ContentBlock{
				{Type: "tool_use", ToolUseID: "duplicate", ToolName: "Write"},
				{Type: "tool_use", ToolUseID: "duplicate", ToolName: "ExitPlanMode"},
			},
			want: []string{"skipped: ExitPlanMode is an approval boundary", "ExitPlanMode ok"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &planModeStreamProvider{streams: [][]llm.StreamEvent{toolBatchEvents(test.calls...)}}
			gate := permission.New(permission.ModeBypass)
			if test.planMode {
				gate.SetMode(permission.ModePlan)
			}
			registry := tools.NewRegistry()
			registry.Register(planBatchTestTool{name: "Read", gate: gate})
			registry.Register(planBatchTestTool{name: "Write", gate: gate})
			registry.Register(planBatchTestTool{name: "ExitPlanMode", gate: gate, exitMode: permission.ModeBypass})
			loop := NewLoop(provider, registry, gate, nil, "system", 5)
			loop.SetPlanMode(test.planMode)
			loop.AppendUser("run duplicate provider IDs")
			out := make(chan Event, 128)
			if err := loop.Run(context.Background(), out); err != nil {
				t.Fatalf("Run: %v", err)
			}
			close(out)

			var uses, results []llm.ContentBlock
			for _, msg := range loop.Messages {
				for _, block := range msg.Content {
					switch block.Type {
					case "tool_use":
						uses = append(uses, block)
					case "tool_result":
						results = append(results, block)
					}
				}
			}
			if len(uses) != len(test.calls) || len(results) != len(test.calls) {
				t.Fatalf("uses=%+v results=%+v", uses, results)
			}
			starts := map[string]Event{}
			ends := map[string]Event{}
			for ev := range out {
				switch ev.Kind {
				case EventToolStart:
					starts[ev.TraceCallID] = ev
				case EventToolResult:
					ends[ev.TraceCallID] = ev
				}
			}
			if len(starts) != len(test.calls) || len(ends) != len(test.calls) {
				t.Fatalf("starts=%+v ends=%+v", starts, ends)
			}
			seen := map[string]bool{}
			for i := range uses {
				use, result := uses[i], results[i]
				if use.ToolUseID != "duplicate" || result.ToolUseID != "duplicate" {
					t.Fatalf("provider wire IDs changed: use=%+v result=%+v", use, result)
				}
				if use.TraceCallID == "" || seen[use.TraceCallID] || result.TraceCallID != use.TraceCallID {
					t.Fatalf("trace pairing wrong: use=%+v result=%+v", use, result)
				}
				seen[use.TraceCallID] = true
				if starts[use.TraceCallID].ToolName != use.ToolName || ends[use.TraceCallID].ToolName != use.ToolName {
					t.Fatalf("events do not match persisted call %+v: start=%+v result=%+v", use, starts[use.TraceCallID], ends[use.TraceCallID])
				}
				if got := result.ToolResult; len(got) < len(test.want[i]) || got[:len(test.want[i])] != test.want[i] {
					t.Fatalf("result[%d]=%q, want prefix %q", i, got, test.want[i])
				}
			}
			raw, err := json.Marshal(loop.Messages)
			if err != nil {
				t.Fatal(err)
			}
			var restored []llm.Message
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			var restoredKeys []string
			for _, message := range restored {
				for _, block := range message.Content {
					if block.Type == "tool_use" || block.Type == "tool_result" {
						restoredKeys = append(restoredKeys, block.TraceCallID)
					}
				}
			}
			if len(restoredKeys) != 2*len(uses) {
				t.Fatalf("JSON history dropped tool blocks: %s", raw)
			}
			for i := range uses {
				if restoredKeys[i] != uses[i].TraceCallID || restoredKeys[i+len(uses)] != uses[i].TraceCallID {
					t.Fatalf("JSON history lost occurrence pairing: %v", restoredKeys)
				}
			}
		})
	}
}

func TestStreamedToolTraceCallIDStableFromArgsToResult(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(planBatchTestTool{name: "Read"})
	loop := NewLoop(nil, registry, permission.New(permission.ModeBypass), nil, "system", 1)
	out := make(chan Event, 16)
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "tool_use_start", ToolUseID: "same", ToolName: "Read"},
		{Type: "tool_input_delta", ToolUseID: "same", InputDelta: `{}`},
		{Type: "tool_use_stop", ToolUseID: "same"},
		{Type: "message_stop", StopReason: "tool_use"},
	}}
	blocks, _, _, err := loop.consumeStream(context.Background(), stream, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].TraceCallID == "" {
		t.Fatalf("streamed tool occurrence key missing: %+v", blocks)
	}
	results, err := loop.executeBatch(context.Background(), blocks, out, HookContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].TraceCallID != blocks[0].TraceCallID {
		t.Fatalf("result key drifted: tool=%+v result=%+v", blocks, results)
	}
	close(out)
	seen := map[EventKind]bool{}
	for ev := range out {
		if ev.Kind != EventToolArgsDelta && ev.Kind != EventToolStart && ev.Kind != EventToolResult {
			continue
		}
		seen[ev.Kind] = true
		if ev.TraceCallID != blocks[0].TraceCallID {
			t.Fatalf("event key drifted: %+v, tool=%+v", ev, blocks[0])
		}
	}
	for _, kind := range []EventKind{EventToolArgsDelta, EventToolStart, EventToolResult} {
		if !seen[kind] {
			t.Fatalf("missing %v event", kind)
		}
	}
}

func TestRepairOrphanedToolUsesRetainsTraceOccurrence(t *testing.T) {
	history := []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "duplicate", TraceCallID: "call-first", ToolName: "Read"},
		{Type: "tool_use", ToolUseID: "duplicate", TraceCallID: "call-second", ToolName: "Read"},
	}}}
	// A keyed result may arrive for the second occurrence first. Repair must
	// synthesize a result for the first, carrying its exact occurrence key.
	history = append(history, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Type: "tool_result", ToolUseID: "duplicate", TraceCallID: "call-second", ToolResult: "done",
	}}})
	repaired := RepairOrphanedToolUses(history)
	if len(repaired) != 3 || len(repaired[2].Content) != 1 || repaired[2].Content[0].TraceCallID != "call-first" {
		t.Fatalf("repair lost occurrence identity: %+v", repaired)
	}
	if twice := RepairOrphanedToolUses(repaired); len(twice) != len(repaired) {
		t.Fatalf("repair repeated after keyed compensation: %+v", twice)
	}
}
