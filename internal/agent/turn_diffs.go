package agent

// turn_diffs.go — claude-code-style per-turn file change report.
//
// What this is: walk Loop.Messages, group tool_use events by USER
// PROMPT boundary (each user-text message starts a new "turn"),
// generate a unified-diff for every Edit / Write inside the turn,
// aggregate +N/-M line counts per file and per turn. Returns
// newest-turn-first so /diff lands on "what just happened?".
//
// Why this exists: mirrors claude-code's useTurnDiffs hook
// (restored-src/src/hooks/useTurnDiffs.ts). The earlier metis
// TouchedFiles report (touched_files.go) counted Read/Edit/Write
// frequency per path — useful but flat: no turn boundaries, no
// actual diff, no "the agent changed 12 lines in this turn"
// signal that the user needs to audit work.
//
// Implementation notes:
//
//   - Edit tool_use blocks carry the FULL `old` and `new` strings
//     on their input map. We regenerate the unified diff at /diff
//     time via udiff.Strings — same algorithm internal/tui/
//     render_tool.go::renderEditDiff already uses for the per-call
//     preview. No new storage; the message log IS the source.
//
//   - Write tool_use carries only `path` + `content`. Without the
//     pre-write content we can't form a true diff, so Write is
//     reported as a synthetic +full-content hunk (matching
//     claude-code's behaviour for the file-creation case). The
//     line count overcounts when Write overwrites an existing
//     file, but the user still sees WHICH file was touched and a
//     rough volume — and the actual current state is always one
//     `git diff` away below.
//
//   - Turn boundaries: a "user-text" message (Role=user with a
//     non-empty text block) opens a turn. tool_result messages
//     (also Role=user) do NOT count — they're parts of the
//     in-progress turn, not new prompts.

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	udiff "github.com/aymanbagabas/go-udiff"
)

// TurnFileDiff is one file's aggregated edit data within a turn.
type TurnFileDiff struct {
	Path         string
	IsNewFile    bool // true when first appearance is via Write
	LinesAdded   int
	LinesRemoved int
	// EditCount is how many separate Edit/Write tool calls touched
	// this file inside the turn. Useful for "the model retried 3
	// times" debugging.
	EditCount int
}

// TurnDiff is one user-prompt → assistant-tool-cycles unit.
type TurnDiff struct {
	// Index is 1-based, monotonically increasing in chronological
	// order (Index=1 is the oldest turn).
	Index              int
	UserPromptPreview  string                   // first ~60 chars of the user prompt
	Files              map[string]*TurnFileDiff // keyed by path
	FilesChanged       int
	TotalLinesAdded    int
	TotalLinesRemoved  int
	HasMessageMessages bool // turn had user text that wasn't a tool_result
}

// turnPromptPreviewLen is the length we trim user prompts to for the
// per-turn header. 60 chars is enough to disambiguate two different
// asks without dominating the /diff layout.
const turnPromptPreviewLen = 60

