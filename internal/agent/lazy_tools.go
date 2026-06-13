// Package agent — lazy MCP tool schema (ToolSearch).
//
// claude-code's pattern: when an MCP-heavy session has 50+ tools whose
// JSON schemas balloon the prompt to 20K+ tokens, strip the schemas
// from the upfront tools list and expose a meta-tool ToolSearch. The
// model uses ToolSearch to fetch a specific tool's schema on demand,
// trading one extra round-trip for a much smaller per-iteration prompt.
//
// Mode is controlled by the `ENABLE_TOOL_SEARCH` environment variable.
// Default matches claude-code's `tst` (always-defer) rather than
// openclaude's `tst-auto` — running unrestricted MCP servers on a
// modern 192k+ window otherwise burns 4-20k tokens per turn before the
// model even sees the user's message:
//
//	(unset)    → always lazy (deferred MCP tools always stripped)
//	"auto"     → auto, fires when deferred tokens > 10% of context window
//	"auto:N"   → auto, N% (1..99)
//	"auto:0"   → always lazy (equivalent to "true")
//	"auto:100" → never lazy (equivalent to "false")
//	"true"     → always lazy
//	"false"    → never lazy (full schemas always sent)
//
// Trade-off: lazy mode adds ~one tool-call round-trip the FIRST time
// the model uses each MCP tool. After that the schema is in the
// conversation history. Net win when MCP schemas dominate the prompt.
package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// MaxToolSearchResults caps the matches a single ToolSearch keyword
// query returns. claude-code uses 5; we match. Higher values give the
// model more options but blow up the tool_result body — and the model
// can always re-query with refined terms if 5 isn't enough.
const MaxToolSearchResults = 50

// DefaultToolSearchResults — the keyword-search return cap when the
// caller doesn't pass `max_results`. Matches claude-code's default
// (ToolSearchTool.ts:34).
const DefaultToolSearchResults = 5

// handleToolSearch resolves the model's ToolSearch invocation. Three
// query shapes are accepted (mirrors claude-code-sourcemap's
// ToolSearchTool.ts:21-34 + 186-302):
//
//	{"query": "select:n1,n2,n3"}   → exact multi-fetch (returns schemas)
//	{"query": "screenshot",        → weighted keyword search (returns
//	 "max_results": 5}                names+descriptions only; model
//	                                  follows up with select:<name>)
//	{"name": "single_name"}        → legacy alias, treated as select:name
//
// Legacy `{name}` is kept so older transcripts replayed after the
// rewrite still work — costs nothing and avoids breaking sessions that
// crossed the upgrade boundary mid-flight.
func handleToolSearch(l *Loop, b llm.ContentBlock) llm.ContentBlock {
	query := stringField(b.ToolInput, "query")
	if query == "" {
		if name := stringField(b.ToolInput, "name"); name != "" {
			query = "select:" + name
		}
	}
	if query == "" {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: b.ToolUseID,
			ToolResult: "error: ToolSearch requires {\"query\":\"...\"} — use \"select:<name>\" for exact lookup or keywords to search (e.g. \"screenshot\", \"+slack send\")",
			IsError:    true,
		}
	}

	maxResults := DefaultToolSearchResults
	switch n := b.ToolInput["max_results"].(type) {
	case float64:
		if n > 0 {
			maxResults = int(n)
		}
	case int:
		if n > 0 {
			maxResults = n
		}
	}
	if maxResults > MaxToolSearchResults {
		maxResults = MaxToolSearchResults
	}

	if strings.HasPrefix(query, "select:") {
		return handleSelectQuery(l, b.ToolUseID, strings.TrimPrefix(query, "select:"))
	}
	return handleKeywordSearch(l, b.ToolUseID, query, maxResults)
}

