package builtin

// memory.go — LLM-facing Memory tool.
//
// Rebuilt 2026-04-30 to fix the critical disconnect the memory audit
// caught: the previous version wrote to `~/.metis/memories/MEMORY.md`
// flat-files, while `MemoryManager.BuildContext()` (the thing that
// actually injects memory into the next turn's system prompt) read
// from `<sessionDir>/memory/core.d/*.md`. Two stores, never reconciled
// — every Memory.add silently dropped on the floor for system-prompt
// purposes.
//
// New design: the tool is a thin wrapper over MemoryManager's Repository API.
// Core block read-modify-write operations reload authoritative state while
// holding the cross-process repository lock; searches hit the same archival
// passages.jsonl that has BM25 + LLM-rerank already wired.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Memory is the LLM-facing tool. It keeps the Repository contract rather than
// a concrete *memory.MemoryManager so Desktop can bind the loop and this tool
// to one shared, atomically re-targetable workspace view. Writes therefore
// always flow through the same repository BuildContext reads, including after
// an in-process session switch crosses workspace boundaries.
type Memory struct {
	tools.BaseTool
	gate              *permission.Gate
	mm                memory.Repository
	sourceSessionIDFn func() string
}

// NewMemory is the runtime constructor — mirrors NewAgent /
// NewSendMessage / NewSkill / NewScheduleWakeup. Passing a nil
// manager doesn't crash; Execute returns a clear error so the LLM
// knows the capability is unavailable.
func NewMemory(gate *permission.Gate, mm memory.Repository) Memory {
	if mm != nil {
		value := reflect.ValueOf(mm)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				mm = nil
			}
		}
	}
	return Memory{gate: gate, mm: mm}
}

// WithSourceSessionIDFn returns a copy that attributes session-owned archival
// writes to the currently active conversation. Core user/system blocks remain
// deliberately global preferences; session deletion only removes repository
// rows whose provenance names that session.
func (m Memory) WithSourceSessionIDFn(fn func() string) Memory {
	m.sourceSessionIDFn = fn
	return m
}

func (Memory) Name() string { return "Memory" }
func (Memory) Description() string {
	return "Persistent memory storage. Writes through the agent's MemoryManager so changes appear in the next turn's system prompt."
}
func (Memory) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (m Memory) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "add | replace | remove | read | search | stats | archive",
				"enum":        []string{"add", "replace", "remove", "read", "search", "stats", "archive"},
			},
			"target": map[string]any{
				"type":        "string",
				"description": "Block label for add/replace/remove/read: user (preferences), system (identity), working (current task), summary (rolling summary). Default: user.",
				"enum":        []string{"user", "system", "working", "summary"},
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Content to add/replace/archive.",
			},
			"match": map[string]any{
				"type":        "string",
				"description": "Substring to find (replace/remove).",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Search query for unified archival and topic memory (BM25F-ranked).",
			},
			"limit": map[string]any{
				"type":        "number",
				"description": "Max results for search (default 10, max 50).",
			},
			"memory_type": map[string]any{
				"type":        "string",
				"description": "For 'archive' / 'search': passage type. user=durable identity/preferences; feedback=corrections to remember; project=current-repo context (may go stale); reference=external resource pointers; context=generic.",
				"enum":        []string{"user", "feedback", "project", "reference", "context"},
			},
			"tags": map[string]any{
				"type":        "array",
				"description": "Tags to attach to an archived passage (or filter by during search). Curated keywords; weighted higher than content text in BM25F ranking.",
				"items":       map[string]any{"type": "string"},
			},
		},
	}
}

func (m Memory) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	// Memory writes (add/replace/remove) mutate the system prompt for
	// every following turn — a real side effect. Route through the
	// gate so plan mode correctly blocks them and ask mode prompts.
	// Pre-fix this always returned PermissionAllow, which made
	// `/mode plan` silently let the model rewrite memory blocks — the
	// 2026-05-18 plan-mode audit caught this.
	//
	// Read-only actions (search/recall/list) currently go through the
	// same Ask path in acceptEdits because Memory.IsReadOnly hardcodes
	// false; a future refactor can make IsReadOnly input-aware to
	// auto-allow those. The conservative default here is correct.
	if m.gate == nil {
		return tools.PermissionAllow, "memory operations (no gate)"
	}
	action, _ := in["action"].(string)
	d, _ := m.gate.Check(ctx, "Memory", action)
	return mapDecision(d), "memory:" + action
}

