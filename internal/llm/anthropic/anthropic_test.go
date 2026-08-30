package anthropic

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func makeAnthStream(payload string) *anthropicStream {
	return newAnthropicStream(io.NopCloser(strings.NewReader(payload)))
}

func TestAnthropicStream_TextOnly(t *testing.T) {
	payload := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	s := makeAnthStream(payload)
	defer s.Close()

	expected := []struct {
		typ  string
		text string
	}{
		{"message_start", ""},
		{"text_delta", "Hello"},
		{"text_delta", " world"},
		{"message_delta", ""},
		{"message_stop", ""},
	}
	for i, e := range expected {
		ev, err := s.Recv()
		if i == len(expected)-1 {
			if err != io.EOF {
				t.Fatalf("event %d: want EOF, got %v", i, err)
			}
		} else if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if ev.Type != e.typ {
			t.Errorf("event %d: type=%q want %q", i, ev.Type, e.typ)
		}
		if ev.TextDelta != e.text {
			t.Errorf("event %d: text=%q want %q", i, ev.TextDelta, e.text)
		}
	}
}

func TestAnthropicStream_ToolUse(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"LS","input":{}}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"/tmp\"}"}}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	s := makeAnthStream(payload)
	defer s.Close()

	must := func(typ string) StreamEvent {
		ev, err := s.Recv()
		if err != nil && err != io.EOF {
			t.Fatalf("recv err: %v", err)
		}
		if ev.Type != typ {
			t.Fatalf("want %q got %q", typ, ev.Type)
		}
		return ev
	}
	start := must("tool_use_start")
	if start.ToolName != "LS" || start.ToolUseID != "toolu_1" {
		t.Errorf("start: name=%q id=%q", start.ToolName, start.ToolUseID)
	}
	must("tool_input_delta")
	must("tool_input_delta")
	stop := must("tool_use_stop")
	if stop.InputDelta != `{"path":"/tmp"}` {
		t.Errorf("input json = %q", stop.InputDelta)
	}
	must("message_delta")
	must("message_stop")
}

// --- prompt cache tests (Task #63) -----------------------------------------

// TestBuildSystemBlocks_NoBoundary — when caller passes a plain system
// string with no boundary marker, we still emit a single block (no
// cache_control on it because the last-tool marker already creates a
// breakpoint covering tools+system).
func TestBuildSystemBlocks_NoBoundary(t *testing.T) {
	got := buildSystemBlocks("you are an agent")
	if len(got) != 1 {
		t.Fatalf("expected 1 block; got %d (%+v)", len(got), got)
	}
	if got[0].Text != "you are an agent" {
		t.Errorf("text mismatch: %q", got[0].Text)
	}
	if got[0].CacheControl != nil {
		t.Errorf("single-block path must not set cache_control (the last-tool marker covers it); got %+v", got[0].CacheControl)
	}
}

// TestBuildSystemBlocks_WithBoundary — boundary present → 2-block split
// where ONLY the static prefix carries cache_control. Dynamic suffix
// (env/cwd/date) sits outside the cache so per-call updates don't
// invalidate the cached prefix.
func TestBuildSystemBlocks_WithBoundary(t *testing.T) {
	prompt := "STATIC IDENTITY\n\n" + SystemPromptCacheBoundary + "\n\n<env>cwd=/tmp</env>"
	got := buildSystemBlocks(prompt)
	if len(got) != 2 {
		t.Fatalf("expected 2 blocks (static + dynamic); got %d", len(got))
	}
	if got[0].Text != "STATIC IDENTITY" {
		t.Errorf("static block text mismatch: %q", got[0].Text)
	}
	if got[0].CacheControl == nil || got[0].CacheControl.Type != "ephemeral" {
		t.Errorf("static block must carry cache_control{type:ephemeral}; got %+v", got[0].CacheControl)
	}
	if got[1].Text != "<env>cwd=/tmp</env>" {
		t.Errorf("dynamic block text mismatch: %q", got[1].Text)
	}
	if got[1].CacheControl != nil {
		t.Errorf("dynamic block must NOT have cache_control (it changes per call); got %+v", got[1].CacheControl)
	}
}