// handleSelectQuery fans out an exact-name list (`select:n1,n2,n3`)
// into a {matches:[...], missing:[...]} payload. Returns IsError only
// when EVERY requested name misses — partial misses still ship the
// hits, so the model can act on what it got and re-query the rest.
func handleSelectQuery(l *Loop, toolUseID, body string) llm.ContentBlock {
	names := strings.Split(body, ",")
	matches := make([]map[string]any, 0, len(names))
	var missing []string
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		t, ok := l.Registry.Get(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		matches = append(matches, map[string]any{
			"name":         t.Name(),
			"description":  t.Description(),
			"input_schema": t.InputSchema(),
		})
		// Cross-compaction stability: once we hand the schema to the
		// model, the next toolSpecs() build keeps that mcp__ tool's
		// schema intact instead of re-stripping it. Avoids an extra
		// ToolSearch round-trip after compaction (P6).
		l.markMCPDiscovered(t.Name())
	}
	if len(matches) == 0 {
		// All-miss → IsError so the model retries with a different
		// query rather than continuing with empty hands.
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: toolUseID,
			ToolResult: fmt.Sprintf("error: tool(s) %v not found", missing),
			IsError:    true,
		}
	}
	result := map[string]any{"matches": matches}
	if len(missing) > 0 {
		result["missing"] = missing
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: toolUseID,
			ToolResult: fmt.Sprintf("error: marshal failed: %v", err),
			IsError:    true,
		}
	}
	return llm.ContentBlock{
		Type: "tool_result", ToolUseID: toolUseID,
		ToolResult: string(raw),
	}
}

// handleKeywordSearch runs searchToolsWithKeywords across the
// registry's deferred (mcp__) tools, returning the top-N matches as
// name+description pairs (NO schemas). The model follows up with a
// `select:<name>` call once it picks a winner — keeps the search-
// result body small and lets the model use cheap text to rank
// alternatives before paying the schema-fetch round-trip.
func handleKeywordSearch(l *Loop, toolUseID, query string, maxResults int) llm.ContentBlock {
	matches := searchToolsWithKeywords(l, query, maxResults)
	if len(matches) == 0 {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: toolUseID,
			ToolResult: fmt.Sprintf("no deferred tools matched %q — try a broader query or `select:<exact-name>` if you know the name", query),
		}
	}
	raw, err := json.Marshal(map[string]any{"matches": matches})
	if err != nil {
		return llm.ContentBlock{
			Type: "tool_result", ToolUseID: toolUseID,
			ToolResult: fmt.Sprintf("error: marshal failed: %v", err),
			IsError:    true,
		}
	}
	return llm.ContentBlock{
		Type: "tool_result", ToolUseID: toolUseID,
		ToolResult: string(raw),
	}
}