func (m Memory) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if m.mm == nil {
		return &tools.Result{
			Output:  "Memory: manager not initialized (this is a metis bug — please report)",
			IsError: true,
		}, nil
	}
	action, _ := in["action"].(string)
	target, _ := in["target"].(string)
	content, _ := in["content"].(string)
	match, _ := in["match"].(string)

	if target == "" {
		target = "user"
	}
	// Reject unknown blocks early so the LLM gets a clear hint instead
	// of silently writing to a created-on-the-fly block that
	// BuildContext doesn't render.
	switch target {
	case "user", "system", "working", "summary":
		// ok
	default:
		return &tools.Result{
			Output:  "Memory: unknown target " + target + " — must be user|system|working|summary",
			IsError: true,
		}, nil
	}

	switch action {
	case "add":
		return m.add(target, content)
	case "replace":
		return m.replace(target, match, content)
	case "remove":
		return m.remove(target, match)
	case "read":
		return m.read(target)
	case "search":
		query, _ := in["query"].(string)
		limit := intOrDefault(in["limit"], 10, 50)
		mtype, _ := in["memory_type"].(string)
		return m.search(query, mtype, limit)
	case "archive":
		mtype, _ := in["memory_type"].(string)
		tags := stringsFromAny(in["tags"])
		return m.archive(content, mtype, tags, m.sourceSessionID())
	case "stats":
		return m.stats()
	}
	return &tools.Result{
		Output:  "Memory: unknown action " + action + " — use add|replace|remove|read|search|archive|stats",
		IsError: true,
	}, nil
}

// add appends content to an existing block. Pure append (with a
// newline separator if the block was non-empty) — replaces the block
// only when it was empty. This matches user mental model: "add" =
// "remember this in addition to what's there."
func (m Memory) add(target, content string) (*tools.Result, error) {
	if content == "" {
		return &tools.Result{Output: "Memory add: content required", IsError: true}, nil
	}
	block, err := m.mm.AddCoreBlock(target, content)
	if err != nil {
		return &tools.Result{Output: "Memory add: " + err.Error(), IsError: true}, nil
	}
	return &tools.Result{Output: fmt.Sprintf("added to %s memory (now %d chars)", target, len(block.Content))}, nil
}

// replace swaps the FIRST occurrence of `match` with `content`. Plain
// substring replace — no regex, no fuzzy. Caller must include enough
// context in `match` to disambiguate.
func (m Memory) replace(target, match, content string) (*tools.Result, error) {
	if match == "" {
		return &tools.Result{Output: "Memory replace: match required", IsError: true}, nil
	}
	if content == "" {
		return &tools.Result{Output: "Memory replace: content required", IsError: true}, nil
	}
	_, err := m.mm.ReplaceCoreBlock(target, match, content)
	if errors.Is(err, memory.ErrCoreMatchNotFound) {
		return &tools.Result{Output: "Memory replace: match not found in " + target + " block", IsError: true}, nil
	}
	if err != nil {
		return &tools.Result{Output: "Memory replace: " + err.Error(), IsError: true}, nil
	}
	return &tools.Result{Output: "replaced in " + target + " memory"}, nil
}

// remove deletes the FIRST occurrence of `match`. Like replace but
// the new value is empty — deliberately surfaced as a separate action
// so the LLM doesn't have to construct an empty `content` field.
func (m Memory) remove(target, match string) (*tools.Result, error) {
	if match == "" {
		return &tools.Result{Output: "Memory remove: match required", IsError: true}, nil
	}
	_, err := m.mm.RemoveCoreBlock(target, match)
	if errors.Is(err, memory.ErrCoreMatchNotFound) {
		return &tools.Result{Output: "Memory remove: match not found in " + target + " block", IsError: true}, nil
	}
	if err != nil {
		return &tools.Result{Output: "Memory remove: " + err.Error(), IsError: true}, nil
	}
	return &tools.Result{Output: "removed from " + target + " memory"}, nil
}

func (m Memory) read(target string) (*tools.Result, error) {
	blk, err := m.mm.ReadCoreBlock(target)
	if errors.Is(err, memory.ErrCoreBlockNotFound) {
		return &tools.Result{Output: "(" + target + " block doesn't exist)", IsError: true}, nil
	}
	if err != nil {
		return &tools.Result{Output: "Memory read: " + err.Error(), IsError: true}, nil
	}
	body := blockText(blk)
	if body == "" {
		return &tools.Result{Output: "(" + target + " memory empty)"}, nil
	}
	return &tools.Result{Output: body + "\n\n[block " + target + ": " + fmt.Sprintf("%d chars", len(body)) + "]"}, nil
}

