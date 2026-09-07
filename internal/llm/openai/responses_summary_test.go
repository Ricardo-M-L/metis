package openai

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestCodexResponsesSummaryDoesNotRequireEffort(t *testing.T) {
	for _, effort := range []Effort{EffortDefault, EffortHigh} {
		t.Run("effort_"+string(effort), func(t *testing.T) {
			h := newResponsesHarness(t, []string{`{"type":"response.completed","response":{"status":"completed"}}`})
			p := NewCodexResponses("gpt-5.5", 4096, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
				return ResponsesOAuthCredential{AccessToken: "test-token", AccountID: "test-account"}, nil
			})
			p.BaseURL = h.server.URL
			stream, err := p.Stream(context.Background(), provider.Request{Effort: effort})
			if err != nil {
				t.Fatal(err)
			}
			drainStream(t, stream)
			var body struct {
				Reasoning map[string]string `json:"reasoning"`
			}
			if err := json.Unmarshal(h.gotBody, &body); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"summary": "auto"}
			if effort != EffortDefault {
				want["effort"] = string(effort)
			}
			if !reflect.DeepEqual(body.Reasoning, want) {
				t.Fatalf("reasoning = %#v, want %#v", body.Reasoning, want)
			}
		})
	}
}

func TestResponsesSummaryOptInDoesNotChangeCompatibilityRequests(t *testing.T) {
	for _, effort := range []Effort{EffortDefault, EffortHigh} {
		t.Run("effort_"+string(effort), func(t *testing.T) {
			p := NewResponses("test-key", "https://gateway.example/v1", "vendor-model", 4096, time.Second, 0)
			body, err := p.buildResponsesRequest(provider.Request{Effort: effort})
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), `"summary"`) {
				t.Fatalf("unknown compatible route must not opt into summaries: %s", wire)
			}
			if effort == EffortDefault && body.Reasoning != nil {
				t.Fatalf("default compatible request gained reasoning options: %s", wire)
			}
			if effort != EffortDefault && (body.Reasoning == nil || body.Reasoning.Effort != string(effort)) {
				t.Fatalf("explicit reasoning effort changed: %s", wire)
			}
		})
	}
}

