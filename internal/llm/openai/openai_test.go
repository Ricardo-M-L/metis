package openai

import (
	"io"
	"strings"
	"testing"
)

// makeStream wraps a raw SSE payload in a stream reader for testing.
func makeStream(payload string) *openAIStream {
	return newOpenAIStream(io.NopCloser(strings.NewReader(payload)))
}

func TestOpenAIStream_PlainText(t *testing.T) {
	// Two-chunk standard OpenAI streaming response.
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello","role":"assistant"},"index":0}]}`,
		``,
		`data: {"choices":[{"delta":{"content":" world"},"index":0}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	want := []struct {
		typ  string
		text string
		stop string
	}{
		{"text_delta", "Hello", ""},
		{"text_delta", " world", ""},
		{"message_delta", "", "end_turn"},
		{"message_stop", "", ""},
	}
	for i, w := range want {
		ev, err := s.Recv()
		if i == len(want)-1 {
			if err != io.EOF {
				t.Fatalf("event %d: want EOF, got err=%v", i, err)
			}
		} else if err != nil {
			t.Fatalf("event %d: unexpected err %v", i, err)
		}
		if ev.Type != w.typ {
			t.Errorf("event %d: type=%q want %q", i, ev.Type, w.typ)
		}
		if ev.TextDelta != w.text {
			t.Errorf("event %d: text=%q want %q", i, ev.TextDelta, w.text)
		}
		if ev.StopReason != w.stop {
			t.Errorf("event %d: stop=%q want %q", i, ev.StopReason, w.stop)
		}
	}
}

func TestOpenAIStream_GeminiBundledChunk(t *testing.T) {
	// Reproduces the bug fixed earlier: Gemini's OpenAI-compat layer
	// puts content + finish_reason + usage in a single chunk.
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"pong","role":"assistant"},"finish_reason":"stop","index":0}],"usage":{"completion_tokens":1,"prompt_tokens":7,"total_tokens":8}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	// Expect: text_delta "pong", then message_delta with stop+usage, then message_stop.
	ev, err := s.Recv()
	if err != nil || ev.Type != "text_delta" || ev.TextDelta != "pong" {
		t.Fatalf("event 1: want text_delta pong, got %+v err=%v", ev, err)
	}
	ev, err = s.Recv()
	if err != nil || ev.Type != "message_delta" || ev.StopReason != "end_turn" {
		t.Fatalf("event 2: want message_delta end_turn, got %+v err=%v", ev, err)
	}
	if ev.OutputTokens != 1 || ev.InputTokens != 7 {
		t.Errorf("event 2: want tokens in=7 out=1, got in=%d out=%d", ev.InputTokens, ev.OutputTokens)
	}
	ev, err = s.Recv()
	if err != io.EOF || ev.Type != "message_stop" {
		t.Fatalf("event 3: want message_stop EOF, got %+v err=%v", ev, err)
	}
}

func TestOpenAIStream_ToolCall(t *testing.T) {
	// A single tool call streamed across multiple chunks.
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"LS"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	mustNext := func(typ string) StreamEvent {
		ev, err := s.Recv()
		if err != nil && err != io.EOF {
			t.Fatalf("recv err: %v", err)
		}
		if ev.Type != typ {
			t.Fatalf("want %q got %q", typ, ev.Type)
		}
		return ev
	}
	mustNext("tool_use_start")
	mustNext("tool_input_delta")
	mustNext("tool_input_delta")
	stop := mustNext("tool_use_stop")
	if stop.InputDelta != `{"path":"/tmp"}` {
		t.Errorf("accumulated input json = %q, want %q", stop.InputDelta, `{"path":"/tmp"}`)
	}
	mustNext("message_delta")
	mustNext("message_stop")
}

func TestOpenAIStream_ToolCallWithoutIndexOrIDKeepsOneIdentity(t *testing.T) {
	for _, startID := range []string{"", "call_name_only_id"} {
		name := "fully_anonymous"
		idField := ""
		if startID != "" {
			name = "id_only_on_name_frame"
			idField = `"id":"` + startID + `",`
		}
		t.Run(name, func(t *testing.T) {
			payload := strings.Join([]string{
				`data: {"choices":[{"delta":{"tool_calls":[{` + idField + `"function":{"name":"Read"}}]}}]}`,
				``,
				`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"path\":"}}]}}]}`,
				``,
				`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"/tmp/a.go\"}"}}]}}]}`,
				``,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n")
			s := makeStream(payload)
			defer s.Close()

			var starts, deltas, stops []StreamEvent
			for {
				ev, err := s.Recv()
				switch ev.Type {
				case "tool_use_start":
					starts = append(starts, ev)
				case "tool_input_delta":
					deltas = append(deltas, ev)
				case "tool_use_stop":
					stops = append(stops, ev)
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(starts) != 1 || len(deltas) != 2 || len(stops) != 1 {
				t.Fatalf("starts=%+v deltas=%+v stops=%+v", starts, deltas, stops)
			}
			id := starts[0].ToolUseID
			if id == "" || deltas[0].ToolUseID != id || deltas[1].ToolUseID != id || stops[0].ToolUseID != id {
				t.Fatalf("anonymous call identity changed: starts=%+v deltas=%+v stops=%+v", starts, deltas, stops)
			}
			if startID != "" && id != startID {
				t.Fatalf("provider id changed: got %q want %q", id, startID)
			}
			if stops[0].InputDelta != `{"path":"/tmp/a.go"}` {
				t.Fatalf("accumulated arguments = %q", stops[0].InputDelta)
			}
		})
	}
}

func TestOpenAIStream_IDThenIndexReconcilesToOriginalCall(t *testing.T) {
	for _, includeIDOnIndexedFrame := range []bool{true, false} {
		name := "index_only_later"
		indexedDelta := `{"index":7,"function":{"arguments":"{\"path\":\"/tmp/a.go\"}"}}`
		if includeIDOnIndexedFrame {
			name = "id_and_index_later"
			indexedDelta = `{"index":7,"id":"call_reconcile","function":{"arguments":"{\"path\":\"/tmp/a.go\"}"}}`
		}
		t.Run(name, func(t *testing.T) {
			payload := strings.Join([]string{
				`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_reconcile","function":{"name":"Read"}}]}}]}`,
				``,
				`data: {"choices":[{"delta":{"tool_calls":[` + indexedDelta + `]}}]}`,
				``,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				``,
				`data: [DONE]`,
				``,
			}, "\n")
			s := makeStream(payload)
			defer s.Close()

			var starts, stops []StreamEvent
			for {
				ev, err := s.Recv()
				if ev.Type == "tool_use_start" {
					starts = append(starts, ev)
				}
				if ev.Type == "tool_use_stop" {
					stops = append(stops, ev)
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(starts) != 1 || len(stops) != 1 {
				t.Fatalf("call was split: starts=%+v stops=%+v", starts, stops)
			}
			if starts[0].ToolUseID != "call_reconcile" || stops[0].ToolUseID != "call_reconcile" {
				t.Fatalf("id was not preserved: starts=%+v stops=%+v", starts, stops)
			}
			if stops[0].InputDelta != `{"path":"/tmp/a.go"}` {
				t.Fatalf("arguments were not reconciled: %q", stops[0].InputDelta)
			}
		})
	}
}

