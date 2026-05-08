package tui

// cmd_phase_c.go — claude-code-parity slash commands added in Phase C.
// Each handler is small; the goal is matching surface area, not
// re-architecting the chat loop. Where a richer LLM-driven version is
// the obvious follow-up (drafted commit messages, sanitized exports),
// the v1 here points the user at the manual workflow + leaves a TODO
// at the relevant call site.
//
// Registered from BuildREPLCommands at the bottom of commands.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// cmdCopy yanks the last N assistant messages and pushes them through
// the same OSC 52 + ~/.metis/clipboard.txt path the Ctrl+Y key already
// uses. /copy with no arg = last 1 message (matches Ctrl+Y); /copy 3 =
// concatenated last 3 assistant messages with `---` separators.
func cmdCopy(r *REPL, args string) string {
	n := 1
	if v := strings.TrimSpace(args); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			return "usage: /copy [N]  — N defaults to 1; counts most recent assistant replies"
		}
		n = parsed
	}
	if r.Loop == nil {
		return "(no active session)"
	}
	hist := r.Loop.History()
	collected := make([]string, 0, n)
	for i := len(hist) - 1; i >= 0 && len(collected) < n; i-- {
		m := hist[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		text := assembleAssistantText(m)
		if text == "" {
			continue
		}
		// Prepend so final order is oldest → newest.
		collected = append([]string{text}, collected...)
	}
	if len(collected) == 0 {
		return "(nothing to copy yet — type a message and let the model reply)"
	}
	body := strings.Join(collected, "\n\n---\n\n")
	writeClipboard(body)
	// Match the silent-copy norm — no chat row pollution. A single
	// confirm string is fine because slash-command output is a
	// dedicated, ephemeral row, not a chat message.
	if len(collected) == 1 {
		return fmt.Sprintf("(copied %d chars)", len(body))
	}
	return fmt.Sprintf("(copied %d assistant replies · %d chars total)", len(collected), len(body))
}

// assembleAssistantText flattens an assistant message's text blocks
// into a single string. Skips tool_use blocks (the structured calls
// have no copy-worthy text); tool_result blocks live on user-role
// messages and never enter this path.
func assembleAssistantText(m llm.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(c.Text)
			}
		}
	}
	return b.String()
}

// cmdCommitPushPR is the three-step sugar: git add -A → git commit -m
// <args> → git push → gh pr create. Each step prints its own result so
// the user can intervene mid-flow. Refuses when args is empty (a commit
// message is required by convention).
func cmdCommitPushPR(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "usage: /commit-push-pr <commit message>\n" +
			"  runs: git add -A → git commit -m \"<msg>\" → git push -u origin HEAD → gh pr create --fill"
	}
	steps := []struct {
		name string
		argv []string
	}{
		{"git add", []string{"git", "add", "-A"}},
		{"git commit", []string{"git", "commit", "-m", args}},
		{"git push", []string{"git", "push", "-u", "origin", "HEAD"}},
		{"gh pr create", []string{"gh", "pr", "create", "--fill"}},
	}
	var b strings.Builder
	for _, s := range steps {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, s.argv[0], s.argv[1:]...)
		out, err := cmd.CombinedOutput()
		cancel()
		fmt.Fprintf(&b, "── %s ──\n", s.name)
		if t := strings.TrimSpace(string(out)); t != "" {
			b.WriteString(t)
			b.WriteString("\n")
		}
		if err != nil {
			fmt.Fprintf(&b, "%s failed: %v — stopping pipeline\n", s.name, err)
			return b.String()
		}
	}
	b.WriteString("(commit-push-pr: all steps ok)")
	return b.String()
}

// cmdInsights aggregates session activity from ~/.metis/sessions/*.jsonl
// over the last N days (default 7). Counts turns / unique tools / error
// rates, plus the model mix. Honest about scope — this is a deterministic
// summary, not LLM analysis (the latter is a Phase F item if anyone
// asks for it).
func cmdInsights(r *REPL, args string) string {
	days := 7
	for _, tok := range strings.Fields(args) {
		if strings.HasPrefix(tok, "--days=") {
			if v, err := strconv.Atoi(strings.TrimPrefix(tok, "--days=")); err == nil && v > 0 {
				days = v
			}
		}
	}
	dir := filepath.Join(config.Home(), "sessions")
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "insights: " + err.Error()
	}
	var (
		sessions   int
		messages   int
		toolCalls  int
		toolErrors int
		modelMix   = map[string]int{}
	)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		sessions++
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			// Header line carries `model`.
			if model, ok := rec["model"].(string); ok && model != "" {
				modelMix[model]++
				continue
			}
			messages++
			if role, _ := rec["role"].(string); role == "assistant" {
				if content, ok := rec["content"].([]any); ok {
					for _, c := range content {
						cm, _ := c.(map[string]any)
						if t, _ := cm["type"].(string); t == "tool_use" {
							toolCalls++
						}
					}
				}
			}
			if role, _ := rec["role"].(string); role == "user" {
				if content, ok := rec["content"].([]any); ok {
					for _, c := range content {
						cm, _ := c.(map[string]any)
						if t, _ := cm["type"].(string); t == "tool_result" {
							if isErr, _ := cm["is_error"].(bool); isErr {
								toolErrors++
							}
						}
					}
				}
			}
		}
	}
	if sessions == 0 {
		return fmt.Sprintf("(no sessions modified in the last %d day(s) at %s)", days, dir)
	}
	rows := []infoRow{
		{Key: "window", Value: fmt.Sprintf("last %d day(s)", days)},
		{Key: "sessions", Value: strconv.Itoa(sessions)},
		{Key: "messages", Value: strconv.Itoa(messages)},
		{Key: "tool calls", Value: strconv.Itoa(toolCalls)},
		{Key: "tool errors", Value: strconv.Itoa(toolErrors)},
	}
	if len(modelMix) > 0 {
		// Stable order by count desc, then name asc.
		type kv struct {
			K string
			V int
		}
		pairs := make([]kv, 0, len(modelMix))
		for k, v := range modelMix {
			pairs = append(pairs, kv{k, v})
		}
		sort.Slice(pairs, func(i, j int) bool {
			if pairs[i].V != pairs[j].V {
				return pairs[i].V > pairs[j].V
			}
			return pairs[i].K < pairs[j].K
		})
		var parts []string
		for _, p := range pairs {
			parts = append(parts, fmt.Sprintf("%s × %d", p.K, p.V))
		}
		rows = append(rows, infoRow{Key: "models", Value: strings.Join(parts, ", ")})
	}
	return renderInfoBox("Session Insights", rows)
}