func TestResponsesStreamRecoversSummarySnapshotsWithoutDuplicates(t *testing.T) {
	const summaryItem = `{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Plan carefully."}]}`
	const itemDone = `{"type":"response.output_item.done","output_index":0,"item":` + summaryItem + `}`
	const completed = `{"type":"response.completed","response":{"status":"completed","output":[` + summaryItem + `]}}`
	for _, tc := range []struct {
		name   string
		events []string
		want   []string
	}{
		{
			name:   "item_done_only",
			events: []string{itemDone, completed},
			want:   []string{"Plan carefully."},
		},
		{
			name:   "completed_only",
			events: []string{completed},
			want:   []string{"Plan carefully."},
		},
		{
			name: "partial_delta_then_snapshots",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","output_index":0,"summary_index":0,"delta":"Plan "}`,
				itemDone, completed,
			},
			want: []string{"Plan ", "carefully."},
		},
		{
			name: "full_deltas_then_snapshots",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","output_index":0,"summary_index":0,"delta":"Plan "}`,
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","output_index":0,"summary_index":0,"delta":"carefully."}`,
				itemDone, completed,
			},
			want: []string{"Plan ", "carefully."},
		},
		{
			name: "delta_without_id_is_matched_by_output_index",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"Plan "}`,
				itemDone, completed,
			},
			want: []string{"Plan ", "carefully."},
		},
		{
			name: "item_id_matches_without_output_index",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","summary_index":0,"delta":"Plan "}`,
				completed,
			},
			want: []string{"Plan ", "carefully."},
		},
		{
			name: "added_item_binds_id_and_output_index",
			events: []string{
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_plan","type":"reasoning","summary":[]}}`,
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","summary_index":0,"delta":"Plan "}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","summary":[{"type":"summary_text","text":"Plan carefully."}]}}`,
				completed,
			},
			want: []string{"Plan ", "carefully."},
		},
		{
			name: "late_identity_binding_merges_partial_deltas",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","summary_index":0,"delta":"Plan "}`,
				`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"care"}`,
				itemDone, completed,
			},
			want: []string{"Plan ", "care", "fully."},
		},
		{
			name: "snapshots_without_item_id_use_output_index",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"Plan "}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","summary":[{"type":"summary_text","text":"Plan carefully."}]}}`,
				`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"Plan carefully."}]}]}}`,
			},
			want: []string{"Plan ", "carefully."},
		},
		{
			name: "reasoning_text_is_not_summary_progress",
			events: []string{
				`{"type":"response.reasoning_text.delta","item_id":"rs_plan","output_index":0,"delta":"Detailed reasoning."}`,
				completed,
			},
			want: []string{"Detailed reasoning.", "Plan carefully."},
		},
		{
			name: "summary_parts_and_items_have_independent_progress",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_one","output_index":0,"summary_index":0,"delta":"Same."}`,
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_one","output_index":0,"summary_index":1,"delta":"Sec"}`,
				`{"type":"response.completed","response":{"status":"completed","output":[{"id":"rs_one","type":"reasoning","summary":[{"type":"summary_text","text":"Same."},{"type":"summary_text","text":"Second."}]},{"id":"rs_two","type":"reasoning","summary":[{"type":"summary_text","text":"Same."}]}]}}`,
			},
			want: []string{"Same.", "\n\nSec", "ond.", "Same."},
		},
		{
			name: "snapshot_parts_remain_separate_paragraphs",
			events: []string{
				`{"type":"response.completed","response":{"status":"completed","output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Same."},{"type":"summary_text","text":"Same."}]}]}}`,
			},
			want: []string{"Same.", "\n\nSame."},
		},
		{
			name: "completed_snapshot_does_not_append_to_another_summary_part",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","output_index":0,"summary_index":0,"delta":"Plan "}`,
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","output_index":0,"summary_index":1,"delta":"Then test."}`,
				`{"type":"response.completed","response":{"status":"completed","output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Plan carefully."},{"type":"summary_text","text":"Then test."}]}]}}`,
			},
			want: []string{"Plan ", "\n\nThen test."},
		},
		{
			name: "stale_snapshot_does_not_repeat_or_rewrite_live_text",
			events: []string{
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","output_index":0,"summary_index":0,"delta":"Updated plan."}`,
				itemDone, completed,
			},
			want: []string{"Updated plan."},
		},
		{
			name: "empty_and_non_summary_parts_are_not_visible",
			events: []string{
				`{"type":"response.output_item.done","item":{"id":"rs_empty","type":"reasoning","summary":[{"type":"summary_text","text":""},{"type":"other","text":"opaque"}]}}`,
				`{"type":"response.completed","response":{"status":"completed","output":[{"id":"rs_empty","type":"reasoning","summary":[]}]}}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponsesHarness(t, tc.events)
			p := newResponsesClient(h)
			stream, err := p.Stream(context.Background(), provider.Request{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			var terminal int
			for _, event := range drainStream(t, stream) {
				switch event.Type {
				case "thinking_delta":
					if terminal != 0 {
						t.Fatal("summary was emitted after terminal event")
					}
					got = append(got, event.TextDelta)
				case "message_delta":
					terminal++
					if event.StopReason != "end_turn" {
						t.Fatalf("stop reason = %q", event.StopReason)
					}
				case "error":
					t.Fatal(event.Err)
				}
			}
			if !reflect.DeepEqual(got, tc.want) || terminal != 1 {
				t.Fatalf("summary deltas = %#v, want %#v; terminal events = %d", got, tc.want, terminal)
			}
		})
	}
}

func TestResponsesStreamSummaryFallbackPreservesTerminalSemantics(t *testing.T) {
	for _, tc := range []struct {
		name        string
		terminal    string
		wantSummary string
		wantStop    string
		wantError   bool
	}{
		{"tool_call", `{"type":"response.completed","response":{"status":"completed","output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Plan."}]},{"id":"fc_plan","type":"function_call","call_id":"call_plan","name":"Read","arguments":"{}"}],"usage":{"input_tokens":4,"output_tokens":2}}}`, "Plan.", "tool_use", false},
		{"done_alias", `{"type":"response.done","response":{"status":"completed","output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Plan."}]}],"usage":{"input_tokens":4,"output_tokens":2}}}`, "Plan.", "end_turn", false},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Plan."}]}],"usage":{"input_tokens":4,"output_tokens":2}}}`, "Plan.", "max_tokens", false},
		{"content_filter", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Plan."}]}],"usage":{"input_tokens":4,"output_tokens":2}}}`, "Plan.", "content_filter", false},
		{"failed", `{"type":"response.failed","response":{"status":"failed","error":{"message":"upstream failed"},"output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Do not recover failed output."}]}]}}`, "", "", true},
		{"cancelled", `{"type":"response.done","response":{"status":"cancelled","output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Do not recover cancelled output."}]}]}}`, "", "", true},
		{"unknown_status", `{"type":"response.done","response":{"status":"unknown","output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Do not recover unknown output."}]}]}}`, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponsesHarness(t, []string{tc.terminal})
			p := newResponsesClient(h)
			stream, err := p.Stream(context.Background(), provider.Request{})
			if err != nil {
				t.Fatal(err)
			}
			var summary, stop string
			var gotError bool
			for _, event := range drainStream(t, stream) {
				switch event.Type {
				case "thinking_delta":
					summary += event.TextDelta
				case "message_delta":
					stop = event.StopReason
					if event.InputTokens != 4 || event.OutputTokens != 2 {
						t.Fatalf("terminal usage changed: %#v", event)
					}
				case "error":
					gotError = true
				}
			}
			if summary != tc.wantSummary || stop != tc.wantStop || gotError != tc.wantError {
				t.Fatalf("summary=%q stop=%q error=%v; want %q, %q, %v", summary, stop, gotError, tc.wantSummary, tc.wantStop, tc.wantError)
			}
		})
	}
}

