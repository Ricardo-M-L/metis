package agent

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// RepeatGuard watches an agent's stream of tool calls, counts runs of
// consecutive calls to the same tool with identical canonicalized
// arguments, and at configured run lengths returns an escalating advisory
// reminder for the model to stop repeating itself (DSH repeat-tool-reminder
// parity). It is advisory: it never vetoes, rewrites, or delays a call —
// the decision to change approach or conclude stays with the model.
//
// A reminder is an injected user-message text block the loop appends next
// to the batch's tool_results, so it is model-visible and reconstructable
// from the session log. The chain key is (tool name, canonical arguments);
// canonicalization deep-sorts object keys then JSON-marshals, so argument
// objects differing only in property order count as identical.
//
// Semantics follow the DSH reference:
//   - A call identical to the previous tracked call increments the
//     consecutive counter; a different tracked call resets it to 1.
//   - Untracked calls (matched by include/exclude) are transparent: they
//     neither increment nor reset the counter.
//   - The chain is in-memory only and per-Loop (per-agent): a sub-agent
//     runs its own Loop and therefore its own chain.
type RepeatGuard struct {
	thresholds            []int
	include               []string
	exclude               []string
	argumentsPreviewChars int

	// chain state — one consecutive counter for the current (tool, args).
	curKey   string
	curCount int
}

// RepeatGuardConfig holds the user-visible configuration for the repeat
// reminder guard. Zero values fall back to the DSH defaults.
type RepeatGuardConfig struct {
	// Thresholds are the consecutive counts that trigger a reminder. The
	// FIRST delivers a short generic nudge; every later one delivers the
	// detailed form naming the tool, run length, and canonical arguments.
	Thresholds []int
	// Include holds tool-name patterns to track; empty ⇒ all tools.
	Include []string
	// Exclude holds tool-name patterns transparent to the chain.
	Exclude []string
	// ArgumentsPreviewChars caps the arguments quoted in the detailed
	// reminder (chain comparison always uses the full canonical string).
	ArgumentsPreviewChars int
}

const repeatGuardFirstNudge = "You are repeating the exact same tool call with identical arguments. Carefully analyze the previous result before calling again: if the task is not complete, try a different approach or different arguments instead of repeating the call."

// NewRepeatGuard builds a guard from cfg, applying the DSH defaults for
// zero-valued fields. An invalid thresholds list (a value below 2, a
// duplicate, or empty) is normalized by dropping invalid entries rather
// than failing — metis keeps configuration lenient at runtime, unlike the
// DSH plugin's load-time loud failure.
func NewRepeatGuard(cfg RepeatGuardConfig) *RepeatGuard {
	g := &RepeatGuard{
		thresholds:            []int{3, 5, 8},
		exclude:               []string{"TodoWrite"},
		argumentsPreviewChars: 500,
	}
	if len(cfg.Thresholds) > 0 {
		seen := map[int]bool{}
		normalized := make([]int, 0, len(cfg.Thresholds))
		for _, t := range cfg.Thresholds {
			if t >= 2 && !seen[t] {
				seen[t] = true
				normalized = append(normalized, t)
			}
		}
		if len(normalized) > 0 {
			sort.Ints(normalized)
			g.thresholds = normalized
		}
	}
	if len(cfg.Include) > 0 {
		g.include = append([]string(nil), cfg.Include...)
	}
	if len(cfg.Exclude) > 0 {
		g.exclude = append([]string(nil), cfg.Exclude...)
	}
	if cfg.ArgumentsPreviewChars >= 1 {
		g.argumentsPreviewChars = cfg.ArgumentsPreviewChars
	}
	return g
}

// tracked reports whether the tool name participates in the chain under
// the include/exclude predicates. Patterns support `*` wildcards (via
// path.Match semantics — `*` matches any run of non-separator characters).
func (g *RepeatGuard) tracked(tool string) bool {
	match := func(pats []string) bool {
		for _, p := range pats {
			ok, err := path.Match(p, tool)
			if err == nil && ok {
				return true
			}
		}
		return false
	}
	if len(g.include) > 0 && !match(g.include) {
		return false
	}
	if match(g.exclude) {
		return false
	}
	return true
}

// canonicalArgs deep-sorts object keys and JSON-marshals so that argument
// maps differing only in property order are identical. Scalar/array values
// marshal directly; a marshal failure falls back to a stable string.
func canonicalArgs(in map[string]any) string {
	var normalize func(v any) any
	normalize = func(v any) any {
		if m, ok := v.(map[string]any); ok {
			out := make(map[string]any, len(m))
			for k, mv := range m {
				out[k] = normalize(mv)
			}
			return out
		}
		if s, ok := v.([]any); ok {
			out := make([]any, len(s))
			for i, sv := range s {
				out[i] = normalize(sv)
			}
			return out
		}
		return v
	}
	b, err := json.Marshal(normalize(in))
	if err != nil {
		return fmt.Sprintf("%#v", in)
	}
	return string(b)
}

// thresholdIndex returns the index of the threshold this count equals, or
// -1 when count is not exactly a configured threshold. Later thresholds
// (index >= 1) use the detailed form.
func (g *RepeatGuard) thresholdIndex(count int) int {
	for i, t := range g.thresholds {
		if t == count {
			return i
		}
	}
	return -1
}

// RecordStep consumes one tool batch (in dispatch order) and returns the
// reminder to inject, or "" when no threshold was crossed. The first
// threshold returns the short generic nudge; every later one returns the
// detailed form. Reset must be called at the start of a fresh user turn.
func (g *RepeatGuard) RecordStep(toolUses []llm.ContentBlock) string {
	if g == nil {
		return ""
	}
	reminder := ""
	for _, tu := range toolUses {
		name := tu.ToolName
		if !g.tracked(name) {
			continue // transparent to the chain
		}
		key := name + "\x00" + canonicalArgs(tu.ToolInput)
		if key == g.curKey {
			g.curCount++
		} else {
			g.curKey = key
			g.curCount = 1
		}
		if idx := g.thresholdIndex(g.curCount); idx >= 0 {
			if idx == 0 {
				reminder = repeatGuardFirstNudge
			} else {
				reminder = g.detailedReminder(name, g.curCount, tu.ToolInput)
			}
		}
	}
	return reminder
}

// detailedReminder renders the later-threshold form with a head-truncated
// canonical argument preview. The preview ends "… (+<n> more chars)" when
// the full canonical string exceeds argumentsPreviewChars.
func (g *RepeatGuard) detailedReminder(tool string, count int, in map[string]any) string {
	full := canonicalArgs(redactedToolInput(in))
	preview := full
	omitted := 0
	if len(preview) > g.argumentsPreviewChars {
		omitted = len(preview) - g.argumentsPreviewChars
		preview = preview[:g.argumentsPreviewChars]
	}
	marker := ""
	if omitted > 0 {
		marker = fmt.Sprintf(" \u2026 (+%d more chars)", omitted)
	}
	return fmt.Sprintf(
		"Repeated tool call detected:\n- tool: %s\n- consecutive_calls: %d\n- arguments: %s%s\n\nStop repeating this call. Re-read its last result, then either change your approach, use different arguments, or conclude the task.",
		tool, count, preview, marker,
	)
}

// Reset clears the chain at a fresh user turn (DSH agent/pre-step reset).
func (g *RepeatGuard) Reset() {
	if g == nil {
		return
	}
	g.curKey = ""
	g.curCount = 0
}