// TurnDiffs walks messages chronologically, opens a new TurnDiff on
// each non-tool-result user message, records every Edit / Write /
// NotebookEdit tool_use inside, and returns the list in newest-first
// order. Returns nil when no edits happened.
//
// The function does NOT mutate messages.
func TurnDiffs(messages []llm.Message) []TurnDiff {
	if len(messages) == 0 {
		return nil
	}
	var turns []TurnDiff
	var current *TurnDiff
	idx := 0
	// seenPaths tracks every path the agent has Read / Edit'd / Write'd
	// SO FAR in the chronological walk. A Write to a path NOT yet in
	// the set is flagged as a new-file creation. Pre-loaded before the
	// first turn opens so that paths the agent merely Read prior to
	// editing don't get flagged as "new" later.
	seenPaths := map[string]struct{}{}
	for _, m := range messages {
		// New turn boundary: a user-role message whose content has a
		// real text block AND is not just a tool_result echo. Tool
		// results live on the user role too in the Anthropic shape,
		// so we have to look at the actual block types.
		if m.Role == llm.RoleUser && hasNonToolResultText(m) {
			if current != nil && current.FilesChanged > 0 {
				turns = append(turns, *current)
			}
			idx++
			current = &TurnDiff{
				Index:              idx,
				UserPromptPreview:  firstUserText(m, turnPromptPreviewLen),
				Files:              make(map[string]*TurnFileDiff),
				HasMessageMessages: true,
			}
			continue
		}
		if current == nil {
			// Even before the first user-prompt turn opens, record
			// pre-turn Reads / Edits / Writes into seenPaths so a
			// turn-1 Write to a path the agent saw during system-
			// prompt boot or resume-replay isn't flagged "new".
			for _, b := range m.Content {
				if b.Type != "tool_use" {
					continue
				}
				if p := stringFieldAny(b.ToolInput, "path", "file_path"); p != "" {
					switch b.ToolName {
					case "Read", "Edit", "Write", "NotebookEdit":
						seenPaths[p] = struct{}{}
					}
				}
			}
			continue
		}
		// Collect Edit/Write/NotebookEdit inside the current turn.
		for _, b := range m.Content {
			if b.Type != "tool_use" {
				continue
			}
			path := stringFieldAny(b.ToolInput, "path", "file_path")
			if path == "" {
				continue
			}
			switch b.ToolName {
			case "Edit":
				oldS, _ := b.ToolInput["old"].(string)
				newS, _ := b.ToolInput["new"].(string)
				if oldS == "" && newS == "" {
					// Some external tools use the longer names.
					oldS, _ = b.ToolInput["old_string"].(string)
					newS, _ = b.ToolInput["new_string"].(string)
				}
				added, removed := countDiffLines(oldS, newS)
				accumulate(current, path, false, added, removed)
			case "Write":
				content, _ := b.ToolInput["content"].(string)
				lines := 0
				if content != "" {
					lines = strings.Count(content, "\n")
					// Final line without trailing newline still counts.
					if !strings.HasSuffix(content, "\n") {
						lines++
					}
				}
				// Treat Write as full-file creation (claude-code does
				// the same when structuredPatch is empty); see
				// useTurnDiffs.ts:170-180.
				_, prior := seenPaths[path]
				isNew := !prior
				accumulate(current, path, isNew, lines, 0)
			case "NotebookEdit":
				// We don't have content for the cell on the input map
				// in a generic way; record as 1 edit with 0/0 lines.
				accumulate(current, path, false, 0, 0)
			default:
				// Read / Glob / Grep — not edits, but record paths so
				// a later Write isn't mis-flagged as "new file".
				if b.ToolName == "Read" {
					seenPaths[path] = struct{}{}
				}
				continue
			}
			// Mark Edit/Write/NotebookEdit targets as seen for future
			// new-file classification.
			seenPaths[path] = struct{}{}
		}
	}
	if current != nil && current.FilesChanged > 0 {
		turns = append(turns, *current)
	}
	// Newest-first.
	reverseTurns(turns)
	return turns
}

// TurnDiffs is the Loop accessor; takes a read lock so the slice is
// stable for the caller. Safe to call while the agent loop runs.
func (l *Loop) TurnDiffs() []TurnDiff {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return TurnDiffs(l.Messages)
}

// --- helpers ---------------------------------------------------------------

func hasNonToolResultText(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return true
		}
	}
	return false
}

func firstUserText(m llm.Message, max int) string {
	for _, b := range m.Content {
		if b.Type != "text" {
			continue
		}
		t := strings.TrimSpace(b.Text)
		if t == "" {
			continue
		}
		// Collapse internal whitespace so multi-line prompts don't
		// blow up the preview row.
		t = strings.Join(strings.Fields(t), " ")
		if len(t) <= max {
			return t
		}
		// "…" is 3 bytes in UTF-8 → reserve room for it inside the
		// `max` budget. Without this the returned preview is
		// `max + 2` bytes long, which trips the cap pin test and
		// breaks layouts that assume the byte length is bounded.
		const ellipsis = "…"
		room := max - len(ellipsis)
		if room < 0 {
			room = 0
		}
		// Avoid cutting in the middle of a multi-byte rune.
		for room > 0 && (t[room]&0xC0) == 0x80 {
			room--
		}
		return t[:room] + ellipsis
	}
	return ""
}

func countDiffLines(oldS, newS string) (added, removed int) {
	if oldS == newS {
		return 0, 0
	}
	edits := udiff.Strings(oldS, newS)
	if len(edits) == 0 {
		return 0, 0
	}
	uout, err := udiff.ToUnifiedDiff("", "", oldS, edits, 0)
	if err != nil {
		return 0, 0
	}
	for _, h := range uout.Hunks {
		for _, ln := range h.Lines {
			switch ln.Kind {
			case udiff.Insert:
				added++
			case udiff.Delete:
				removed++
			}
		}
	}
	return added, removed
}

func accumulate(t *TurnDiff, path string, isNew bool, added, removed int) {
	entry, ok := t.Files[path]
	if !ok {
		entry = &TurnFileDiff{Path: path, IsNewFile: isNew}
		t.Files[path] = entry
		t.FilesChanged++
	}
	entry.LinesAdded += added
	entry.LinesRemoved += removed
	entry.EditCount++
	// If a later Write call promotes the file to "new", honour it
	// — useful when Edit failed (so file already exists from a
	// prior session) and the model fell back to Write.
	if isNew {
		entry.IsNewFile = true
	}
	t.TotalLinesAdded += added
	t.TotalLinesRemoved += removed
}

func reverseTurns(s []TurnDiff) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