func TestResponsesStreamSummaryFallbackPreservesEncryptedReasoningAndTools(t *testing.T) {
	const reasoning = `{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Plan carefully."}],"encrypted_content":"test-ciphertext"}`
	const tool = `{"id":"fc_plan","type":"function_call","call_id":"call_plan","name":"Read","arguments":"{}"}`
	h := newResponsesHarness(t, []string{
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_plan","output_index":0,"summary_index":0,"delta":"Plan "}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}`,
		`{"type":"response.output_item.added","output_index":1,"item":` + tool + `}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_plan","delta":"{}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":` + tool + `}`,
		`{"type":"response.completed","response":{"id":"resp_plan","status":"completed","output":[` + reasoning + `,` + tool + `]}}`,
	})
	p := newResponsesClient(h)
	p.StateMode = ResponsesStateProvider
	stream, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var summary string
	for _, event := range drainStream(t, stream) {
		types = append(types, event.Type)
		switch event.Type {
		case "thinking_delta":
			summary += event.TextDelta
		case "redacted_thinking":
			if event.TextDelta != "test-ciphertext" || event.ProviderHint[responsesHintItemID] != "rs_plan" || event.ProviderHint[responsesHintReasoningSummary] != `[{"type":"summary_text","text":"Plan carefully."}]` {
				t.Fatalf("encrypted replay metadata changed: %#v", event)
			}
		case "tool_use_start", "tool_input_delta", "tool_use_stop":
			if event.ToolUseID != "call_plan" {
				t.Fatalf("tool call ID mapping changed: %#v", event)
			}
			if event.Type != "tool_use_start" && event.InputDelta != "{}" {
				t.Fatalf("tool arguments changed: %#v", event)
			}
		case "message_delta":
			if event.StopReason != "tool_use" {
				t.Fatalf("stop reason = %q", event.StopReason)
			}
		}
	}
	wantTypes := []string{"thinking_delta", "thinking_delta", "redacted_thinking", "tool_use_start", "tool_input_delta", "tool_use_stop", "provider_state", "message_delta"}
	if !reflect.DeepEqual(types, wantTypes) || summary != "Plan carefully." {
		t.Fatalf("events = %v, want %v; public summary = %q", types, wantTypes, summary)
	}
}

func TestCodexResponsesCompleteRecoversTerminalSummary(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"rs_plan","type":"reasoning","summary":[{"type":"summary_text","text":"Plan carefully."}]}]}}`,
	})
	p := NewCodexResponses("gpt-5.5", 4096, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
		return ResponsesOAuthCredential{AccessToken: "test-token", AccountID: "test-account"}, nil
	})
	p.BaseURL = h.server.URL
	response, err := p.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "thinking" || response.Content[0].Text != "Plan carefully." {
		t.Fatalf("content = %#v, want one public summary", response.Content)
	}
}
