package builtin

// History — search the FULL session transcript, including messages that
// auto-compaction (Snip / Collapse) has since summarized away from the
// live context. metis's compaction is lossy in-memory, but the session
// JSONL on disk keeps every turn verbatim; this tool reads that and
// gives the model a way to recover an exact past message it no longer
// has in context.
//
// Mirrors MiMo-Code's `history` tool (search + around). Claude Code has
// no equivalent — it relies on its structured summary + tool-result
// spill to cover the same ground; metis now has all three.
//
// The tool takes a loadFn closure (injected by runtime) rather than a
// session.Store directly, so this package stays free of a session-store
// dependency and the search is trivially testable with a fake loader.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

const (
	historyDefaultLimit  = 8
	historyDefaultAround = 3
	historySnippetChars  = 240
)

// History is the LLM-facing transcript-search tool.
type History struct {
	tools.BaseTool
	// loadFn returns the full session transcript (verbatim, pre-
	// compaction). nil or an error → the tool reports it cleanly rather
	// than crashing.
	loadFn func() ([]llm.Message, error)
}

// NewHistory builds the tool. loadFn is injected by runtime and wraps
// session.Store.Load(CurrentSessionID()).
func NewHistory(loadFn func() ([]llm.Message, error)) *History {
	return &History{loadFn: loadFn}
}

func (History) Name() string { return "History" }

func (History) Description() string {
	return `Search the full conversation transcript — including earlier messages that auto-compaction has summarized away from your current context. Use this when you need the EXACT wording of something from earlier (a path, an error, a decision, what the user literally said) that the running summary only paraphrases.

operation "search": full-text rank over every past message. Returns the top matches with their transcript index, role, and a snippet.
operation "around": given a transcript index (from a prior search), return the surrounding messages so you can read the exchange in context.

Typical flow: search for a keyword → note the index of the right hit → around that index to read the full exchange.`
}

func (History) ShortDescription() string {
	return `Search the full conversation transcript (including compacted-away messages) for the exact wording of a past message. operation: "search" {query} or "around" {index}.`
}

func (History) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"operation"},
		"properties": map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"enum":        []string{"search", "around"},
				"description": "search: rank past messages by relevance to `query`. around: return messages surrounding `index`.",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Search terms (for operation=search).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max matches to return (search). Default 8.",
			},
			"index": map[string]any{
				"type":        "integer",
				"description": "Transcript index to center on (for operation=around), as returned by a prior search.",
			},
			"before": map[string]any{
				"type":        "integer",
				"description": "Messages to include before `index` (around). Default 3.",
			},
			"after": map[string]any{
				"type":        "integer",
				"description": "Messages to include after `index` (around). Default 3.",
			},
		},
	}
}

func (History) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

// IsReadOnly: History only reads the on-disk transcript — no side
// effects, safe to fan out and to allow without a permission prompt.
func (History) IsReadOnly(map[string]any) bool { return true }

func (h History) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}

func (h History) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if h.loadFn == nil {
		return &tools.Result{Output: "History is unavailable: no session transcript is wired up.", IsError: true}, nil
	}
	msgs, err := h.loadFn()
	if err != nil {
		return &tools.Result{Output: "History: failed to load the transcript: " + err.Error(), IsError: true}, nil
	}
	op, _ := in["operation"].(string)
	switch op {
	case "search":
		return h.search(msgs, in), nil
	case "around":
		return h.around(msgs, in), nil
	default:
		return &tools.Result{Output: fmt.Sprintf("History: unknown operation %q (want \"search\" or \"around\").", op), IsError: true}, nil
	}
}

