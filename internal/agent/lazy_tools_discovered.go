package agent

// lazy_tools_discovered.go — tracks which MCP tools the model has
// already fetched the schema for, so lazy mode doesn't force the
// model to re-fetch them after compaction.
//
// Why this exists: stripAndAppendToolSearch normally replaces every
// mcp__* schema with a placeholder. If the model fetched a schema in
// turn 3 and then compaction runs in turn 12 (clearing the verbose
// tool_result), turn 13's tools[] would re-strip that tool — the
// model would have to call ToolSearch again for a tool it just used.
// We track the "already discovered" set in-memory on Loop and let
// stripAndAppendToolSearch preserve those schemas.
//
// Mirrors openclaude's cross-conversation discovery scanning
// (restored-src/src/utils/toolSearch.ts:545-592), adapted for our
// synthetic ToolSearch tool_use/tool_result pairs since metis doesn't
// use Anthropic's beta `tool_reference` blocks.

import (
	"encoding/json"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// markDeferredDiscovered records that a Deferred tool's schema has been
// delivered to the model. Exposure metadata is authoritative; the mcp__ name
// fallback lives in Registry for compatibility with older plugin tools.
func (l *Loop) markDeferredDiscovered(name string) {
	if l == nil || l.Registry == nil {
		return
	}
	entry, ok := l.Registry.GetModelEntry(name)
	if !ok || entry.Exposure != tools.ToolExposureDeferred {
		return
	}
	l.markDiscoveredName(name)
}

// markMCPDiscovered is retained for internal compatibility while callers move
// to exposure terminology.
func (l *Loop) markMCPDiscovered(name string) {
	if l == nil || !strings.HasPrefix(name, "mcp__") {
		return
	}
	l.markDiscoveredName(name)
}

func (l *Loop) markDiscoveredName(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.discoveredMCP == nil {
		l.discoveredMCP = make(map[string]bool, 4)
	}
	l.discoveredMCP[name] = true
}

// snapshotDiscoveredMCP returns a read-only copy of the discovered set
// for use by stripAndAppendToolSearch. Returns nil when nothing has
// been discovered yet (saves the caller a len-check on every spec).
// Auto-hydrates from message history on first call.
func (l *Loop) snapshotDiscoveredMCP() map[string]bool {
	l.ensureDiscoveredHydrated()
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.discoveredMCP) == 0 {
		return nil
	}
	out := make(map[string]bool, len(l.discoveredMCP))
	for k := range l.discoveredMCP {
		out[k] = true
	}
	return out
}

// ensureDiscoveredHydrated rebuilds the discovered set from message
// history on first access. Walks Messages for assistant tool_use
// blocks invoking ToolSearch + the matching user tool_result, parses
// each successful payload's matches[].name into the set.
//
// Lazy + once: idempotent re-call returns early via the hydrated
// flag, so the hot path (toolSpecs() per turn) does one cheap RLock
// after the first turn.
//
// Why scan messages rather than persist to disk: the set is implicit
// in the conversation already. A resumed session loads Messages, and
// any ToolSearch payload that survived compaction tells us "the
// model already saw schema X". No extra file to keep in sync.
func (l *Loop) ensureDiscoveredHydrated() {
	l.mu.RLock()
	hydrated := l.discoveredMCPHydrated
	l.mu.RUnlock()
	if hydrated {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.discoveredMCPHydrated {
		return
	}
	l.discoveredMCPHydrated = true
	if l.discoveredMCP == nil {
		l.discoveredMCP = make(map[string]bool, 4)
	}
	candidates := make(map[string]bool)
	rebuildDiscoveredToolNamesFromMessages(candidates, l.Messages)
	for name := range candidates {
		entry, ok := l.Registry.GetModelEntry(name)
		if (ok && entry.Exposure == tools.ToolExposureDeferred) || (!ok && strings.HasPrefix(name, "mcp__")) {
			l.discoveredMCP[name] = true
		}
	}
}

// rebuildDiscoveredMCPFromMessages walks one pass of a message slice
// and adds every "matches[].name" from successful ToolSearch results
// to set. Exported (lowercase) for tests.
//
// Algorithm:
//
//	for each assistant message:
//	    for each tool_use with ToolName == "ToolSearch":
//	        find the next user message containing a tool_result with
//	        the matching tool_use_id; on hit, parse matches[].name.
//
// A tool_result with IsError==true is skipped (no schemas delivered).
// Malformed JSON is also skipped silently — the original turn would
// have errored out for the model, so there's no schema to preserve.
func rebuildDiscoveredMCPFromMessages(set map[string]bool, msgs []llm.Message) {
	candidates := make(map[string]bool)
	rebuildDiscoveredToolNamesFromMessages(candidates, msgs)
	for name := range candidates {
		if strings.HasPrefix(name, "mcp__") {
			set[name] = true
		}
	}
}

func rebuildDiscoveredToolNamesFromMessages(set map[string]bool, msgs []llm.Message) {
	if len(msgs) == 0 {
		return
	}
	// Build a tool_use_id -> result-text index across user messages.
	// Linear scan is fine; this only runs once per session.
	resultByID := make(map[string]llm.ContentBlock)
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				resultByID[b.ToolUseID] = b
			}
		}
	}
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type != "tool_use" || b.ToolName != "ToolSearch" {
				continue
			}
			res, ok := resultByID[b.ToolUseID]
			if !ok || res.IsError {
				continue
			}
			parseDeferredNamesFromResult(set, res.ToolResult)
		}
	}
}

// parseMCPNamesFromResult extracts mcp__ tool names from a ToolSearch
// tool_result body. Accepts both the new {matches:[...]} envelope and
// any future variations that still nest the names under "matches".
func parseMCPNamesFromResult(set map[string]bool, body string) {
	candidates := make(map[string]bool)
	parseDeferredNamesFromResult(candidates, body)
	for name := range candidates {
		if strings.HasPrefix(name, "mcp__") {
			set[name] = true
		}
	}
}

func parseDeferredNamesFromResult(set map[string]bool, body string) {
	if body == "" {
		return
	}
	var parsed struct {
		Matches []struct {
			Name             string         `json:"name"`
			AlreadyAvailable bool           `json:"already_available"`
			InputSchema      map[string]any `json:"input_schema"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return
	}
	for _, m := range parsed.Matches {
		// Keyword search returns names and descriptions only. A tool becomes
		// discovered only after select: actually delivered its schema.
		if m.Name != "" && !m.AlreadyAvailable && m.InputSchema != nil {
			set[m.Name] = true
		}
	}
}
