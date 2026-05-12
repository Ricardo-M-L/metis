package runtime

// subdir_hints.go — when the user @-mentions a path that sits BELOW cwd
// (e.g. `@services/foo/bar.go` from a repo root), gather any per-directory
// CLAUDE.md / AGENTS.md / METIS.md / .metis/CLAUDE.md along the descent
// and emit them as an attachment prepended to the user message.
//
// Pairs with loadProjectContext (system_prompt.go), which walks UP from
// cwd to the git root. That covers ancestors; this covers descendants —
// together they mirror claude-code's getMemoryFilesForNestedDirectory
// (utils/claudemd.ts) which walks an explicit path segment list.
//
// Key design choice: attachments, not system-prompt mutation. The system
// prompt is cached at the LLM (Anthropic prompt cache, 5-min TTL); if we
// re-rendered it on every user turn to splice in subdir hints, every
// @-mention would invalidate the cache. By appending the hint block to
// the user message instead, the cache stays warm and only the volatile
// user-message segment carries the (typically tiny) hint payload.
//
// Dedup: hints share realpath keys with loadProjectContext through a
// caller-supplied `seen` map, so a CLAUDE.md that's already in the
// system prompt (because cwd's ancestor chain picked it up) doesn't
// double-emit in the user message.

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// CollectSubdirHints scans userText for @-mention path tokens, walks
// from cwd to each mentioned path's directory (down-walk only — paths
// above cwd are already handled by loadProjectContext), and returns
// an XML-wrapped block of every CLAUDE.md / AGENTS.md / METIS.md /
// .metis/CLAUDE.md found along the way.
//
// Returns "" when no hints applied. Cheap on hot path: reads ≤ depth
// files per @-mention, dedups by realpath across all mentions, skips
// dirs that fail to stat.
//
// alreadyLoaded is the set of realpaths the system prompt already
// included (passed in by the caller so we don't re-emit them). Pass
// nil when standalone.
func CollectSubdirHints(userText, cwd string, alreadyLoaded map[string]bool) string {
	if userText == "" || cwd == "" {
		return ""
	}
	mentions := extractAtMentionPaths(userText)
	if len(mentions) == 0 {
		return ""
	}
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	// Use realpath on both sides so /tmp ↔ /private/tmp (and other
	// symlink farms) doesn't make every rel path look like it
	// escapes cwd. canonicalPath is also what `seen` keys against,
	// so the dedup map stays coherent.
	cwdReal := canonicalPath(cwdAbs)
	seen := make(map[string]bool, len(alreadyLoaded)+8)
	for k, v := range alreadyLoaded {
		seen[k] = v
	}
	var blocks []string
	for _, m := range mentions {
		// Resolve mention relative to cwd. Absolute paths are accepted
		// but only descended into when they sit under cwd — escaping
		// cwd would let an @-mention pull arbitrary CLAUDE.md from
		// elsewhere on disk, which is both noisy and a (minor) info-
		// leak.
		var target string
		if filepath.IsAbs(m) {
			target = m
		} else {
			target = filepath.Join(cwdAbs, m)
		}
		targetAbs, err := filepath.Abs(target)
		if err != nil {
			continue
		}
		// Realpath both sides so /tmp ↔ /private/tmp doesn't make
		// every rel path look like it escapes cwd. canonicalPath
		// is idempotent on already-real paths so no harm if cwd
		// was already canonical.
		targetReal := canonicalPath(targetAbs)
		// Only walk strictly below cwd. `==` cwd means the file is in
		// cwd itself, whose hints are already in the system prompt.
		rel, err := filepath.Rel(cwdReal, targetReal)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		// Walk from cwd → directory containing the target file,
		// loading hint files at every intermediate level.
		dir := filepath.Dir(targetReal)
		blocks = append(blocks, walkSubdirHints(cwdReal, dir, seen)...)
	}
	if len(blocks) == 0 {
		return ""
	}
	// Wrap the whole thing in one outer tag so the model can spot the
	// boundary at a glance. Inner per-file <project_context> blocks
	// preserve the same shape loadProjectContext uses for its own
	// blocks — uniform format across the prompt.
	var b strings.Builder
	b.WriteString("<subdirectory_hints>\n")
	b.WriteString("The user's message references paths below the current working directory. ")
	b.WriteString("These per-directory hint files describe local conventions for those subtrees.\n\n")
	for _, blk := range blocks {
		b.WriteString(blk)
		b.WriteString("\n\n")
	}
	b.WriteString("</subdirectory_hints>")
	return b.String()
}

// walkSubdirHints walks descending from rootReal to leafDir (inclusive
// of both endpoints, but rootReal usually contributes nothing because
// alreadyLoaded already lists it). At every intermediate dir it tries
// the standard hint filenames; first hit wins per directory so a
// project that uses both CLAUDE.md and AGENTS.md in the same dir
// doesn't double-emit.
//
// Returns one <project_context …> block per hint file found.
func walkSubdirHints(rootReal, leafDir string, seen map[string]bool) []string {
	leafAbs, err := filepath.Abs(leafDir)
	if err != nil {
		return nil
	}
	// Collect the segments between root (exclusive) and leaf (inclusive).
	rel, err := filepath.Rel(rootReal, leafAbs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	// Same dir as root — nothing to descend into.
	if rel == "." {
		return nil
	}
	segments := strings.Split(rel, string(filepath.Separator))
	dir := rootReal
	var out []string
	for _, seg := range segments {
		dir = filepath.Join(dir, seg)
		for _, name := range projectContextCandidates {
			path := filepath.Join(dir, name)
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			body := strings.TrimSpace(string(b))
			if body == "" {
				continue
			}
			canon := canonicalPath(path)
			if seen[canon] {
				continue
			}
			seen[canon] = true
			body = scanForInjection(body, path)
			out = append(out, "<project_context source=\""+path+"\">\n"+body+"\n</project_context>")
		}
	}
	return out
}

// extractAtMentionPaths returns every @-mention token in userText. We
// reuse the detect rule from internal/tui/atmention.go: `@` must be at
// start-of-string or preceded by whitespace, and the path runs until
// the next whitespace.
//
// We accept any path-looking token — caller filters out ones that
// don't resolve to a real file. Punctuation at the end (trailing `.`
// `,` `:` `;` `)` `]`) is trimmed so a sentence like "look at
// @cmd/main.go." doesn't lose the dot to the mention.
func extractAtMentionPaths(s string) []string {
	if !strings.ContainsRune(s, '@') {
		return nil
	}
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '@' {
			continue
		}
		if i > 0 {
			prev := s[i-1]
			if prev != ' ' && prev != '\n' && prev != '\t' {
				continue
			}
		}
		// Consume non-whitespace chars after @.
		j := i + 1
		for j < len(s) {
			r := rune(s[j])
			if unicode.IsSpace(r) {
				break
			}
			j++
		}
		tok := s[i+1 : j]
		tok = strings.TrimRight(tok, ".,:;)]}\"'")
		if tok != "" {
			out = append(out, tok)
		}
		i = j
	}
	return out
}
