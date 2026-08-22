package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

// LoadConfigHooks reads HooksConfig from the loaded config.toml and
// registers each user-declared hook into the live HookRegistry. Mirrors
// claude-code's settings.json hook model — each lifecycle event maps to
// one or more shell commands that receive a JSON payload on stdin and
// (for PreToolUse) can return a JSON response on stdout to short-circuit
// or rewrite the tool call.
//
// First-pass scope: type="command" only. type="http", type="agent",
// type="prompt" are accepted in the schema but skipped here — adding
// them later is a non-breaking change.
//
// Failures (parse errors, missing commands) are logged to stderr but do
// not block startup; chat works even if a user's hook is broken.
func LoadConfigHooks(reg *pubhook.Registry, cfg *config.HooksConfig) {
	if cfg == nil || reg == nil {
		return
	}
	for _, h := range cfg.PreToolUse {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.PreToolUseHandler(func(ctx context.Context, tc pubhook.Context, in *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
			if !matchTool(spec.If, in.Tool, in.Input) {
				return nil
			}
			payload := map[string]any{
				"hook_event_name": "PreToolUse",
				"session_id":      tc.SessionID,
				"model":           tc.Model,
				"turn":            tc.Turn,
				"tool_name":       in.Tool,
				"tool_input":      in.Input,
			}
			out, code, err := runHookCommandWithCode(ctx, spec, payload)
			// Claude-code parity: exit code 49 means "halt the turn",
			// regardless of stdout. A user can `exit 49` from a one-line
			// shell hook without bothering to emit JSON. Honor it
			// whether or not stdout has additional structure.
			if code == 49 {
				mod := parsePreToolUseResponse(out)
				if mod == nil {
					mod = &pubhook.ModifiedPreToolUse{}
				}
				mod.Halt = true
				if mod.HaltReason == "" {
					mod.HaltReason = "halted by hook (exit 49)"
				}
				return mod
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "hook PreToolUse %s: %v\n", spec.Command, err)
				return nil
			}
			return parsePreToolUseResponse(out)
		}))
	}
	for _, h := range cfg.PostToolUse {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.PostToolUseHandler(func(ctx context.Context, tc pubhook.Context, in *pubhook.PostToolUse) {
			if !matchTool(spec.If, in.Tool, in.Input) {
				return
			}
			payload := map[string]any{
				"hook_event_name": "PostToolUse",
				"session_id":      tc.SessionID,
				"model":           tc.Model,
				"turn":            tc.Turn,
				"tool_name":       in.Tool,
				"tool_input":      in.Input,
				"output":          in.Output,
				"is_error":        in.IsError,
			}
			if _, err := runHookCommand(ctx, spec, payload); err != nil {
				fmt.Fprintf(os.Stderr, "hook PostToolUse %s: %v\n", spec.Command, err)
			}
		}))
	}
	for _, h := range cfg.SessionStart {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.SessionStartHandler(func(ctx context.Context, tc pubhook.Context, system, model string) {
			payload := map[string]any{
				"hook_event_name":   "SessionStart",
				"session_id":        tc.SessionID,
				"model":             model,
				"system":            system,
				"working_directory": cwdOrEmpty(),
			}
			if _, err := runHookCommand(ctx, spec, payload); err != nil {
				fmt.Fprintf(os.Stderr, "hook SessionStart %s: %v\n", spec.Command, err)
			}
		}))
	}
	for _, h := range cfg.SessionEnd {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.SessionEndHandler(func(ctx context.Context, tc pubhook.Context, msgCount int, stopReason string) {
			payload := map[string]any{
				"hook_event_name": "SessionEnd",
				"session_id":      tc.SessionID,
				"model":           tc.Model,
				"message_count":   msgCount,
				"stop_reason":     stopReason,
			}
			if _, err := runHookCommand(ctx, spec, payload); err != nil {
				fmt.Fprintf(os.Stderr, "hook SessionEnd %s: %v\n", spec.Command, err)
			}
		}))
	}
	for _, h := range cfg.UserPromptSubmit {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.UserPromptSubmitHandler(func(ctx context.Context, tc pubhook.Context, in *pubhook.UserPromptSubmit) *pubhook.ModifiedUserPromptSubmit {
			payload := map[string]any{
				"hook_event_name": "UserPromptSubmit",
				"session_id":      tc.SessionID,
				"model":           tc.Model,
				"prompt":          in.Prompt,
			}
			out, err := runHookCommand(ctx, spec, payload)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hook UserPromptSubmit %s: %v\n", spec.Command, err)
				return nil
			}
			return parseUserPromptResponse(out)
		}))
	}
	for _, h := range cfg.Notification {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.NotificationHandler(func(ctx context.Context, tc pubhook.Context, n *pubhook.Notification) {
			payload := map[string]any{
				"hook_event_name": "Notification",
				"session_id":      tc.SessionID,
				"level":           n.Level,
				"message":         n.Message,
			}
			if _, err := runHookCommand(ctx, spec, payload); err != nil {
				fmt.Fprintf(os.Stderr, "hook Notification %s: %v\n", spec.Command, err)
			}
		}))
	}
	for _, h := range cfg.PermissionRequest {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.PermissionRequestHandler(func(ctx context.Context, tc pubhook.Context, p *pubhook.PermissionRequest) *pubhook.ModifiedPermissionRequest {
			payload := map[string]any{
				"hook_event_name": "PermissionRequest",
				"session_id":      tc.SessionID,
				"tool_name":       p.Tool,
				"tool_input":      p.Input,
				"reason":          p.Reason,
			}
			out, err := runHookCommand(ctx, spec, payload)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hook PermissionRequest %s: %v\n", spec.Command, err)
				return nil
			}
			return parsePermissionResponse(out)
		}))
	}
	for _, h := range cfg.PermissionDenied {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.PermissionDeniedHandler(func(ctx context.Context, tc pubhook.Context, p *pubhook.PermissionDenied) {
			payload := map[string]any{
				"hook_event_name": "PermissionDenied",
				"session_id":      tc.SessionID,
				"tool_name":       p.Tool,
				"tool_input":      p.Input,
				"reason":          p.Reason,
			}
			if _, err := runHookCommand(ctx, spec, payload); err != nil {
				fmt.Fprintf(os.Stderr, "hook PermissionDenied %s: %v\n", spec.Command, err)
			}
		}))
	}
	for _, h := range cfg.CwdChanged {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.CwdChangedHandler(func(ctx context.Context, tc pubhook.Context, c *pubhook.CwdChanged) {
			payload := map[string]any{
				"hook_event_name": "CwdChanged",
				"session_id":      tc.SessionID,
				"old_cwd":         c.OldCwd,
				"new_cwd":         c.NewCwd,
			}
			if _, err := runHookCommand(ctx, spec, payload); err != nil {
				fmt.Fprintf(os.Stderr, "hook CwdChanged %s: %v\n", spec.Command, err)
			}
		}))
	}
	for _, h := range cfg.PreCompact {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.PreCompactHandler(func(ctx context.Context, tc pubhook.Context, p *pubhook.PreCompact) {
			payload := map[string]any{
				"hook_event_name":  "PreCompact",
				"session_id":       tc.SessionID,
				"model":            tc.Model,
				"trigger":          p.Trigger,
				"message_count":    p.MessageCount,
				"estimated_tokens": p.EstimatedTokens,
			}
			if _, err := runHookCommand(ctx, spec, payload); err != nil {
				fmt.Fprintf(os.Stderr, "hook PreCompact %s: %v\n", spec.Command, err)
			}
		}))
	}
	for _, h := range cfg.PostCompact {
		spec := h
		if !isCommandType(spec.Type) {
			continue
		}
		reg.Register(pubhook.PostCompactHandler(func(ctx context.Context, tc pubhook.Context, p *pubhook.PostCompact) *pubhook.ModifiedPostCompact {
			payload := map[string]any{
				"hook_event_name": "PostCompact",
				"session_id":      tc.SessionID,
				"model":           tc.Model,
				"trigger":         p.Trigger,
				"tier":            p.Tier,
				"before_messages": p.BeforeMessages,
				"after_messages":  p.AfterMessages,
				"before_tokens":   p.BeforeTokens,
				"after_tokens":    p.AfterTokens,
			}
			out, err := runHookCommand(ctx, spec, payload)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hook PostCompact %s: %v\n", spec.Command, err)
				return nil
			}
			// stdout may carry context to re-inject after the compact
			// boundary: {"additional_context": "..."} — the §28.11 P1
			// use case ("compact 后 inject 上下文"). Empty/garbage body
			// is a plain observer and injects nothing.
			return parsePostCompactResponse(out)
		}))
	}
}

