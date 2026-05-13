package agent

// lazy_tools_search_test.go — locks the keyword-search + multi-select
// flavors of handleToolSearch added in the claude-code parity pass
// (2026-05-11). Existing _handle_test.go covers the legacy {name}
// form; this file covers the new {query} form.
//
// Reference: claude-code-sourcemap/restored-src/src/tools/ToolSearchTool/
// ToolSearchTool.ts:186-302 (searchToolsWithKeywords). The scoring
// table here mirrors theirs minus the searchHint weight, which metis
// doesn't have a curated source for yet.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// newLoopWithTools registers multiple fake tools — the keyword search
// needs a meaningful corpus or every test devolves into "1 result".
func newLoopWithTools(specs ...fakeMCPTool) *Loop {
	reg := tools.NewRegistry()
	for _, s := range specs {
		reg.Register(s)
	}
	return &Loop{Registry: reg}
}

// mcpFake — short constructor for a registered MCP tool. Trims the
// boilerplate so each test reads as "what tools exist, then query".
func mcpFake(name, desc string) fakeMCPTool {
	return fakeMCPTool{
		name:        name,
		description: desc,
		schema:      map[string]any{"type": "object"},
	}
}

// invokeSearch is the call-site shorthand. Returns parsed JSON body
// plus the IsError flag so a single helper covers both happy + error
// paths.
func invokeSearch(t *testing.T, l *Loop, input map[string]any) (map[string]any, bool) {
	t.Helper()
	in := llm.ContentBlock{
		Type:      "tool_use",
		ToolUseID: "tu",
		ToolName:  "ToolSearch",
		ToolInput: input,
	}
	got := handleToolSearch(l, in)
	if got.IsError {
		return map[string]any{"error": got.ToolResult}, true
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got.ToolResult), &parsed); err != nil {
		t.Fatalf("result not JSON: %v\nbody: %s", err, got.ToolResult)
	}
	return parsed, false
}

