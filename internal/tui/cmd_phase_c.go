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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
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

// cmdCommitPushPR loads a normal agent prompt instead of executing a hidden
// four-process pipeline. The resulting Git/Bash calls therefore pass through
// the same permission gate, OS sandbox, progress UI and error recovery as any
// other requested repository change.
func cmdCommitPushPR(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "usage: /commit-push-pr <commit message>"
	}
	prompt := "Prepare and publish the current repository changes safely. " +
		"First inspect git status and the complete diff. Do not stage unrelated user changes. " +
		"Run the relevant tests, stage only the intended files, commit with this exact message: " + fmt.Sprintf("%q", args) + ". " +
		"Then push the current branch and create a pull request with gh pr create --fill if no pull request already exists. " +
		"Stop and explain any test, authentication, push, or PR error; never force-push."
	if r != nil && r.InsertInput != nil {
		r.InsertInput(prompt)
		return "commit-push-pr: safe workflow loaded into input — review, then press Enter"
	}
	return "commit-push-pr: submit this prompt to run through the normal tool permission path:\n\n" + prompt
}

type sessionInsights struct {
	sessions   int
	messages   int
	toolCalls  int
	toolErrors int
	modelMix   map[string]int
}

// collectSessionInsights reads the current typed session envelope rather than
// treating each JSONL object as a top-level llm.Message. Store.Load also
// applies history_replace records, so the totals describe each session's
// current logical transcript instead of counting messages that /clear, /undo,
// or /rewind already removed.
func collectSessionInsights(store *session.Store, cutoff time.Time) (sessionInsights, error) {
	stats := sessionInsights{modelMix: make(map[string]int)}
	if store == nil {
		return stats, fmt.Errorf("session store unavailable")
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil {
		return stats, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		header, messages, err := store.Load(id)
		if err != nil || header == nil {
			continue
		}
		stats.sessions++
		stats.messages += len(messages)
		if model := strings.TrimSpace(header.Model); model != "" {
			stats.modelMix[model]++
		}
		for _, message := range messages {
			for _, block := range message.Content {
				switch block.Type {
				case "tool_use":
					stats.toolCalls++
				case "tool_result":
					if block.IsError {
						stats.toolErrors++
					}
				}
			}
		}
	}
	return stats, nil
}

// cmdInsights aggregates logical session activity over the last N days
// (default 7). It is deterministic local analysis, not an LLM-generated
// summary.
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
	store := &session.Store{Dir: dir}
	if r != nil && r.Session != nil {
		store = r.Session
		dir = store.Dir
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	stats, err := collectSessionInsights(store, cutoff)
	if err != nil {
		return "insights: " + err.Error()
	}
	if stats.sessions == 0 {
		return fmt.Sprintf("(no sessions modified in the last %d day(s) at %s)", days, dir)
	}
	rows := []infoRow{
		{Key: "window", Value: fmt.Sprintf("last %d day(s)", days)},
		{Key: "sessions", Value: strconv.Itoa(stats.sessions)},
		{Key: "messages", Value: strconv.Itoa(stats.messages)},
		{Key: "tool calls", Value: strconv.Itoa(stats.toolCalls)},
		{Key: "tool errors", Value: strconv.Itoa(stats.toolErrors)},
	}
	if len(stats.modelMix) > 0 {
		// Stable order by count desc, then name asc.
		type kv struct {
			K string
			V int
		}
		pairs := make([]kv, 0, len(stats.modelMix))
		for k, v := range stats.modelMix {
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

const (
	outputStyleFull        = "full"
	outputStyleStreamlined = "streamlined"
	outputStyleMinimal     = "minimal"
)

func normalizeOutputStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case outputStyleStreamlined:
		return outputStyleStreamlined
	case outputStyleMinimal:
		return outputStyleMinimal
	default:
		return outputStyleFull
	}
}

// cmdOutputStyle updates both the plain REPL state and, through the bridge
// installed by Model.asREPL, the live TUI model. The renderer consumes the
// same value in buildChatItems.
func cmdOutputStyle(r *REPL, args string) string {
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg == "" {
		state := normalizeOutputStyle(r.outputStyle)
		return renderInfoBox("Output Style", []infoRow{
			{Key: "current", Value: state},
			{Key: "", Value: ""},
			{Key: "/output-style full", Value: "default — streaming + thinking + per-tool rows"},
			{Key: "/output-style streamlined", Value: "drop thinking, collapse tool calls into summaries"},
			{Key: "/output-style minimal", Value: "streamlined + no-markdown rendering"},
		})
	}
	style := arg
	if style == "default" {
		style = outputStyleFull
	}
	switch style {
	case outputStyleFull:
		r.outputStyle = outputStyleFull
		r.UseMarkdown = true
		if r.ApplyOutputStyle != nil {
			r.ApplyOutputStyle(outputStyleFull)
		}
		return "(output style: full)"
	case outputStyleStreamlined:
		r.outputStyle = outputStyleStreamlined
		r.UseMarkdown = true
		if r.ApplyOutputStyle != nil {
			r.ApplyOutputStyle(outputStyleStreamlined)
		}
		return "(output style: streamlined — thinking hidden, tool calls summarized)"
	case outputStyleMinimal:
		r.outputStyle = outputStyleMinimal
		r.UseMarkdown = false
		if r.ApplyOutputStyle != nil {
			r.ApplyOutputStyle(outputStyleMinimal)
		}
		return "(output style: minimal — streamlined + no-markdown)"
	}
	return "output-style: unknown '" + arg + "' — use: full | streamlined | minimal"
}

// cmdBreakCache arms a one-shot flag on the agent loop so the next
// request injects a fresh nonce into the system prompt — that shifts
// the prompt prefix and forces the provider to write a new cache entry
// instead of reusing the prior one. The flag is consumed at buildRequest
// time, so it only affects the very next turn.
//
// Falls back to the documentation panel when the bridge closure is nil
// (headless readline REPL has no Model to reach into).
func cmdBreakCache(r *REPL, args string) string {
	if r != nil && r.BypassCache != nil {
		r.BypassCache()
		return "(cache bypass armed — next request will write a fresh cache entry)"
	}
	return renderInfoBox("Cache Refresh", []infoRow{
		{Key: "", Value: "metis sends `cache_control: ephemeral` on every"},
		{Key: "", Value: "request that supports it. To force a fresh cache write:"},
		{Key: "", Value: ""},
		{Key: "/compact", Value: "summarize history → new cache prefix"},
		{Key: "/clear", Value: "drop history entirely → fresh cache from turn 0"},
		{Key: "metis chat --bare", Value: "skip MCP/plugins; smaller prompt → different prefix"},
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
	prompt := buildReviewPrompt(target, true)
	if r != nil && r.InsertInput != nil {
		r.InsertInput(prompt)
		return "security-review: prompt loaded into input — review, then press Enter to send"
	}
	return "security-review: paste this into the prompt to start —\n\n" + prompt
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
