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
	fmt.Fprintf(&b, "Preview (first %d chars):\n", PreviewChars)
	b.WriteString(r.Preview)
	if r.HasMore {
		b.WriteString("\n...")
	}
	return b.String()
}

// makePreview cuts content at limit without splitting a UTF-8 rune
// mid-sequence (CJK tool output is common here).
func makePreview(content string, limit int) (string, bool) {
	if len(content) <= limit {
		return content, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut], true
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