// parseUserPromptResponse expects an empty body (proceed) or:
//
//	{"output": "synthesized reply"}            ← short-circuit assistant turn
//	{"modified_prompt": "rewritten prompt"}    ← rewrite then send to LLM
func parseUserPromptResponse(out []byte) *pubhook.ModifiedUserPromptSubmit {
	out = trimBOM(out)
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	var v struct {
		Output         string `json:"output"`
		ModifiedPrompt string `json:"modified_prompt"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil
	}
	mod := &pubhook.ModifiedUserPromptSubmit{ModifiedPrompt: v.ModifiedPrompt}
	if v.Output != "" {
		mod.Output = &pubhook.Output{Content: v.Output}
	}
	if mod.ModifiedPrompt == "" && mod.Output == nil {
		return nil
	}
	return mod
}

func parsePermissionResponse(out []byte) *pubhook.ModifiedPermissionRequest {
	out = trimBOM(out)
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	var v struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil
	}
	if v.Decision != "allow" && v.Decision != "deny" {
		return nil
	}
	return &pubhook.ModifiedPermissionRequest{Decision: v.Decision, Reason: v.Reason}
}

// parsePostCompactResponse expects an empty body (observer) or:
//
//	{"additional_context": "..."}   ← inject after the compact boundary
func parsePostCompactResponse(out []byte) *pubhook.ModifiedPostCompact {
	out = trimBOM(out)
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	var v struct {
		AdditionalContext string `json:"additional_context"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil
	}
	if strings.TrimSpace(v.AdditionalContext) == "" {
		return nil
	}
	return &pubhook.ModifiedPostCompact{AdditionalContext: v.AdditionalContext}
}

