package tui

// cmd_phase_f.go — Phase F slash commands. Smaller, more discoverable
// surfaces; the heavy lifters (background tasks via Ctrl+B, voice mode
// pipeline, daemon attach) are tracked separately.
//
// What lands here:
//   /thinkback   — surface the last assistant turn's thinking trace
//   /ultraplan   — guided "deep plan" prompt: bumps effort to high
//                   and pre-loads the planning frame
//   /onboarding  — interactive setup recap (auth → config → first prompt)
//
// Each is small enough that combining them in one file beats spawning
// six near-empty siblings of cmd_phase_c.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// cmdThinkback walks the conversation history backwards looking for
// the most recent assistant message that carried a "thinking" block.
// Many providers don't persist thinking into Content — only as
// transient EventThinkingDelta — so the typical answer is "no thinking
// captured for this turn". We surface that explicitly rather than
// silently empty so the user knows the feature works as designed.
func cmdThinkback(r *REPL, args string) string {
	if r == nil || r.Loop == nil {
		return "(no active session)"
	}
	hist := r.Loop.History()
	for i := len(hist) - 1; i >= 0; i-- {
		m := hist[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, c := range m.Content {
			// Anthropic's content type is "thinking"; some adapters
			// (Gemini reasoning) use "thought" or "reasoning". Accept
			// any plausible name and let the caller see the body.
			t := strings.ToLower(c.Type)
			if t == "thinking" || t == "thought" || t == "reasoning" {
				body := strings.TrimSpace(c.Text)
				if body == "" {
					continue
				}
				return renderInfoBox("Last Thinking", []infoRow{
					{Key: "", Value: body},
				})
			}
		}
		// Found the most recent assistant turn — stop, even if it had
		// no thinking. /thinkback is "the LAST one", not "any prior".
		break
	}
	return "(no thinking trace captured for the most recent turn — " +
		"some providers don't persist thinking into Content. " +
		"Streaming display still showed it; it just isn't on disk.)"
}

// cmdUltraplan is sugar over /plan: nudges effort to high and emits
// the deeper-planning prompt frame. Doesn't yet wire a "plan-signal"
// pipeline (the agent loop has no PlanMode handler today); v1 is a
// guided message the user submits as the next prompt. Once /plan
// gets a structured planner agent, /ultraplan becomes its high-effort
// alias.
func cmdUltraplan(r *REPL, args string) string {
	if r != nil && r.Loop != nil {
		r.Loop.SetEffort(llm.EffortHigh)
	}
	target := strings.TrimSpace(args)
	if target == "" {
		target = "the task I just described"
	}
	return "ultraplan: effort=high enabled. Now describe the goal, then prompt:\n" +
		"  'Produce an ultra-detailed plan for " + target + ". " +
		"Include: file/component dependency graph, sequencing rationale, " +
		"a risk register (likelihood × impact), test strategy at unit / " +
		"integration / e2e tiers, rollback plan, and explicit success " +
		"criteria. Number each step; cite files by path:line.'"
}

// cmdOnboarding gives a structured first-run recap. claude-code has a
// genuine setup wizard; metis has `metis auth login`, `metis config
// init`, etc. — this command stitches them into one bordered cheat
// sheet so a new user knows where to start.
func cmdOnboarding(r *REPL, args string) string {
	cfgPath := filepath.Join(config.Home(), "config.toml")
	authPath := filepath.Join(config.Home(), "auth.json")
	cwdPart := "/Users/<you>/<project>"
	if cwd := getCwd(); cwd != "" {
		cwdPart = cwd
	}
	rows := []infoRow{
		{Key: "1. credentials", Value: "metis auth login [provider]"},
		{Key: "", Value: "  stores key under " + authPath},
		{Key: "", Value: ""},
		{Key: "2. project doc", Value: "/init   (this chat)"},
		{Key: "", Value: "  scaffolds CLAUDE.md so the model knows the conventions"},
		{Key: "", Value: ""},
		{Key: "3. config", Value: cfgPath},
		{Key: "", Value: "  edit defaults: provider, model, theme, statusline"},
		{Key: "", Value: ""},
		{Key: "4. talk!", Value: "type anything, or /help for commands"},
		{Key: "", Value: "  cwd today: " + cwdPart},
		{Key: "", Value: ""},
		{Key: "skill drop-ins", Value: "drop a SKILL.md under ~/.metis/skills/<name>/"},
		{Key: "", Value: "see /skills info <name> for a worked example"},
	}
	if r != nil && r.Loop != nil && r.Loop.Provider != nil {
		rows = append(rows, infoRow{Key: "", Value: ""},
			infoRow{Key: "current provider", Value: r.Loop.Provider.Name() + " · " + r.Loop.Model})
	}
	return renderInfoBox("Welcome to metis", rows)
}

// getCwd is a tiny wrapper so cmdOnboarding doesn't import os twice
// (cmd_phase_f.go already pulls config / strings; the cmd_session_ops
// helpers handle absolute paths in a different cmd, hence this local).
func getCwd() string {
	wd, _ := os.Getwd()
	return wd
}

// fmtSize is currently unused; reserved for the future "show metrics
// in onboarding" follow-up.
var _ = fmt.Sprintf