func (h History) search(msgs []llm.Message, in map[string]any) *tools.Result {
	query, _ := in["query"].(string)
	if strings.TrimSpace(query) == "" {
		return &tools.Result{Output: "History search: `query` is required.", IsError: true}
	}
	limit := intArg(in, "limit", historyDefaultLimit)
	if limit <= 0 {
		limit = historyDefaultLimit
	}

	// Build BM25 docs keyed by transcript index so we can map a hit
	// back to its position for a follow-up `around`.
	docs := make([]*memory.BM25Doc, 0, len(msgs))
	for i, m := range msgs {
		text, _ := messageSearchText(m)
		if strings.TrimSpace(text) == "" {
			continue
		}
		docs = append(docs, memory.NewBM25Doc(fmt.Sprintf("%d", i), text))
	}
	ranked := memory.BM25FRank(query, docs)

	type hit struct {
		Index   int     `json:"index"`
		Role    string  `json:"role"`
		Kind    string  `json:"kind"`
		Score   float64 `json:"score"`
		Snippet string  `json:"snippet"`
	}
	out := make([]hit, 0, limit)
	for _, r := range ranked {
		if len(out) >= limit {
			break
		}
		idx := atoiSafe(r.ID)
		if idx < 0 || idx >= len(msgs) {
			continue
		}
		text, kind := messageSearchText(msgs[idx])
		out = append(out, hit{
			Index:   idx,
			Role:    string(msgs[idx].Role),
			Kind:    kind,
			Score:   r.Score,
			Snippet: snippet(text, historySnippetChars),
		})
	}
	if len(out) == 0 {
		return &tools.Result{Output: fmt.Sprintf("History search: no messages matched %q (transcript has %d messages).", query, len(msgs))}
	}
	body, _ := json.Marshal(map[string]any{
		"transcript_len": len(msgs),
		"matches":        out,
	})
	return &tools.Result{Output: string(body)}
}

func (h History) around(msgs []llm.Message, in map[string]any) *tools.Result {
	idx := intArg(in, "index", -1)
	if idx < 0 || idx >= len(msgs) {
		return &tools.Result{Output: fmt.Sprintf("History around: `index` %d is out of range (transcript has %d messages, valid 0..%d).", idx, len(msgs), len(msgs)-1), IsError: true}
	}
	before := intArg(in, "before", historyDefaultAround)
	after := intArg(in, "after", historyDefaultAround)
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	lo := idx - before
	if lo < 0 {
		lo = 0
	}
	hi := idx + after
	if hi >= len(msgs) {
		hi = len(msgs) - 1
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Transcript messages %d..%d (centered on %d):\n\n", lo, hi, idx)
	for i := lo; i <= hi; i++ {
		text, kind := messageSearchText(msgs[i])
		marker := "  "
		if i == idx {
			marker = "» "
		}
		fmt.Fprintf(&b, "%s[%d] %s (%s):\n%s\n\n", marker, i, msgs[i].Role, kind, text)
	}
	return &tools.Result{Output: b.String()}
}

// messageSearchText flattens a message into searchable text and a
// coarse kind label. tool_use contributes its name + input; tool_result
// its content; text blocks their text.
func messageSearchText(m llm.Message) (text, kind string) {
	kind = string(m.Role)
	var parts []string
	for _, blk := range m.Content {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				parts = append(parts, blk.Text)
			}
		case "tool_use":
			kind = "tool_use"
			if blk.ToolName != "" {
				if raw, err := json.Marshal(blk.ToolInput); err == nil {
					parts = append(parts, blk.ToolName+" "+string(raw))
				} else {
					parts = append(parts, blk.ToolName)
				}
			}
		case "tool_result":
			kind = "tool_result"
			if blk.ToolResult != "" {
				parts = append(parts, blk.ToolResult)
			}
		}
	}
	return strings.Join(parts, "\n"), kind
}

// snippet returns the first n chars of text on a single line.
func snippet(text string, n int) string {
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) <= n {
		return flat
	}
	cut := n
	for cut > 0 && flat[cut]&0xC0 == 0x80 { // don't split a UTF-8 rune
		cut--
	}
	return flat[:cut] + "…"
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	if s == "" {
		return -1
	}
	return n
}