func isCommandType(t string) bool {
	return t == "" || t == "command"
}

// matchTool implements claude-code's `if` syntax minimally:
//   - empty: matches everything
//   - "ToolName": exact tool name
//   - "ToolName(*)" or "ToolName(prefix*)": tool + optional prefix glob
//     against a stringified input form (best-effort)
func matchTool(rule, tool string, input map[string]any) bool {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return true
	}
	if !strings.Contains(rule, "(") {
		return rule == tool
	}
	open := strings.IndexByte(rule, '(')
	close := strings.LastIndexByte(rule, ')')
	if open < 0 || close < open {
		return rule == tool
	}
	if rule[:open] != tool {
		return false
	}
	pattern := strings.TrimSpace(rule[open+1 : close])
	if pattern == "" || pattern == "*" {
		return true
	}
	// Best-effort: stringify the input and glob-prefix-match.
	flat := stringifyInput(input)
	prefix := strings.TrimSuffix(pattern, "*")
	return strings.HasPrefix(flat, prefix)
}

func stringifyInput(in map[string]any) string {
	if v, ok := in["command"].(string); ok {
		return v
	}
	if v, ok := in["path"].(string); ok {
		return v
	}
	if v, ok := in["file_path"].(string); ok {
		return v
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func cwdOrEmpty() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// runHookCommand invokes the user's shell command, feeds the JSON event
// on stdin, and returns the captured stdout. Stderr is forwarded so the
// user can `echo "..." >&2` from their hook for debug output without
// polluting the JSON return channel.
func runHookCommand(ctx context.Context, spec config.HookSpec, payload map[string]any) ([]byte, error) {
	out, _, err := runHookCommandWithCode(ctx, spec, payload)
	return out, err
}

// runHookCommandWithCode is runHookCommand plus an exit-code channel.
// Used by the PreToolUse path to catch the claude-code "exit 49 =
// halt" convention (a hook script can `exit 49` without bothering to
// emit JSON, and we still treat it as a turn-halt signal).
func runHookCommandWithCode(ctx context.Context, spec config.HookSpec, payload map[string]any) ([]byte, int, error) {
	timeout := time.Duration(spec.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	cmd := exec.CommandContext(cctx, "sh", "-c", spec.Command) //nolint:gosec — user-configured by design
	cmd.Stdin = strings.NewReader(string(body))
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"METIS_HOOK_EVENT="+payload["hook_event_name"].(string),
	)
	out, err := cmd.Output()
	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
			// Exit codes 49 and the documented "block tool" 2 are
			// signals, not failures — strip the err so callers
			// process them as data.
			if exitCode == 49 || exitCode == 2 {
				return out, exitCode, nil
			}
		}
	}
	return out, exitCode, err
}