// searchToolsWithKeywords ranks the registry's deferred (mcp__) tools
// against a free-text query, returning the top-N as ordered
// {name, description} pairs (no schemas).
//
// Query syntax (mirrors claude-code-sourcemap's
// ToolSearchTool.ts:186-302 `searchToolsWithKeywords`):
//
//	"screenshot"          → optional terms: tools containing "screenshot" rank higher
//	"+slack send"         → required: "slack" must appear; "send" ranks
//	"+screenshot +window" → all required terms must appear
//
// Scoring weights (per term):
//
//	+12   exact-part match in name (mcp__server__action split on `__`)
//	+8    required term present in name
//	+6    partial substring in name
//	+4    appears in the tool's curated searchHint (pkg/tool.SearchHinter;
//	      MCP servers surface it via _meta — most tools have none and
//	      simply never collect this weight)
//	+2    appears in description (word-boundary fuzzy via Contains)
//
// Required terms are also part of the rank score, so among tools that
// all satisfy the required filter, the ones with the strongest match
// for those terms still come out ahead.
func searchToolsWithKeywords(l *Loop, query string, maxResults int) []map[string]any {
	if l == nil || l.Registry == nil {
		return nil
	}
	required, optional := splitQueryTerms(query)
	if len(required) == 0 && len(optional) == 0 {
		return nil
	}

	type scored struct {
		name        string
		description string
		score       int
	}

	all := l.Registry.SortedForCache()
	results := make([]scored, 0, len(all))
	for _, t := range all {
		name := t.Name()
		if !strings.HasPrefix(name, "mcp__") {
			continue
		}
		nameLower := strings.ToLower(name)
		descLower := strings.ToLower(t.Description())
		hintLower := strings.ToLower(pubtool.SearchHint(t))
		nameParts := splitToolNameParts(nameLower)

		// Required-term filter: every required term must appear
		// somewhere (name, hint or description). One miss disqualifies.
		skip := false
		for _, req := range required {
			if !strings.Contains(nameLower, req) && !strings.Contains(descLower, req) && !strings.Contains(hintLower, req) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		score := scoreQueryAgainstTool(required, optional, nameParts, nameLower, descLower, hintLower)
		if score == 0 {
			// No signal at all — only required terms matched and they
			// only appear in description with no extras. Skip to keep
			// the result list focused.
			continue
		}
		results = append(results, scored{
			name:        name,
			description: t.Description(),
			score:       score,
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].name < results[j].name
	})
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	out := make([]map[string]any, 0, len(results))
	for _, r := range results {
		out = append(out, map[string]any{
			"name":        r.name,
			"description": r.description,
		})
	}
	return out
}

// splitQueryTerms partitions a query string into required (+prefix)
// and optional terms. Lowercased + de-duped. Empty terms (e.g. lone
// "+" tokens or excess whitespace) are dropped.
func splitQueryTerms(query string) (required, optional []string) {
	seen := make(map[string]bool, 4)
	for _, raw := range strings.Fields(strings.ToLower(query)) {
		isRequired := strings.HasPrefix(raw, "+")
		term := strings.TrimPrefix(raw, "+")
		term = strings.TrimSpace(term)
		if term == "" || seen[term] {
			continue
		}
		seen[term] = true
		if isRequired {
			required = append(required, term)
		} else {
			optional = append(optional, term)
		}
	}
	return required, optional
}

// splitToolNameParts breaks "mcp__server__action_name" into
// ["server", "action_name"] for exact-part scoring. The leading
// "mcp" segment is dropped — every deferred tool starts with it, so
// matching on it gives no signal.
func splitToolNameParts(name string) []string {
	parts := strings.Split(name, "__")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "mcp" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// scoreQueryAgainstTool applies the weight table to one tool. Kept as
// a separate fn so tests can call it directly with controlled inputs.
func scoreQueryAgainstTool(required, optional, nameParts []string, nameLower, descLower, hintLower string) int {
	score := 0
	for _, term := range optional {
		for _, part := range nameParts {
			if part == term {
				score += 12
				break
			}
		}
		if strings.Contains(nameLower, term) {
			score += 6
		}
		if hintLower != "" && strings.Contains(hintLower, term) {
			score += 4
		}
		if strings.Contains(descLower, term) {
			score += 2
		}
	}
	for _, req := range required {
		if strings.Contains(nameLower, req) {
			score += 8
		}
		if hintLower != "" && strings.Contains(hintLower, req) {
			score += 4
		}
		if strings.Contains(descLower, req) {
			score += 2
		}
	}
	return score
}

// LazyTokenPercentageDefault — the share of the model's context window
// that deferred (mcp__) tool definitions are allowed to occupy before
// auto-mode lazy kicks in. 10% matches openclaude's `tst-auto`
// default (toolSearch.ts:49). Lower → more aggressive, higher →
// more permissive.
//
// Why a percentage, not a fixed token count: a 16k-window model
// chokes on 6k of MCP schemas (37% of budget), but a 200k-window
// model wouldn't notice (3%). One static threshold can't satisfy
// both — a percentage tracks the model's actual budget.
const LazyTokenPercentageDefault = 10

// LazyMode is the user-selected lazy-tool-loading mode parsed from
// ENABLE_TOOL_SEARCH. Mirrors openclaude's `ToolSearchMode` tri-state.
type LazyMode int

const (
	// LazyModeStandard — never strip schemas. The full tools[] array
	// (including MCP) is sent every turn. Use when the model is
	// confused by deferred tools or you're debugging tool selection.
	LazyModeStandard LazyMode = iota

	// LazyModeAuto — strip schemas when deferred (mcp__) tools'
	// estimated token cost exceeds Percentage% of the context window.
	// The default. Adapts to the model's window automatically.
	LazyModeAuto

	// LazyModeAlways — always strip mcp__ schemas, regardless of
	// budget. Useful for very small-window providers where even
	// modest MCP loads matter, or for matching claude-code's
	// "always-defer" behavior on Anthropic models.
	LazyModeAlways
)

// parseEnableToolSearch resolves the ENABLE_TOOL_SEARCH env value into
// a (mode, percentage) pair. Empty falls back to LazyModeAlways
// (claude-code parity — see file header). Unrecognized values fall
// back to Auto/10 so a typo doesn't silently break MCP loads.
//
//	value         mode      percentage    notes
//	(empty)       Always    0             default (claude-code "tst")
//	"auto"        Auto      10            opt-in budget-based trigger
//	"auto:N"      Auto      N             1 ≤ N ≤ 99
//	"auto:0"      Always    0             same as "true"
//	"auto:100"    Standard  0             same as "false"
//	"true"/"1"/"yes"/"on"    Always    0
//	"false"/"0"/"no"/"off"   Standard  0
//	other         Auto      10            silent fallback
//
// Lower-cased and trimmed before matching so users can write
// `ENABLE_TOOL_SEARCH=Auto:25` or `ENABLE_TOOL_SEARCH= true ` and have
// it work. Returns the percentage as 0 for non-Auto modes (irrelevant).
func parseEnableToolSearch(value string) (LazyMode, int) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return LazyModeAlways, 0
	}
	if v == "auto" {
		return LazyModeAuto, LazyTokenPercentageDefault
	}
	if strings.HasPrefix(v, "auto:") {
		n, err := strconv.Atoi(strings.TrimPrefix(v, "auto:"))
		if err != nil {
			return LazyModeAuto, LazyTokenPercentageDefault
		}
		if n <= 0 {
			return LazyModeAlways, 0
		}
		if n >= 100 {
			return LazyModeStandard, 0
		}
		return LazyModeAuto, n
	}
	switch v {
	case "true", "1", "yes", "on":
		return LazyModeAlways, 0
	case "false", "0", "no", "off":
		return LazyModeStandard, 0
	}
	return LazyModeAuto, LazyTokenPercentageDefault
}