// TestBuildSystemBlocks_Empty — empty string returns nil so omitempty
// drops the field, matching pre-cache-control behavior.
func TestBuildSystemBlocks_Empty(t *testing.T) {
	if got := buildSystemBlocks(""); got != nil {
		t.Errorf("empty input must return nil; got %+v", got)
	}
}

// TestToAnthropic_LastToolGetsCacheControl — the highest-leverage cache
// breakpoint is the tool array (typically 5–10K tokens of descriptions
// + JSON schemas across 30+ tools). Marking the LAST tool caches the
// entire array. Verify both that the marker is on the last tool AND
// that earlier tools have no marker (only one breakpoint per "level").
func TestToAnthropic_LastToolGetsCacheControl(t *testing.T) {
	req := Request{
		System: "test",
		Tools: []ToolSpec{
			{Name: "Read", Description: "read file", InputSchema: map[string]any{}},
			{Name: "Edit", Description: "edit file", InputSchema: map[string]any{}},
			{Name: "Bash", Description: "run shell", InputSchema: map[string]any{}},
		},
	}
	out := toAnthropic(req, "claude-sonnet-4", 1024)
	if len(out.Tools) != 3 {
		t.Fatalf("tool count mismatch: got %d", len(out.Tools))
	}
	for i := 0; i < 2; i++ {
		if out.Tools[i].CacheControl != nil {
			t.Errorf("tool[%d] (%s) must NOT have cache_control — only last tool gets the marker; got %+v",
				i, out.Tools[i].Name, out.Tools[i].CacheControl)
		}
	}
	last := out.Tools[2]
	if last.CacheControl == nil || last.CacheControl.Type != "ephemeral" {
		t.Errorf("last tool (%s) must carry cache_control{type:ephemeral}; got %+v", last.Name, last.CacheControl)
	}
}

func TestToAnthropic_ExposureMarksStableDirectPrefix(t *testing.T) {
	req := Request{
		System: "test",
		Tools: []ToolSpec{
			{Name: "Bash", Exposure: "direct", InputSchema: map[string]any{}},
			{Name: "Read", Exposure: "direct", InputSchema: map[string]any{}},
			{Name: "mcp__docs__query", Exposure: "deferred", InputSchema: map[string]any{}},
			{Name: "ToolSearch", Exposure: "deferred", InputSchema: map[string]any{}},
		},
	}
	out := toAnthropic(req, "claude-sonnet-4", 1024)
	if len(out.Tools) != 4 {
		t.Fatalf("tool count mismatch: got %d", len(out.Tools))
	}
	if out.Tools[0].CacheControl != nil {
		t.Fatalf("first direct tool must not carry boundary: %+v", out.Tools[0].CacheControl)
	}
	if out.Tools[1].CacheControl == nil || out.Tools[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("last direct tool must carry stable-prefix boundary: %+v", out.Tools[1].CacheControl)
	}
	for i := 2; i < len(out.Tools); i++ {
		if out.Tools[i].CacheControl != nil {
			t.Fatalf("deferred tool %q must stay after the cache boundary", out.Tools[i].Name)
		}
	}
}

func TestToAnthropic_AllDeferredToolsHaveNoToolCacheBoundary(t *testing.T) {
	req := Request{
		System: "test",
		Tools: []ToolSpec{
			{Name: "mcp__docs__query", Exposure: "deferred", InputSchema: map[string]any{}},
			{Name: "ToolSearch", Exposure: "deferred", InputSchema: map[string]any{}},
		},
	}
	out := toAnthropic(req, "claude-sonnet-4", 1024)
	for i, tool := range out.Tools {
		if tool.CacheControl != nil {
			t.Fatalf("all-deferred tool[%d] %q must not define an unstable cache boundary", i, tool.Name)
		}
	}
}

// TestToAnthropic_NoToolsNoCacheMarker — zero tools means no cache
// breakpoint on the tools level. The system block path still works
// (verified above) but we must not crash.
func TestToAnthropic_NoToolsNoCacheMarker(t *testing.T) {
	req := Request{System: "no tools"}
	out := toAnthropic(req, "claude-sonnet-4", 1024)
	if len(out.Tools) != 0 {
		t.Errorf("expected 0 tools; got %d", len(out.Tools))
	}
	// No panic = pass.
}