// Regression: when the upstream omits `index` (e.g. some Groq/Together
// streams), the previous code defaulted every parallel call to index 0
// and merged their arguments. We now route each call to its own slot
// using the call id.
func TestOpenAIStream_ParallelToolCallsWithoutIndex(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_a","function":{"name":"LS"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"name":"Glob"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"arguments":"{\"a\":1}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"arguments":"{\"b\":2}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	stops := map[string]string{}
	for {
		ev, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == "tool_use_stop" {
			stops[ev.ToolUseID] = ev.InputDelta
		}
	}
	if got := stops["call_a"]; got != `{"a":1}` {
		t.Errorf("call_a accumulated = %q, want {\"a\":1}", got)
	}
	if got := stops["call_b"]; got != `{"b":2}` {
		t.Errorf("call_b accumulated = %q, want {\"b\":2}", got)
	}
}

// Some OpenAI-compatible providers retain the standard index field but omit
// tool_call.id entirely. The adapter must manufacture a stable id per index;
// otherwise consumeStream sees every call under the empty-string map key and
// interleaved arguments overwrite one another.
func TestOpenAIStream_ParallelToolCallsWithoutIDs(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Read","arguments":"{\"path\":\"/tmp/"}},{"index":1,"function":{"name":"Grep","arguments":"{\"pattern\":\"TO"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"DO\"}"}},{"index":0,"function":{"arguments":"a.go\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	var starts, stops []StreamEvent
	for {
		ev, err := s.Recv()
		switch ev.Type {
		case "tool_use_start":
			starts = append(starts, ev)
		case "tool_use_stop":
			stops = append(stops, ev)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(starts) != 2 || len(stops) != 2 {
		t.Fatalf("starts=%+v stops=%+v; want two of each", starts, stops)
	}
	if starts[0].ToolUseID == "" || starts[1].ToolUseID == "" {
		t.Fatalf("synthetic ids must be non-empty: %+v", starts)
	}
	if starts[0].ToolUseID == starts[1].ToolUseID {
		t.Fatalf("parallel calls reused synthetic id %q", starts[0].ToolUseID)
	}
	if stops[0].ToolUseID != starts[0].ToolUseID || stops[1].ToolUseID != starts[1].ToolUseID {
		t.Fatalf("stop ids did not retain start identity: starts=%+v stops=%+v", starts, stops)
	}
	if stops[0].InputDelta != `{"path":"/tmp/a.go"}` {
		t.Errorf("Read args crossed streams: %q", stops[0].InputDelta)
	}
	if stops[1].InputDelta != `{"pattern":"TODO"}` {
		t.Errorf("Grep args crossed streams: %q", stops[1].InputDelta)
	}
}

// Stops must be deterministic and follow tool start order. consumeStream
// reserves assistant content blocks at start time, so stable stop order also
// makes raw adapter traces reproducible instead of depending on Go map order.
func TestOpenAIStream_ParallelToolStopsKeepStartOrder(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"Read","arguments":"{\"path\":\"/tmp/a\"}"}},{"index":1,"id":"call_b","function":{"name":"Read","arguments":"{\"path\":\"/tmp/b\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	var starts, stops []string
	for {
		ev, err := s.Recv()
		if ev.Type == "tool_use_start" {
			starts = append(starts, ev.ToolUseID)
		}
		if ev.Type == "tool_use_stop" {
			stops = append(stops, ev.ToolUseID)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(starts, ",") != "call_a,call_b" {
		t.Fatalf("start order = %v", starts)
	}
	if strings.Join(stops, ",") != "call_a,call_b" {
		t.Fatalf("stop order = %v", stops)
	}
}

func TestFromOpenAIChoice_MissingToolIDsGetResponseScopedIDs(t *testing.T) {
	choice := oaiChoice{FinishReason: "tool_calls"}
	choice.Message.ToolCalls = make([]oaiToolCall, 2)
	choice.Message.ToolCalls[0].Function.Name = "Read"
	choice.Message.ToolCalls[0].Function.Arguments = `{"path":"/tmp/a"}`
	choice.Message.ToolCalls[1].Function.Name = "Grep"
	choice.Message.ToolCalls[1].Function.Arguments = `{"pattern":"TODO"}`

	first := fromOpenAIChoice(choice, oaiUsage{})
	second := fromOpenAIChoice(choice, oaiUsage{})
	if len(first.Content) != 2 || len(second.Content) != 2 {
		t.Fatalf("unexpected content: first=%+v second=%+v", first.Content, second.Content)
	}
	firstA, firstB := first.Content[0].ToolUseID, first.Content[1].ToolUseID
	if firstA == "" || firstB == "" || firstA == firstB {
		t.Fatalf("missing/non-unique synthetic ids: %q %q", firstA, firstB)
	}
	if firstA == second.Content[0].ToolUseID || firstB == second.Content[1].ToolUseID {
		t.Fatalf("identical responses reused history-visible ids: first=%+v second=%+v", first.Content, second.Content)
	}
}

func TestOpenAIStream_SyntheticIDsDoNotDependOnProcessSequence(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Read","arguments":"{\"path\":\"/tmp/a\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	readID := func() string {
		s := makeStream(payload)
		defer s.Close()
		for {
			ev, err := s.Recv()
			if ev.Type == "tool_use_start" {
				return ev.ToolUseID
			}
			if err != nil {
				t.Fatalf("did not receive tool start: %v", err)
			}
		}
	}
	first, second := readID(), readID()
	if first == "" || second == "" || first == second {
		t.Fatalf("response-scoped synthetic ids collided: %q %q", first, second)
	}
}

// TestToOpenAI_ImageContentBlock — pasted-image flow. The user turn
// carries one text + one image block; toOpenAI must emit a
// content-parts array (not a string) with `{"type":"image_url",
// "image_url":{"url":"data:image/png;base64,..."}}`. OpenAI / Kimi
// (Moonshot) / DeepSeek-Chat-V3-vision all parse this shape.
func TestToOpenAI_ImageContentBlock(t *testing.T) {
	req := Request{
		Messages: []Message{{
			Role: RoleUser,
			Content: []ContentBlock{
				{Type: "text", Text: "What's this?"},
				{Type: "image", MediaType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUg=="},
			},
		}},
	}
	out := toOpenAI(req, "gpt-4o", 1024)
	if len(out.Messages) != 1 {
		t.Fatalf("message count: got %d", len(out.Messages))
	}
	m := out.Messages[0]
	parts, ok := m.Content.([]oaiContentPart)
	if !ok {
		t.Fatalf("expected Content to be []oaiContentPart for image-bearing turn; got %T", m.Content)
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 content parts (text + image_url), got %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "What's this?" {
		t.Errorf("text part: got %+v", parts[0])
	}
	if parts[1].Type != "image_url" {
		t.Errorf("image part Type: %q", parts[1].Type)
	}
	if parts[1].ImageURL == nil {
		t.Fatal("image part missing ImageURL")
	}
	if parts[1].ImageURL.URL != "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==" {
		t.Errorf("data URL not assembled correctly: %q", parts[1].ImageURL.URL)
	}
}

// TestToOpenAI_TextOnlyStaysString — backward compat: turns without
// images keep the historical string-content shape (every provider
// accepts it; no need to bump every request to the array form).
func TestToOpenAI_TextOnlyStaysString(t *testing.T) {
	req := Request{
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: "text", Text: "hello"}},
		}},
	}
	out := toOpenAI(req, "gpt-4o-mini", 512)
	if len(out.Messages) != 1 {
		t.Fatalf("message count")
	}
	if str, ok := out.Messages[0].Content.(string); !ok || str != "hello" {
		t.Errorf("text-only turn should stay as string content; got %T %v", out.Messages[0].Content, out.Messages[0].Content)
	}
}
