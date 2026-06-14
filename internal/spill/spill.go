// Package spill persists oversized tool results to disk at ingestion
// time, so a single huge tool_result never enters the model context
// wholesale. The model receives a stub with a short preview and the
// file path; it recovers the full content via Read on demand.
//
// This is the ingestion-time counterpart to the Compactor's
// Microcompact tier (internal/agent/compact.go), which offloads
// RETROACTIVELY once the context passes the snip threshold — by then
// a 500 KB Bash dump has already burned a full turn of window. Spill
// intercepts at the dispatch layer instead, the moment the result is
// produced.
//
// Mirrors claude-code's toolResultStorage.ts (maybePersistLargeToolResult
// + buildLargeToolResultMessage): same tool-results store, 2k preview,
// and write-once-per-tool_use_id idempotency via O_EXCL.
package spill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// PreviewChars is how much of the original content the stub carries
// inline. Matches claude-code's PREVIEW_SIZE_BYTES (toolResultStorage.ts:109).
const PreviewChars = 2000

// Result describes one persisted tool result.
type Result struct {
	Path         string
	OriginalSize int
	Preview      string
	HasMore      bool
}

// Store persists content under dir/<toolUseID>.txt and returns the
// stub ingredients. The write uses O_EXCL: tool_use_id is unique per
// invocation and content is deterministic for a given id, so an
// existing file means a prior turn already persisted it — fall through
// to the preview without rewriting (claude-code's 'wx' flag pattern).
func Store(dir, toolUseID, content string) (*Result, error) {
	if dir == "" {
		return nil, fmt.Errorf("spill: no directory configured")
	}
	if toolUseID == "" {
		return nil, fmt.Errorf("spill: empty tool_use_id")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// ".spill.txt" suffix, NOT bare "<id>.txt": spill shares its
	// directory with the Compactor's Microcompact offload, which
	// writes "<id>.txt" via os.WriteFile (truncating). If both keyed
	// the same filename, a Microcompact pass over the spill stub
	// (possible if MicrocompactMinChars is ever tuned below the stub
	// size) would truncate the full content this file holds — a
	// self-referential data loss (2026-06-11 review). Separate
	// namespaces keep the two offload paths from clobbering each other.
	path := filepath.Join(dir, SanitizeID(toolUseID)+".spill.txt")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, werr := f.WriteString(content)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			os.Remove(path) // partial write is worse than no cache
			if werr == nil {
				werr = cerr
			}
			return nil, werr
		}
	} else if !os.IsExist(err) {
		return nil, err
	}
	preview, hasMore := makePreview(content, PreviewChars)
	return &Result{
		Path:         path,
		OriginalSize: len(content),
		Preview:      preview,
		HasMore:      hasMore,
	}, nil
}

// Stub renders the model-facing replacement body. Wording follows the
// Microcompact stub ("use Read tool with this exact path") so the model
// learns one recovery idiom for both offload paths.
func (r *Result) Stub() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[tool output too large (%d chars). Full output saved to: %s — use Read tool with this exact path to recover the full content]\n\n", r.OriginalSize, r.Path)
	b.WriteString("Preview:\n")
	b.WriteString(r.Preview)
	return b.String()
}

// errorMarkers (lowercase) trigger tail-preservation when found in the
// part of the output that head-only truncation would drop. Compiler /
// test / stack-trace failures put their verdict at the very END, so a
// pure-head preview is exactly where it goes blind.
var errorMarkers = []string{
	"error", "exception", "traceback", "panic", "fatal",
	"failed", "fail:", "segfault", "assert", "--- fail",
}

// makePreview builds the in-context preview of oversized content. It is
// head-only by default, but when the dropped tail carries error signal
// it keeps head (70%) + tail (30%) so the model still sees the failure.
// Mirrors MiMo-Code's error-aware truncation (Claude Code, by contrast,
// truncates head-only and loses tail errors). All cuts respect UTF-8
// rune boundaries (CJK tool output is common). Returns the fully
// formatted preview (truncation markers included) plus whether any
// truncation happened.
func makePreview(content string, limit int) (string, bool) {
	if len(content) <= limit {
		return content, false
	}
	if hasErrorTail(content, limit) {
		head := cutRuneSafe(content, limit*7/10)
		tail := cutRuneSafeFromEnd(content, limit-len(head))
		omitted := len(content) - len(head) - len(tail)
		return fmt.Sprintf(
			"%s\n\n... [%d chars omitted; tail kept below — it contains error output] ...\n\n%s",
			head, omitted, tail), true
	}
	head := cutRuneSafe(content, limit)
	return fmt.Sprintf("%s\n\n... [%d chars truncated] ...", head, len(content)-len(head)), true
}

// hasErrorTail reports whether the portion of content that pure-head
// truncation would discard (content[limit:]) contains an error marker.
// Only the dropped region matters: an error already inside the head is
// visible without keeping the tail.
func hasErrorTail(content string, limit int) bool {
	if limit >= len(content) {
		return false
	}
	return HasErrorMarker(content[limit:])
}

// HasErrorMarker reports whether text contains a recognized error /
// failure marker (case-insensitive). The single source of truth for
// "is this output a failure?" — shared by spill's tail preservation and
// Bash's capped-output tail ring (internal/tools/builtin/bash), so both
// offload paths agree on what's worth keeping.
func HasErrorMarker(text string) bool {
	low := strings.ToLower(text)
	for _, m := range errorMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// cutRuneSafe returns the first n bytes of s without splitting a rune.
func cutRuneSafe(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutRuneSafeFromEnd returns roughly the last n bytes of s without
// splitting a rune at the start of the slice.
func cutRuneSafeFromEnd(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// SanitizeID keeps tool_use_ids filesystem-safe. Provider IDs are
// normally [A-Za-z0-9_-], but MCP/OpenAI-compat ids have shown slashes
// in the wild; map anything suspicious to '_'. Exported so the
// Compactor's Microcompact offload (internal/agent/compact.go) keys its
// cache files identically — a slash-bearing id would otherwise make its
// os.WriteFile target a non-existent subdirectory and silently skip the
// offload.
func SanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, id)
}