// --- JSON wire-format tests (Task #63 verification补充) ---

// TestToAnthropic_JSONHasCacheControl_SystemAndLastTool — round-trip
// the request struct through JSON marshal and assert the bytes
// contain `"cache_control":{"type":"ephemeral"}` at exactly two
// positions: the static system block + the last tool. Anything else
// would silently drop the cache markers on the wire.
func TestToAnthropic_JSONHasCacheControl_SystemAndLastTool(t *testing.T) {
	sys := "STATIC IDENTITY HERE\n\n" + SystemPromptCacheBoundary + "\n\n<env>cwd=/tmp</env>"
	req := Request{
		System: sys,
		Tools: []ToolSpec{
			{Name: "Read", Description: "read file", InputSchema: map[string]any{"type": "object"}},
			{Name: "Edit", Description: "edit file", InputSchema: map[string]any{"type": "object"}},
			{Name: "Bash", Description: "run shell", InputSchema: map[string]any{"type": "object"}},
		},
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: "text", Text: "hi"}},
		}},
	}
	out := toAnthropic(req, "claude-sonnet-4", 1024)

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	cacheMarkerCount := strings.Count(body, `"cache_control":{"type":"ephemeral"}`)
	// 3 markers expected post CC-B: static system + last tool + last user message.
	// (Not 4 yet because the test fixture has no addendum / boundary 2.)
	if cacheMarkerCount != 3 {
		t.Fatalf("expected exactly 3 cache_control markers (static system + last tool + last user msg); got %d in:\n%s",
			cacheMarkerCount, body)
	}
	// System must serialize as an array of blocks (not bare string).
	if !strings.Contains(body, `"system":[{"type":"text"`) {
		t.Errorf("system field must serialize as array of blocks; got:\n%s", body)
	}
	// Last tool must be the one carrying cache_control. Bash is last in
	// the input slice; assert its tool entry is the marked one.
	bashIdx := strings.Index(body, `"name":"Bash"`)
	editIdx := strings.Index(body, `"name":"Edit"`)
	if bashIdx < editIdx || bashIdx < 0 {
		t.Fatalf("Bash tool not found / out of order in JSON")
	}
	// The marker between Bash entry start and request end must exist.
	tail := body[bashIdx:]
	if !strings.Contains(tail, `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("Bash (last tool) must carry cache_control in JSON tail; got:\n%s", tail)
	}
	// Confirm Edit (NOT last) does NOT carry cache_control.
	editTail := body[editIdx:bashIdx]
	if strings.Contains(editTail, `"cache_control"`) {
		t.Errorf("Edit (not last tool) must NOT carry cache_control; got:\n%s", editTail)
	}
}

// TestToAnthropic_JSONHasCacheControl_NoBoundary — without the boundary
// marker, system serializes as a single block with NO cache_control on
// it (the last-tool marker covers tools+system together). Verify the
// wire shape so we don't accidentally emit two breakpoints.
func TestToAnthropic_JSONHasCacheControl_NoBoundary(t *testing.T) {
	req := Request{
		System: "plain system, no boundary",
		Tools: []ToolSpec{
			{Name: "OnlyTool", Description: "x", InputSchema: map[string]any{}},
		},
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	}
	raw, _ := json.Marshal(toAnthropic(req, "x", 100))
	body := string(raw)
	cacheMarkerCount := strings.Count(body, `"cache_control":{"type":"ephemeral"}`)
	// 2 markers expected post CC-B: last tool + last user message.
	// System (no boundary) is a single uncached block.
	if cacheMarkerCount != 2 {
		t.Errorf("no-boundary path must emit exactly 2 cache_control (last tool + last user msg); got %d in:\n%s",
			cacheMarkerCount, body)
	}
}

// TestToAnthropic_AntiDistillationOptInIsTopLevelField — exact wire
// parity with claude-code's services/api/claude.ts:312
// (`result.anti_distillation = ['fake_tools']`). When the flag is on,
// the request body gets a TOP-LEVEL `anti_distillation: ["fake_tools"]`
// field. The tools[] array stays untouched — server-side handles the
// real countermeasures. Anything else (e.g. injecting fake tool defs
// client-side) would just give the model fake tools to misuse.
func TestToAnthropic_AntiDistillationOptInIsTopLevelField(t *testing.T) {
	req := Request{
		System: "test",
		Tools: []ToolSpec{
			{Name: "Read", Description: "read file", InputSchema: map[string]any{}},
			{Name: "Bash", Description: "run shell", InputSchema: map[string]any{}},
		},
	}
	out := toAnthropicWithFlags(req, "test-model", 1024, true, false, false)

	// Top-level field set to exactly ["fake_tools"] — matches CC bytes.
	if len(out.AntiDistillation) != 1 || out.AntiDistillation[0] != "fake_tools" {
		t.Errorf("AntiDistillation field mismatch: got %+v, want [\"fake_tools\"]", out.AntiDistillation)
	}
	// Tools array untouched — real tools only, no decoys.
	if len(out.Tools) != 2 {
		t.Fatalf("tools array must NOT be padded with decoys; got %d, want 2", len(out.Tools))
	}
	for i, tool := range out.Tools {
		if strings.HasPrefix(tool.Name, "_internal_") {
			t.Errorf("decoy tool leaked into wire-format tools[%d]: %q (CC opt-in is server-side, no client decoys)", i, tool.Name)
		}
	}
	// cache_control marker still on the LAST real tool — anti-distill
	// must not break the prompt-cache wiring.
	if out.Tools[1].CacheControl == nil || out.Tools[1].CacheControl.Type != "ephemeral" {
		t.Errorf("last tool must still carry cache_control with anti-distill on; got %+v", out.Tools[1].CacheControl)
	}
}

// TestToAnthropic_AntiDistillationOff — flag off → field omitted from
// wire-format (omitempty). Default path matches pre-#75 behavior
// bit-for-bit.
func TestToAnthropic_AntiDistillationOff(t *testing.T) {
	req := Request{
		System: "test",
		Tools: []ToolSpec{
			{Name: "Read", Description: "read file", InputSchema: map[string]any{}},
		},
	}
	out := toAnthropicWithFlags(req, "test-model", 1024, false, false, false)
	if out.AntiDistillation != nil {
		t.Errorf("flag off → AntiDistillation must be nil so omitempty drops the JSON field; got %+v", out.AntiDistillation)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("default path: expected 1 tool, got %d", len(out.Tools))
	}
}

// TestToAnthropic_AntiDistillationJSONWireFormat — round-trip through
// json.Marshal and assert the bytes contain `"anti_distillation":["fake_tools"]`
// with the exact key + value CC sends. A typo in the JSON tag would
// silently break the opt-in (Anthropic backend ignores unknown fields).
func TestToAnthropic_AntiDistillationJSONWireFormat(t *testing.T) {
	req := Request{
		System:   "x",
		Tools:    []ToolSpec{{Name: "Read", Description: "r", InputSchema: map[string]any{}}},
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	}
	on := toAnthropicWithFlags(req, "m", 100, true, false, false)
	rawOn, _ := json.Marshal(on)
	if !strings.Contains(string(rawOn), `"anti_distillation":["fake_tools"]`) {
		t.Errorf("on-path JSON missing anti_distillation field; got:\n%s", rawOn)
	}

	off := toAnthropicWithFlags(req, "m", 100, false, false, false)
	rawOff, _ := json.Marshal(off)
	if strings.Contains(string(rawOff), "anti_distillation") {
		t.Errorf("off-path JSON should NOT contain anti_distillation; got:\n%s", rawOff)
	}
}

// --- Client-side decoys (Task #85) -----------------------------------------

// TestToAnthropic_ClientSideDecoys_FieldOnWireToolsArrayUntouched —
// proves the core "non-pollution" property: when client_side_decoys
// is on, the wire body has the `_decoy_tools_v2_archive` field
// populated, but the real `tools[]` array is BIT-IDENTICAL to the
// flag-off path. The model only ever sees `tools[]`, so it cannot be
// affected by the decoys.
func TestToAnthropic_ClientSideDecoys_FieldOnWireToolsArrayUntouched(t *testing.T) {
	req := Request{
		System: "x",
		Tools: []ToolSpec{
			{Name: "Read", Description: "read file", InputSchema: map[string]any{"type": "object"}},
			{Name: "Bash", Description: "run shell", InputSchema: map[string]any{"type": "object"}},
		},
	}
	off := toAnthropicWithFlags(req, "m", 100, false, false, false)
	on := toAnthropicWithFlags(req, "m", 100, false, true, false)

	// Real tools[] must be identical between off/on (model sees only this).
	if len(on.Tools) != len(off.Tools) {
		t.Fatalf("client_side_decoys MUST NOT change tools[] count; off=%d on=%d", len(off.Tools), len(on.Tools))
	}
	for i := range on.Tools {
		if on.Tools[i].Name != off.Tools[i].Name {
			t.Errorf("tools[%d] name mutated: off=%q on=%q", i, off.Tools[i].Name, on.Tools[i].Name)
		}
	}
	// On-path: decoy archive field must be populated and look like real tools.
	if len(on.DecoyToolsArchive) < 5 {
		t.Errorf("decoy archive should contain a meaningful number of fakes; got %d", len(on.DecoyToolsArchive))
	}
	for _, d := range on.DecoyToolsArchive {
		if d.Name == "" || d.Description == "" || d.InputSchema == nil {
			t.Errorf("decoy must mimic a real tool spec (name+desc+schema); got %+v", d)
		}
		// Must NOT collide with real tool names — that would defeat
		// both honest debugging and decoy plausibility.
		if d.Name == "Read" || d.Name == "Bash" {
			t.Errorf("decoy collides with real tool name: %q", d.Name)
		}
	}
	// Off-path: archive field must be nil so omitempty drops it.
	if off.DecoyToolsArchive != nil {
		t.Errorf("off-path should leave archive nil; got %+v", off.DecoyToolsArchive)
	}
}

// TestToAnthropic_ClientSideDecoys_JSONWireField — round-trip through
// json.Marshal and assert the bytes contain the exact non-standard
// field name `_decoy_tools_v2_archive`. A typo in the JSON tag would
// silently make the entire feature inert (Go would marshal under
// `DecoyToolsArchive` and downstream omitempty wouldn't match).
func TestToAnthropic_ClientSideDecoys_JSONWireField(t *testing.T) {
	req := Request{
		System: "x",
		Tools:  []ToolSpec{{Name: "Read", Description: "r", InputSchema: map[string]any{}}},
	}
	on := toAnthropicWithFlags(req, "m", 100, false, true, false)
	rawOn, _ := json.Marshal(on)
	if !strings.Contains(string(rawOn), `"_decoy_tools_v2_archive":[`) {
		t.Errorf("decoy archive field missing on wire; got:\n%s", rawOn)
	}
	// Spot-check: the decoys appear in JSON before being stripped by API.
	if !strings.Contains(string(rawOn), `"FilesystemSnapshot"`) {
		t.Errorf("expected at least one known decoy name in wire; got:\n%s", rawOn)
	}

	off := toAnthropicWithFlags(req, "m", 100, false, false, false)
	rawOff, _ := json.Marshal(off)
	if strings.Contains(string(rawOff), "_decoy_tools_v2_archive") {
		t.Errorf("off-path JSON must NOT contain the field; got:\n%s", rawOff)
	}
}

// TestToAnthropic_ClientSideDecoys_SystemAndCacheUntouched — the decoy
// channel is at the wire level only. System blocks, cache_control
// markers, and other prompt-shaping features must keep working
// identically when decoys are on.
func TestToAnthropic_ClientSideDecoys_SystemAndCacheUntouched(t *testing.T) {
	sys := "STATIC\n\n" + SystemPromptCacheBoundary + "\n\n<env>cwd=/tmp</env>"
	req := Request{
		System: sys,
		Tools: []ToolSpec{
			{Name: "Read", Description: "r", InputSchema: map[string]any{}},
			{Name: "Bash", Description: "b", InputSchema: map[string]any{}},
		},
	}
	on := toAnthropicWithFlags(req, "m", 100, false, true, false)
	// System still split into 2 blocks with cache_control on the static prefix.
	if len(on.System) != 2 {
		t.Fatalf("system must still split into 2 blocks; got %d", len(on.System))
	}
	if on.System[0].CacheControl == nil {
		t.Errorf("static system block must keep cache_control")
	}
	if on.System[1].CacheControl != nil {
		t.Errorf("dynamic system block must NOT have cache_control")
	}
	// Last real tool must keep cache_control.
	if on.Tools[len(on.Tools)-1].CacheControl == nil {
		t.Errorf("last real tool must keep cache_control marker")
	}
	// Decoy archive entries must NOT carry cache_control (they're not
	// breakpoints — the field is wire-only and ignored by the API).
	for i, d := range on.DecoyToolsArchive {
		if d.CacheControl != nil {
			t.Errorf("decoy[%d] must not carry cache_control; got %+v", i, d.CacheControl)
		}
	}
}

// TestToAnthropic_BothFlagsIndependent — the CC server-side opt-in
// (anti_distillation) and the metis client-side decoys
// (_decoy_tools_v2_archive) are independent channels. Verify all 4
// combinations produce the right wire shape.
func TestToAnthropic_BothFlagsIndependent(t *testing.T) {
	req := Request{
		System: "x",
		Tools:  []ToolSpec{{Name: "Read", Description: "r", InputSchema: map[string]any{}}},
	}
	cases := []struct {
		antiDistill, clientDecoys bool
		wantAntiDistill           bool
		wantDecoyField            bool
	}{
		{false, false, false, false},
		{true, false, true, false},
		{false, true, false, true},
		{true, true, true, true},
	}
	for _, tc := range cases {
		out := toAnthropicWithFlags(req, "m", 100, tc.antiDistill, tc.clientDecoys, false)
		hasAD := len(out.AntiDistillation) > 0
		hasDecoy := len(out.DecoyToolsArchive) > 0
		if hasAD != tc.wantAntiDistill || hasDecoy != tc.wantDecoyField {
			t.Errorf("flags=(%v,%v): got AD=%v decoy=%v; want AD=%v decoy=%v",
				tc.antiDistill, tc.clientDecoys, hasAD, hasDecoy, tc.wantAntiDistill, tc.wantDecoyField)
		}
	}
}

// TestToAnthropic_ImageContentBlock — pasted-image flow. The user
// turn carries one text + one image block; the wire-format converter
// must emit the canonical Anthropic `{"type":"image",
// "source":{"type":"base64", "media_type":..., "data":...}}` shape.
// Verifies both the JSON tag layout and the base64 payload survives
// round-trip without modification.
func TestToAnthropic_ImageContentBlock(t *testing.T) {
	req := Request{
		Messages: []Message{{
			Role: RoleUser,
			Content: []ContentBlock{
				{Type: "text", Text: "What's this?"},
				{Type: "image", MediaType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUg=="},
			},
		}},
	}
	out := toAnthropic(req, "claude-sonnet-4", 1024)
	if len(out.Messages) != 1 {
		t.Fatalf("message count: %d", len(out.Messages))
	}
	m := out.Messages[0]
	if len(m.Content) != 2 {
		t.Fatalf("user content blocks: want 2 (text + image), got %d", len(m.Content))
	}
	img := m.Content[1]
	if img.Type != "image" {
		t.Errorf("Type = %q, want \"image\"", img.Type)
	}
	if img.Source == nil {
		t.Fatalf("image block missing Source")
	}
	if img.Source.Type != "base64" {
		t.Errorf("Source.Type = %q, want \"base64\"", img.Source.Type)
	}
	if img.Source.MediaType != "image/png" {
		t.Errorf("Source.MediaType = %q", img.Source.MediaType)
	}
	if img.Source.Data != "iVBORw0KGgoAAAANSUhEUg==" {
		t.Errorf("Source.Data not preserved: %q", img.Source.Data)
	}

	// Also verify the JSON shape — we want exactly the keys the
	// Anthropic API expects.
	buf, err := json.Marshal(img)
	if err != nil {
		t.Fatal(err)
	}
	got := string(buf)
	for _, want := range []string{`"type":"image"`, `"source":{`, `"media_type":"image/png"`, `"data":"iVBOR`} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON missing %q: %s", want, got)
		}
	}
}
