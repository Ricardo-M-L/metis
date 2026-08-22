package tui

// at_session.go — `@session:<id-or-prefix-or-title>` text-reference
// expansion (DSH cross-session reference parity).
//
// The user types `@session:yesterday-debug what did we conclude` and the
// TUI resolves the reference against the session store, builds a bounded
// digest of that session via builtin.SessionDigest (the same shape the
// Sessions tool's read op returns), strips the @ref from the visible
// text, and appends the digest as a text block before AppendUserBlocks.
//
// Resolution mirrors the tool's resolver: exact id → unique id prefix →
// unique title substring. Ambiguous or unmatched refs degrade to a
// warning + the @ref stays verbatim (same convention as @file images).

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

// atSessionPattern matches `@session:<ref>` where ref is a non-space
// token (session id, id prefix, or a title slug — use `-` or `_`
// instead of spaces in titles).
var atSessionPattern = regexp.MustCompile(`(?:^|\s)@session:([A-Za-z0-9_\-\.]+)`)

// expandAtSession scans text for @session: refs, strips the
// successfully-resolved ones, and returns the rewritten text + digest
// text blocks + per-ref errors. Read-only over the store.
func expandAtSession(text string, listFn func(limit int) ([]builtin.SessionInfo, error), loadFn func(id string) ([]llm.Message, error)) (string, []llm.ContentBlock, []string) {
	matches := atSessionPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil, nil
	}
	infos, err := listFn(100)
	if err != nil {
		// Store unavailable: leave every @session: ref verbatim.
		return text, nil, []string{"@session: session store unavailable (" + err.Error() + ")"}
	}

	var (
		out    strings.Builder
		blocks []llm.ContentBlock
		errs   []string
	)
	cursor := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		rawRef := text[m[2]:m[3]]
		matchStart := start
		if matchStart < len(text) && (text[matchStart] == ' ' || text[matchStart] == '\t' || text[matchStart] == '\n') {
			matchStart++
		}

		id := builtin.ResolveSessionRef(infos, rawRef)
		if id == "" {
			out.WriteString(text[cursor:end])
			errs = append(errs, fmt.Sprintf("@session:%s: no unique match (use /resume to list sessions)", rawRef))
			cursor = end
			continue
		}
		var info builtin.SessionInfo
		for _, si := range infos {
			if si.ID == id {
				info = si
				break
			}
		}
		msgs, err := loadFn(id)
		if err != nil {
			out.WriteString(text[cursor:end])
			errs = append(errs, fmt.Sprintf("@session:%s: %v", rawRef, err))
			cursor = end
			continue
		}
		out.WriteString(text[cursor:matchStart])
		blocks = append(blocks, llm.ContentBlock{
			Type: "text",
			Text: builtin.SessionDigest(info, msgs, -1, 6000),
		})
		cursor = end
	}
	out.WriteString(text[cursor:])
	return out.String(), blocks, errs
}
