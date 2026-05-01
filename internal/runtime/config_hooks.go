package runtime

import (
	"context"
	"encoding/json"
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
			out, err := runHookCommand(ctx, spec, payload)
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
	timeout := time.Duration(spec.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(cctx, "sh", "-c", spec.Command) //nolint:gosec — user-configured by design
	cmd.Stdin = strings.NewReader(string(body))
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"METIS_HOOK_EVENT="+payload["hook_event_name"].(string),
	)
	out, err := cmd.Output()
	if err != nil {
		return out, err
	}
	return out, nil
}

// parsePreToolUseResponse expects the hook to return either an empty body
// (proceed unchanged) or a JSON document like:
//
//	{
//	  "decision": "allow" | "deny",
//	  "reason": "...",
//	  "modified_input": {...}
//	}
//
// Unknown / malformed JSON is treated as "proceed unchanged" so a
// misbehaving hook doesn't accidentally block the agent.
func parsePreToolUseResponse(out []byte) *pubhook.ModifiedPreToolUse {
	out = trimBOM(out)
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	var v struct {
		Decision      string         `json:"decision"`
		Reason        string         `json:"reason"`
		ModifiedInput map[string]any `json:"modified_input"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil
	}
	switch v.Decision {
	case "deny":
		reason := v.Reason
		if reason == "" {
			reason = "denied by hook"
		}
		return &pubhook.ModifiedPreToolUse{
			Output: &pubhook.Output{Content: reason, IsError: true},
		}
	case "allow":
		// Hook explicitly allowed — claude-code semantics: short-circuit
		// the permission system. We don't have a "force allow" channel
		// in our PreToolUse return today, so honor it as "no change"
		// (the upstream gate decides). Kept as an explicit case so
		// when we add force-allow it slots in here.
		_ = reason
	}
	if v.ModifiedInput != nil {
		return &pubhook.ModifiedPreToolUse{ModifiedInput: v.ModifiedInput}
	}
	return nil
}

// reason is a no-op symbol referenced above to keep the Allow case
// readable without the linter complaining about unused locals when the
// codebase eventually gets a proper "force allow" plumbing.
var reason any

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
