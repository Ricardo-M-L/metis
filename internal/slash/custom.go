// Package slash — custom user-authored commands.
//
// User-authored commands live as .md files under two locations:
//
//	~/.metis/commands/<name>.md            (per-user, loaded for every session)
//	<cwd>/.metis/commands/<name>.md        (per-project, overrides per-user)
//
// Each file becomes a slash command. The filename (sans .md) is the
// command name. The file body is treated as a prompt template — when
// the user types `/<name> arg1 arg2 ...`, the template is rendered
// (with $ARGUMENTS / $1 / $2 substitution) and the result becomes the
// next user message in the chat. Mirrors claude-code's per-user
// commands convention so users keep one set of recipes across both
// agents.
//
// Optional YAML-ish front matter (between two `---` lines at the very
// top) sets the description shown in `/help`:
//
//	---
//	description: Run our standard pre-commit checks
//	---
//	Run go test ./..., golangci-lint, and report only failures.
//	Focus on $ARGUMENTS if provided.
//
// Without front matter, the description defaults to the first non-blank
// body line truncated to 80 chars.
package slash

import (
	"os"
	"path/filepath"
	"strings"
)

// LoadCustomCommands scans the conventional locations and registers each
// .md file as a Cmd. Project-local files override per-user files of the
// same name (last-Register wins via the index map). Errors reading any
// individual file are silently skipped — a typo'd front matter or a
// non-readable file should never block startup.
//
// Returns the list of registered command names so callers can log /
// surface the inventory.
func LoadCustomCommands(r *Registry, homeDir string) []string {
	var loaded []string
	// Per-user FIRST, then project-local SECOND so project entries
	// override on name collision. The Registry's Register appends to
	// cmds AND overwrites index[name], so the project-local handler
	// wins on /name dispatch.
	if homeDir != "" {
		loaded = append(loaded, scanDir(r, filepath.Join(homeDir, "commands"), "user")...)
	}
	if cwd, err := os.Getwd(); err == nil {
		loaded = append(loaded, scanDir(r, filepath.Join(cwd, ".metis", "commands"), "project")...)
	}
	return loaded
}

// scanDir reads <dir>/*.md and registers each. `source` is annotated
// onto the description so `/help` users can tell user vs project.
func scanDir(r *Registry, dir, source string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // dir missing is the common case — silent
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		// Hidden / OS junk — same filter as the skills loader uses.
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		// Reject names that would collide with built-ins or the
		// reserved leading-`/` prefix. Built-in dispatch happens via
		// the index map — re-Register() with same name silently shadows
		// the built-in, which is footgun-shaped. We refuse the load
		// instead.
		if _, exists := r.Get(base); exists {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		desc, template := parseFrontMatter(string(body))
		if desc == "" {
			desc = firstNonBlankLine(template, 80)
		}
		desc = "[" + source + "] " + desc
		// Capture template by value so each Cmd's closure has its own
		// copy (loop iteration shares variable scope otherwise).
		tmpl := template
		r.Register(Cmd{
			Name:        base,
			Description: desc,
			Handler: func(args string) (string, Signal) {
				return renderTemplate(tmpl, args), SignalCustomPrompt
			},
		})
		names = append(names, base)
	}
	return names
}

// parseFrontMatter strips a `---\n...\n---\n` block at the very top and
// extracts a `description:` field if present. Returns (description, body).
// We deliberately don't pull in a YAML parser — only `description:` is
// honored, anything else is preserved as part of the body so the
// template still sees it (matches CC's tolerant parsing).
func parseFrontMatter(s string) (description, body string) {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", s
	}
	rest := s[4:]
	if strings.HasPrefix(s, "---\r\n") {
		rest = s[5:]
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", s
	}
	header := rest[:end]
	body = strings.TrimLeft(rest[end+4:], "\r\n")
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "description:") {
			_, val, _ := cut(line, ":")
			description = strings.TrimSpace(val)
			break
		}
	}
	return description, body
}

func firstNonBlankLine(s string, maxLen int) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if len(ln) > maxLen {
			ln = ln[:maxLen-1] + "…"
		}
		return ln
	}
	return "(custom command)"
}

// renderTemplate substitutes:
//
//	$ARGUMENTS  → the entire arg string
//	$1, $2, …   → individual whitespace-split args
//
// Out-of-range positional refs ($3 when only 2 args supplied) render
// to empty string — same as bash. We intentionally don't escape; the
// template author owns the body content, and the result is sent to
// the LLM (not a shell), so injection isn't a concern.
func renderTemplate(template, args string) string {
	out := strings.ReplaceAll(template, "$ARGUMENTS", args)
	parts := strings.Fields(args)
	for i, p := range parts {
		out = strings.ReplaceAll(out, "$"+itoa(i+1), p)
	}
	// Remaining unresolved positional refs → blank.
	for i := len(parts) + 1; i <= 9; i++ {
		out = strings.ReplaceAll(out, "$"+itoa(i), "")
	}
	return strings.TrimSpace(out)
}

// itoa avoids strconv import for the single-digit positional arg case.
func itoa(n int) string {
	if n < 0 || n > 9 {
		// Defensive: positional refs cap at $9 anyway. Anything else
		// returns empty so we don't miscompile a $10 reference.
		return ""
	}
	return string(rune('0' + n))
}