// charsPerToken — rough char-to-token ratio for ToolSpec JSON
// (name + description + schema). 2.5 is openclaude's fallback
// constant (toolSearch.ts:99) chosen for JSON-heavy text where
// punctuation density is high; the more common "4 chars/token"
// rule of thumb under-counts tool schemas by ~40%.
const charsPerToken = 2.5

// stripAndAppendToolSearch performs the actual lazy-mode rewrite,
// independent of the trigger logic. Pulled out so both the legacy
// count-based trigger (applyLazySchema) and the token-budget trigger
// (applyLazySchemaByTokens) share one implementation.
//
// Calls into stripAndAppendToolSearchWithPreserve with an empty
// preserve set — every mcp__ tool gets its schema stripped. Use this
// form from tests and any call site that doesn't have a Loop handle.
func stripAndAppendToolSearch(specs []llm.ToolSpec) []llm.ToolSpec {
	return stripAndAppendToolSearchWithPreserve(specs, nil)
}

// stripAndAppendToolSearchWithPreserve is the discovery-aware variant.
// Tools whose name is in `preserve` keep their original schema — they
// were already fetched in this session and re-stripping them would
// force the model to re-invoke ToolSearch after compaction
// (cross-compaction stability, openclaude pattern; see
// lazy_tools_discovered.go).
//
// The rewrite:
//   - For each mcp__ tool NOT in preserve, replace InputSchema with
//     the lazy placeholder.
//   - For each mcp__ tool IN preserve, keep the original schema.
//   - Append the synthetic ToolSearch entry IFF any stripping
//     happened — if preserve covers all mcp__ tools, the meta-tool is
//     pointless and would just add a confusing entry to tools[].
//
// Non-MCP tools (built-ins) are always passed through verbatim. The
// core Read/Edit/Bash etc. have small, well-known schemas; deferring
// them would cost a round-trip for zero benefit.
func stripAndAppendToolSearchWithPreserve(specs []llm.ToolSpec, preserve map[string]bool) []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(specs)+1)
	stripped := false
	for _, s := range specs {
		if !strings.HasPrefix(s.Name, "mcp__") {
			out = append(out, s)
			continue
		}
		if preserve[s.Name] {
			// Already discovered — keep the schema. The model can
			// invoke this tool directly without another ToolSearch
			// round-trip.
			out = append(out, s)
			continue
		}
		stripped = true
		out = append(out, llm.ToolSpec{
			Name:        s.Name,
			Description: stripDescriptionForLazy(s.Description),
			InputSchema: lazyPlaceholderSchema(),
		})
	}
	if !stripped {
		// Either no mcp__ tools at all, or every mcp__ tool is already
		// preserved. Nothing to defer → don't append ToolSearch.
		return specs
	}
	out = append(out, toolSearchSpec(specs))
	return out
}