// matchNames extracts just the "name" field from the matches array.
// Most tests care about WHICH tools matched and in what order, not
// the full payload shape.
func matchNames(t *testing.T, parsed map[string]any) []string {
	t.Helper()
	arr, ok := parsed["matches"].([]any)
	if !ok {
		t.Fatalf("expected matches array; got %T (%+v)", parsed["matches"], parsed)
	}
	out := make([]string, 0, len(arr))
	for _, m := range arr {
		mm, _ := m.(map[string]any)
		if n, _ := mm["name"].(string); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// TestHandleToolSearch_SelectMultiple — `select:n1,n2` returns BOTH
// schemas in one round-trip. The whole point of multi-select is to
// save the model the round-trip cost of fetching tools one at a time.
func TestHandleToolSearch_SelectMultiple(t *testing.T) {
	l := newLoopWithTools(
		mcpFake("mcp__fs__read", "read a file"),
		mcpFake("mcp__fs__write", "write a file"),
		mcpFake("mcp__http__get", "fetch a url"),
	)
	parsed, isErr := invokeSearch(t, l, map[string]any{"query": "select:mcp__fs__read,mcp__fs__write"})
	if isErr {
		t.Fatalf("expected success; got error: %v", parsed)
	}
	got := matchNames(t, parsed)
	if len(got) != 2 {
		t.Fatalf("expected 2 matches; got %d (%v)", len(got), got)
	}
	// Order should follow the request. Models tend to mirror the order
	// back in subsequent tool calls, so preserving it avoids spurious
	// re-fetches when the model double-checks one of them.
	if got[0] != "mcp__fs__read" || got[1] != "mcp__fs__write" {
		t.Errorf("order not preserved: %v", got)
	}
	// Each match must carry a schema (the whole point of select).
	for _, raw := range parsed["matches"].([]any) {
		m := raw.(map[string]any)
		if _, ok := m["input_schema"].(map[string]any); !ok {
			t.Errorf("match %q missing input_schema; got %T", m["name"], m["input_schema"])
		}
	}
}

// TestHandleToolSearch_SelectPartialMiss — `select:` with one hit + one
// miss returns the hit in matches and the miss in missing[], NOT an
// error. The model can act on what's there and re-query the missing
// name if it still wants it.
func TestHandleToolSearch_SelectPartialMiss(t *testing.T) {
	l := newLoopWithTools(
		mcpFake("mcp__real", "real tool"),
	)
	parsed, isErr := invokeSearch(t, l, map[string]any{"query": "select:mcp__real,mcp__ghost"})
	if isErr {
		t.Fatalf("partial miss should NOT be IsError; got: %v", parsed)
	}
	got := matchNames(t, parsed)
	if len(got) != 1 || got[0] != "mcp__real" {
		t.Errorf("matches wrong; got %v", got)
	}
	missing, _ := parsed["missing"].([]any)
	if len(missing) != 1 || missing[0] != "mcp__ghost" {
		t.Errorf("missing should list mcp__ghost; got %v", missing)
	}
}

// TestHandleToolSearch_SelectAllMiss — when EVERY requested name
// misses, return IsError so the model immediately re-queries with a
// different approach (likely a keyword search) instead of continuing
// with an empty matches[] (which it might silently ignore).
func TestHandleToolSearch_SelectAllMiss(t *testing.T) {
	l := newLoopWithTools(mcpFake("mcp__real", "real"))
	parsed, isErr := invokeSearch(t, l, map[string]any{"query": "select:mcp__a,mcp__b"})
	if !isErr {
		t.Errorf("all-miss should be IsError; got: %v", parsed)
	}
	body, _ := parsed["error"].(string)
	if !strings.Contains(body, "mcp__a") || !strings.Contains(body, "mcp__b") {
		t.Errorf("error should echo missing names; got: %s", body)
	}
}

// TestHandleToolSearch_KeywordRanksExactPart — `read` should rank
// mcp__fs__read above mcp__http__get even though both mention "read"
// somewhere. The exact-part bonus (+12 for matching a `__`-split
// segment) is what separates them.
func TestHandleToolSearch_KeywordRanksExactPart(t *testing.T) {
	l := newLoopWithTools(
		mcpFake("mcp__http__get", "fetch a URL and read the body"),
		mcpFake("mcp__fs__read", "read a file"),
		mcpFake("mcp__fs__write", "write to a file"),
	)
	parsed, isErr := invokeSearch(t, l, map[string]any{"query": "read"})
	if isErr {
		t.Fatalf("expected success; got: %v", parsed)
	}
	got := matchNames(t, parsed)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 matches; got %v", got)
	}
	if got[0] != "mcp__fs__read" {
		t.Errorf("exact part match should rank first; got order %v", got)
	}
}

// TestHandleToolSearch_KeywordRequiredFilter — `+slack` filters out
// every tool whose name+description doesn't include "slack", even if
// other terms would otherwise score the tool highly.
func TestHandleToolSearch_KeywordRequiredFilter(t *testing.T) {
	l := newLoopWithTools(
		mcpFake("mcp__slack__send", "send a message to slack"),
		mcpFake("mcp__email__send", "send an email"),
		mcpFake("mcp__sms__send", "send an sms"),
	)
	parsed, isErr := invokeSearch(t, l, map[string]any{"query": "+slack send"})
	if isErr {
		t.Fatalf("expected success; got: %v", parsed)
	}
	got := matchNames(t, parsed)
	if len(got) != 1 || got[0] != "mcp__slack__send" {
		t.Errorf("required +slack filter should leave only the slack tool; got %v", got)
	}
}

// TestHandleToolSearch_KeywordMaxResults — `max_results` caps the
// returned slice. Smaller max → fewer matches even when more would
// qualify.
func TestHandleToolSearch_KeywordMaxResults(t *testing.T) {
	l := newLoopWithTools(
		mcpFake("mcp__a__read", "read"),
		mcpFake("mcp__b__read", "read"),
		mcpFake("mcp__c__read", "read"),
		mcpFake("mcp__d__read", "read"),
	)
	parsed, isErr := invokeSearch(t, l, map[string]any{
		"query":       "read",
		"max_results": float64(2),
	})
	if isErr {
		t.Fatalf("expected success; got: %v", parsed)
	}
	got := matchNames(t, parsed)
	if len(got) != 2 {
		t.Errorf("max_results=2 should cap at 2; got %d (%v)", len(got), got)
	}
}

// TestHandleToolSearch_KeywordNoMCPMatchesEmpty — non-MCP tools must
// NOT match keyword search. Builtins are always in tools[] anyway —
// returning them here would be noise.
func TestHandleToolSearch_KeywordExcludesNonMCP(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(mcpFake("Read", "read a file"))             // not mcp__
	reg.Register(mcpFake("mcp__fs__read", "read remote fs")) // is mcp__
	l := &Loop{Registry: reg}
	parsed, isErr := invokeSearch(t, l, map[string]any{"query": "read"})
	if isErr {
		t.Fatalf("expected success; got: %v", parsed)
	}
	got := matchNames(t, parsed)
	if len(got) != 1 || got[0] != "mcp__fs__read" {
		t.Errorf("non-mcp tool should NOT appear in keyword results; got %v", got)
	}
}

// TestHandleToolSearch_KeywordNoMatchesReturnsHelpfulMessage — when
// nothing matches, the body should suggest a fallback (broader query
// or select:<exact-name>). Models tend to give up gracefully on an
// empty matches[]; a hint nudges them to refine.
func TestHandleToolSearch_KeywordNoMatchesReturnsHelpfulMessage(t *testing.T) {
	l := newLoopWithTools(mcpFake("mcp__fs__read", "read"))
	in := llm.ContentBlock{
		Type:      "tool_use",
		ToolUseID: "tu",
		ToolName:  "ToolSearch",
		ToolInput: map[string]any{"query": "absolutely-no-such-keyword"},
	}
	got := handleToolSearch(l, in)
	if got.IsError {
		t.Errorf("no-match keyword search should NOT be IsError (model can re-query); got: %s", got.ToolResult)
	}
	if !strings.Contains(got.ToolResult, "broader") && !strings.Contains(got.ToolResult, "select") {
		t.Errorf("body should hint at fallbacks; got: %s", got.ToolResult)
	}
}

// TestSplitQueryTerms_ParsesRequiredAndOptional — the small parser
// underneath searchToolsWithKeywords. Lock the grammar so a refactor
// of the scoring weights can't accidentally drop "+" handling.
func TestSplitQueryTerms_ParsesRequiredAndOptional(t *testing.T) {
	cases := []struct {
		in           string
		wantRequired []string
		wantOptional []string
	}{
		{"slack", nil, []string{"slack"}},
		{"+slack", []string{"slack"}, nil},
		{"+slack send", []string{"slack"}, []string{"send"}},
		{"+slack +chat send", []string{"slack", "chat"}, []string{"send"}},
		{"  +screenshot  ", []string{"screenshot"}, nil},
		{"+ +slack", []string{"slack"}, nil},        // lone '+' dropped
		{"SCREENSHOT", nil, []string{"screenshot"}}, // case-insensitive
		{"slack slack", nil, []string{"slack"}},     // de-duped
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			gotR, gotO := splitQueryTerms(c.in)
			if !equalStringSlices(gotR, c.wantRequired) {
				t.Errorf("required: got %v, want %v", gotR, c.wantRequired)
			}
			if !equalStringSlices(gotO, c.wantOptional) {
				t.Errorf("optional: got %v, want %v", gotO, c.wantOptional)
			}
		})
	}
}