// cmdOutputStyle records the user's preferred output verbosity on the
// REPL. claude-code's three modes — minimal, full, streamlined — map
// to: full=streaming on + thinking visible (default), streamlined=
// thinking dropped + tool calls collapsed, minimal=streamlined+no
// markdown. The REPL.outputStyle field is the source of truth; the
// render pipeline reads from it where it's already wired (markdown
// toggling) and the streamlined-render gating lands in the follow-up
// pass that ports cmdRun's accumulator into the interactive path.
func cmdOutputStyle(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg == "" {
		state := r.outputStyle
		if state == "" {
			state = "full"
		}
		return renderInfoBox("Output Style", []infoRow{
			{Key: "current", Value: state},
			{Key: "", Value: ""},
			{Key: "/output-style full", Value: "default — streaming + thinking + per-tool rows"},
			{Key: "/output-style streamlined", Value: "drop thinking, collapse tool calls into summaries"},
			{Key: "/output-style minimal", Value: "streamlined + no-markdown rendering"},
		})
	}
	switch arg {
	case "full", "default":
		r.outputStyle = "full"
		r.UseMarkdown = true
		return "(output style: full)"
	case "streamlined":
		r.outputStyle = "streamlined"
		return "(output style: streamlined — thinking dropped, tool calls summarized [render gate pending])"
	case "minimal":
		r.outputStyle = "minimal"
		r.UseMarkdown = false
		return "(output style: minimal — streamlined + no-markdown)"
	}
	return "output-style: unknown '" + arg + "' — use: full | streamlined | minimal"
}

// cmdBreakCache documents how to force a fresh cache write. We don't
// reach into the provider's cache_control headers from a slash command
// (that's a Phase F provider-layer change); instead we tell the user
// the actual mechanics today: /compact, /clear, or restart with --bare.
//
// The benefit of the slash even in this read-only form is discovery —
// the user types /break-cache expecting something, and gets a precise
// recipe rather than an error.
func cmdBreakCache(r *REPL, args string) string {
	return renderInfoBox("Cache Refresh", []infoRow{
		{Key: "", Value: "metis sends `cache_control: ephemeral` on every"},
		{Key: "", Value: "request that supports it. To force a fresh cache write:"},
		{Key: "", Value: ""},
		{Key: "/compact", Value: "summarize history → new cache prefix"},
		{Key: "/clear", Value: "drop history entirely → fresh cache from turn 0"},
		{Key: "metis chat --bare", Value: "skip MCP/plugins; smaller prompt → different prefix"},
		{Key: "", Value: ""},
		{Key: "", Value: "Phase F adds an explicit `next request bypasses cache` toggle."},
	})
}

// cmdSecurityReview is a /review with an OWASP-flavored framing.
// claude-code parity: the same nudge style as cmdReview, just pre-
// loaded with a security checklist. The actual scan happens in the
// next turn via Bash (git diff) + LLM analysis.
func cmdSecurityReview(r *REPL, args string) string {
	target := strings.TrimSpace(args)
	if target == "" {
		target = "the staged changes (git diff --cached)"
	}
	return "security-review: prompt the model with: 'review " + target + " for security issues. " +
		"Check for: SQL injection, command injection, XSS, path traversal, hardcoded secrets, " +
		"insecure deserialization, weak crypto, race conditions, OWASP top 10 in general. " +
		"Cite exact lines.'"
}

// cmdFeedback is /bug under a more discoverable name. Same body —
// claude-code calls it /feedback and many users expect that.
func cmdFeedback(r *REPL, args string) string {
	return cmdBug(r, args)
}

// cmdInitGuided is unused (kept here so the registration site has a
// stable callable for the future "interactive scaffold" mode). The
// existing cmdInit already produces a CLAUDE.md template so we don't
// need a new handler — just register /onboarding as an alias to keep
// the slash surface honest.
//
// Phase F adds the actual interactive wizard at registration time;
// this stub is intentionally a small helper for then.