// applyLazySchemaByTokens triggers lazy mode when the deferred (mcp__)
// tools' total token estimate exceeds `contextWindow * percentage / 100`.
//
// Why this is a better default than count-based: a single big-schema
// MCP tool (e.g. playwright with ~5k tokens of schema) on a 16k-window
// model is a runaway, but five small MCP tools (~50 tokens each) on a
// 200k-window model are noise. Count-based triggers miss the first and
// over-fire on the second — token-based gets both right.
//
// percentage values:
//   - ≤ 0  → disabled (no-op)
//   - ≥ 100 → disabled (allows MCP schemas to occupy 100% of budget)
//   - default LazyTokenPercentageDefault (10) when caller passes 0
//
// contextWindow ≤ 0 → no-op (caller should fall back to the legacy
// count-based path).
func applyLazySchemaByTokens(specs []llm.ToolSpec, contextWindow, percentage int) []llm.ToolSpec {
	return applyLazySchemaByTokensWithPreserve(specs, contextWindow, percentage, nil)
}

// applyLazySchemaByTokensWithPreserve — same trigger logic as
// applyLazySchemaByTokens, but threads a "preserve" set through to
// stripAndAppendToolSearchWithPreserve so cross-compaction discovered
// tools keep their schemas. Used by dispatch.go::toolSpecs once it
// has a Loop handle to read snapshotDiscoveredMCP() from.
func applyLazySchemaByTokensWithPreserve(specs []llm.ToolSpec, contextWindow, percentage int, preserve map[string]bool) []llm.ToolSpec {
	if contextWindow <= 0 || percentage <= 0 || percentage >= 100 {
		return specs
	}
	deferredTokens := 0
	for _, s := range specs {
		if strings.HasPrefix(s.Name, "mcp__") {
			deferredTokens += estimateSpecTokens(s)
		}
	}
	budget := contextWindow * percentage / 100
	if deferredTokens < budget {
		return specs
	}
	return stripAndAppendToolSearchWithPreserve(specs, preserve)
}

// estimateSpecTokens — cheap token estimate for a single ToolSpec, used
// only for the lazy-mode trigger decision. Sums name + description +
// JSON-marshalled schema length, divides by charsPerToken (2.5).
//
// Not exact — a real tokenizer would be ~10× slower per call and
// requires loading a vocab. The error band is ±20%, which is fine for
// a "should we strip schemas" branch (the budget itself is heuristic).
func estimateSpecTokens(s llm.ToolSpec) int {
	n := len(s.Name) + len(s.Description)
	if raw, err := json.Marshal(s.InputSchema); err == nil {
		n += len(raw)
	}
	// Framing overhead for the {"name":..., "description":..., "input_schema":...} wrapper.
	n += 32
	return int(float64(n) / charsPerToken)
}

// lazyDescriptionCap — when a tool's schema is stripped, also clip
// its description to one short sentence. The model only needs enough
// hint to decide "should I ToolSearch for this one" — the full doc
// comes back attached to the eventual schema fetch. 80 chars fits
// most one-line summaries ("Take a screenshot of the current screen",
// "Click at (x, y) using the named mouse button").
//
// Empirically: untrimmed mcp__computer-use__* descriptions averaged
// 270 chars each across 24 tools = 6.5 KB / ~1.6k tokens spent
// describing tools the model isn't using this turn. Trim to ~80 +
// hint suffix = ~120 chars/tool = 2.9 KB / ~720 tokens for the same
// surface, saving ~880 tokens on a fresh "你好" turn.
const lazyDescriptionCap = 80