// search hits the unified archival + topic corpus (BM25F-ranked passages). Optional
// memory_type filter narrows to one of user/feedback/project/
// reference/context. Empty type = all types.
func (m Memory) search(query, memoryType string, limit int) (*tools.Result, error) {
	if query == "" {
		return &tools.Result{Output: "Memory search: query required", IsError: true}, nil
	}
	opts := memory.SearchOptions{
		Query:  query,
		Limit:  limit,
		SortBy: "relevance",
	}
	if memoryType != "" {
		if !memory.IsKnownType(memoryType) {
			return &tools.Result{
				Output:  "Memory search: unknown memory_type " + memoryType + " — use user|feedback|project|reference|context",
				IsError: true,
			}, nil
		}
		opts.Types = []string{memoryType}
	}
	results, err := m.mm.Search(opts)
	if err != nil {
		return &tools.Result{Output: "Memory search: " + err.Error(), IsError: true}, nil
	}
	if len(results) == 0 {
		return &tools.Result{Output: "no memory results for: " + query}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Memory search for %q (%d hits):\n\n", query, len(results))
	for i, p := range results {
		typeLabel := p.Type
		if typeLabel == "" {
			typeLabel = "context"
		}
		fmt.Fprintf(&b, "%d. [%s · %s] %s\n", i+1, p.CreatedAt, typeLabel, p.Content)
	}
	return &tools.Result{Output: b.String()}, nil
}

// archive writes a durable Passage to archival memory. Used by the
// LLM (via Memory.archive(content, memory_type=user, tags=[...])) and
// by the auto-distillation pipeline (calls Archival().Insert directly,
// not through this tool wrapper).
//
// Defaults memory_type to "context" when caller doesn't specify — keeps
// backwards compat and avoids errors on minimal inputs.
func (m Memory) archive(content, memoryType string, tags []string, sourceSessionID string) (*tools.Result, error) {
	if content == "" {
		return &tools.Result{Output: "Memory archive: content required", IsError: true}, nil
	}
	if memoryType == "" {
		memoryType = memory.TypeContext
	}
	if !memory.IsKnownType(memoryType) {
		return &tools.Result{
			Output:  "Memory archive: unknown memory_type " + memoryType + " — use user|feedback|project|reference|context",
			IsError: true,
		}, nil
	}
	p := memory.Passage{
		Content:         content,
		Type:            memoryType,
		Tags:            tags,
		Source:          "memory-tool",
		SourceSessionID: sourceSessionID,
	}
	archival := m.mm.Archival()
	if archival == nil {
		return &tools.Result{Output: "Memory archive: memory repository is unavailable", IsError: true}, nil
	}
	if err := archival.Insert(p); err != nil {
		return &tools.Result{Output: "Memory archive: " + err.Error(), IsError: true}, nil
	}
	return &tools.Result{
		Output: fmt.Sprintf("archived passage (type=%s, %d tags, %d chars)",
			memoryType, len(tags), len(content)),
	}, nil
}

func (m Memory) sourceSessionID() string {
	if m.sourceSessionIDFn == nil {
		return ""
	}
	return strings.TrimSpace(m.sourceSessionIDFn())
}

// stringsFromAny coerces a JSON-decoded any into a []string. The LLM
// usually emits arrays as []any of strings; bare strings get wrapped.
func stringsFromAny(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	}
	return nil
}

// stats reports per-block usage so the LLM can tell when blocks are
// approaching their character limits and proactively summarize.
func (m Memory) stats() (*tools.Result, error) {
	stats, err := m.mm.CoreBlockStats()
	if err != nil {
		return &tools.Result{Output: "Memory stats: " + err.Error(), IsError: true}, nil
	}
	type row struct {
		Label string `json:"label"`
		Used  int    `json:"used"`
		Limit int    `json:"limit"`
	}
	labels := []string{"user", "system", "working", "summary"}
	out := make([]row, 0, len(labels))
	for _, label := range labels {
		blockStats, ok := stats[label]
		if !ok {
			continue
		}
		out = append(out, row{Label: label, Used: blockStats.Used, Limit: blockStats.Limit})
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	stamp := time.Now().Format(time.RFC3339)
	return &tools.Result{Output: "memory stats (" + stamp + "):\n" + string(data)}, nil
}

func blockText(b *memory.Block) string {
	if b == nil {
		return ""
	}
	return b.Content
}

// intOrDefault clamps an LLM-provided number param to a sensible
// range with a default. The LLM tends to send floats (JSON numbers
// have no int/float distinction), so we accept both.
func intOrDefault(v any, def, max int) int {
	var n int
	switch x := v.(type) {
	case float64:
		n = int(x)
	case int:
		n = x
	case int64:
		n = int(x)
	default:
		return def
	}
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