// parsePreToolUseResponse parses a PreToolUse subprocess hook's stdout.
// Three accepted shapes (in order of precedence):
//
//  1. **Claude-code envelope** (preferred for cross-CLI compat):
//
//     {"hookSpecificOutput": {
//     "hookEventName": "PreToolUse",
//     "permissionDecision": "allow"|"deny"|"ask",
//     "permissionDecisionReason": "...",
//     "modifiedInput": {...}
//     }}
//
//  2. **metis-flat form** (older config.toml hooks already on disk):
//
//     {"decision": "allow"|"deny"|"halt",
//     "reason": "...",
//     "modified_input": {...},
//     "halt": true}
//
//  3. **empty body** → proceed unchanged.
//
// `decision: "halt"` (or `halt: true` flag) signals the agent loop to
// stop the entire turn after the current tool batch — claude-code
// parity for "veto chain" hooks. The exit-code-49 convention is
// handled at the caller (runHookCommand returns the exit code so we
// can flip the same signal there).
//
// Unknown / malformed JSON is treated as "proceed unchanged" so a
// misbehaving hook doesn't accidentally block the agent.
func parsePreToolUseResponse(out []byte) *pubhook.ModifiedPreToolUse {
	out = trimBOM(out)
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	// Claude-code envelope first — the field is a discriminator the
	// flat form can't accidentally collide with.
	var envelope struct {
		HookSpecificOutput *struct {
			HookEventName            string         `json:"hookEventName"`
			PermissionDecision       string         `json:"permissionDecision"`
			PermissionDecisionReason string         `json:"permissionDecisionReason"`
			ModifiedInput            map[string]any `json:"modifiedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &envelope); err == nil && envelope.HookSpecificOutput != nil {
		hso := envelope.HookSpecificOutput
		return preToolFromDecision(hso.PermissionDecision, hso.PermissionDecisionReason, hso.ModifiedInput, false)
	}
	// Fall through to the flat form.
	var flat struct {
		Decision      string         `json:"decision"`
		Reason        string         `json:"reason"`
		ModifiedInput map[string]any `json:"modified_input"`
		Halt          bool           `json:"halt"`
	}
	if err := json.Unmarshal(out, &flat); err != nil {
		return nil
	}
	return preToolFromDecision(flat.Decision, flat.Reason, flat.ModifiedInput, flat.Halt)
}

// preToolFromDecision is the shared decision-to-ModifiedPreToolUse
// translator used by both the claude-code envelope and the flat form.
// Centralizes the "halt overrides allow", "deny becomes Output IsError"
// rules so both shapes behave identically.
func preToolFromDecision(decision, reason string, modifiedInput map[string]any, haltFlag bool) *pubhook.ModifiedPreToolUse {
	mod := &pubhook.ModifiedPreToolUse{}
	switch strings.ToLower(decision) {
	case "deny":
		r := reason
		if r == "" {
			r = "denied by hook"
		}
		mod.Output = &pubhook.Output{Content: r, IsError: true}
	case "halt":
		// Halt is a stronger deny: deny the tool AND stop the turn.
		// We synthesize a tool_result so the model has context for
		// why the turn ended (otherwise the transcript shows a
		// dangling tool_use with no matching result).
		r := reason
		if r == "" {
			r = "halted by hook"
		}
		mod.Output = &pubhook.Output{Content: r, IsError: true}
		mod.Halt = true
		mod.HaltReason = r
	case "allow":
		// Explicit allow: claude-code's "skip the gate". metis doesn't
		// yet have a force-allow channel; treat as "proceed normally"
		// so the gate still runs. When we add force-allow it slots in
		// here.
	}
	if haltFlag && !mod.Halt {
		mod.Halt = true
		mod.HaltReason = reason
		if mod.HaltReason == "" {
			mod.HaltReason = "halted by hook"
		}
	}
	if modifiedInput != nil {
		mod.ModifiedInput = modifiedInput
	}
	if mod.Output == nil && mod.ModifiedInput == nil && !mod.Halt {
		return nil // nothing to change
	}
	return mod
}

// trimBOM strips a leading UTF-8 BOM that scripts written on Windows
// sometimes emit. Without this, json.Unmarshal silently fails on the
// first byte and the hook is treated as a no-op.
func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// _ keep filepath imported for future use in resolving relative hook
// command paths to be CLAUDE_CODE_HOOK-style stable.
var _ = filepath.Join