// stripDescriptionForLazy clips a tool's description to the first
// sentence (or first newline, or `lazyDescriptionCap` bytes — whichever
// comes first), then appends the lazy hint suffix. The model still
// sees what the tool does, but doesn't pay for the full multi-
// paragraph docstring on every turn before it's even relevant.
//
// Why one sentence rather than the full description: claude-code's
// deferred-tool surface only lists name + a curated `searchHint`
// field; metis doesn't have curated hints, so we approximate by
// taking the first sentence which is almost always the summary line.
func stripDescriptionForLazy(desc string) string {
	const hint = "  [schema lazy — call ToolSearch to fetch parameters before invoking]"
	d := desc
	// Trim the "[MCP] " prefix that MCPTool.Description() adds, since
	// every stripped tool by definition is an MCP tool — the prefix
	// just burns ~6 bytes per tool repeating what the mcp__ name
	// prefix already conveys.
	d = strings.TrimPrefix(d, "[MCP] ")
	// Cut at first `\n`.
	if i := strings.IndexByte(d, '\n'); i >= 0 {
		d = d[:i]
	}
	// Cut at first ". " (period followed by space). Standalone "." inside
	// a tool name (e.g. ".env") shouldn't trigger; the ". " bigram is a
	// strong sentence-end signal.
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i+1] // include the period
	}
	// Hard byte cap. Walk back to a UTF-8 boundary so we don't cut a
	// rune in half (same approach as MCP server.go::truncateDescription).
	if len(d) > lazyDescriptionCap {
		cut := lazyDescriptionCap
		for cut > 0 && (d[cut]&0xC0) == 0x80 {
			cut--
		}
		d = strings.TrimSpace(d[:cut]) + "…"
	}
	return d + hint
}

// lazyPlaceholderSchema is the minimal "any object" schema we leave
// behind when stripping a real MCP schema. The model cannot call the
// tool successfully with this (params will be unvalidated), which is
// the point — it must go via ToolSearch first to learn what params
// the tool actually takes.
func lazyPlaceholderSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"description":          "schema deferred — call ToolSearch first",
		"additionalProperties": true,
	}
}

// toolSearchSpec — the meta-tool definition the model invokes to
// resolve deferred (mcp__) schemas. Matches claude-code-sourcemap's
// ToolSearchTool.ts:304 spec shape: a single `query` string + optional
// `max_results` cap.
//
// The description is intentionally minimal — it explains the query
// grammar but does NOT list tool names. Deferred names appear separately
// as their own (schema-stripped) entries in tools[], so the model can
// already see "what's available" without paying for ~500 tokens of
// repeated names here. claude-code uses the same split: meta-tool
// description = syntax help only; per-tool placeholder entries =
// names + short hint.
func toolSearchSpec(allSpecs []llm.ToolSpec) llm.ToolSpec {
	deferred := 0
	for _, s := range allSpecs {
		if strings.HasPrefix(s.Name, "mcp__") {
			deferred++
		}
	}
	desc := fmt.Sprintf(
		"Fetches full schema definitions for deferred tools so they can be called.\n\n"+
			"Deferred tools appear by name in the tools list with '[schema lazy]' in their description "+
			"but no parameter schema. Call this BEFORE invoking such a tool — otherwise the tool call "+
			"will fail with an invalid-params error. Currently %d MCP tool(s) are deferred.\n\n"+
			"Query forms:\n"+
			"  \"select:Read,Edit,Grep\" — fetch these exact tools by name\n"+
			"  \"notebook jupyter\" — keyword search, up to max_results best matches\n"+
			"  \"+slack send\" — require \"slack\" in the name, rank by remaining terms\n\n"+
			"Result format:\n"+
			"  select: form returns {matches:[{name,description,input_schema},...], missing:[...]}\n"+
			"  keyword form returns {matches:[{name,description},...]} — follow up with select:<name>",
		deferred,
	)
	return llm.ToolSpec{
		Name:        "ToolSearch",
		Description: desc,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Query to find deferred tools. Use \"select:<tool_name>\" for direct selection, or keywords to search.",
				},
				"max_results": map[string]any{
					"type":        "number",
					"description": fmt.Sprintf("Maximum number of results to return (default: %d)", DefaultToolSearchResults),
				},
			},
			"required": []string{"query"},
		},
	}
}