// TestSplitToolNameParts_DropsMcpPrefix — every deferred tool starts
// with "mcp" so matching on it is no signal. Drop it consistently.
func TestSplitToolNameParts_DropsMcpPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"mcp__fs__read", []string{"fs", "read"}},
		{"mcp__server__action_name", []string{"server", "action_name"}},
		{"mcp__foo", []string{"foo"}},
		{"mcp__", nil},
		{"mcp__foo__", []string{"foo"}}, // trailing empty segment dropped
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := splitToolNameParts(c.in)
			if !equalStringSlices(got, c.want) {
				t.Errorf("splitToolNameParts(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// equalStringSlices — tiny helper. nil == empty for our purposes
// (tests don't care about distinguishing them).
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStripDescriptionForLazy_TrimsBothPrefixAndTail — the cost we're
// trying to cut: each stripped MCP tool was carrying a 270-char
// description across 24 tools = ~1600 tokens of "remember what this
// tool does" before the model even needed any of them. The trim
// keeps a one-sentence hint and the standard lazy suffix.
func TestStripDescriptionForLazy_TrimsBothPrefixAndTail(t *testing.T) {
	in := "[MCP] Run a sequence of computer-use steps in one round-trip. Each step is `{tool, params}`. Aborts on the first error."
	got := stripDescriptionForLazy(in)
	if strings.Contains(got, "[MCP]") {
		t.Errorf("[MCP] prefix should be trimmed for lazy descriptions; got %q", got)
	}
	if !strings.Contains(got, "Run a sequence") {
		t.Errorf("first sentence should be preserved; got %q", got)
	}
	if strings.Contains(got, "Each step") {
		t.Errorf("trailing sentences should be cut; got %q", got)
	}
	if !strings.Contains(got, "schema lazy") {
		t.Errorf("lazy hint suffix should be appended; got %q", got)
	}
	if len(got) > lazyDescriptionCap+100 {
		t.Errorf("trimmed output should be <%d chars; got %d (%q)", lazyDescriptionCap+100, len(got), got)
	}
}

// TestStripDescriptionForLazy_HardCapOnRunon — when there's no period
// or newline (one giant run-on sentence) the byte cap kicks in. UTF-8
// boundary safety: cut should not land mid-rune.
func TestStripDescriptionForLazy_HardCapOnRunon(t *testing.T) {
	in := "[MCP] " + strings.Repeat("a", 200) // no sentence break, 200 chars
	got := stripDescriptionForLazy(in)
	if !strings.Contains(got, "…") {
		t.Errorf("hard-capped trim should end with ellipsis; got %q", got)
	}
	if !strings.Contains(got, "schema lazy") {
		t.Errorf("lazy hint suffix should still be appended; got %q", got)
	}
}
