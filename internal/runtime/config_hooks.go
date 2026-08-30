package runtime

import (
	"bytes"
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
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/security"
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
// Registration failures do not block startup. Once registered, a failing
// PreToolUse policy hook fails the evaluated tool closed; observer hooks only
// log their failure and let the session continue.
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
			// Parse the control channel before redacting model-visible text.
			// A trusted hook may intentionally inject a credential into
			// modifiedInput for the tool to consume internally; mutating the raw
			// JSON first would replace that value and silently break the tool.
			mod, responseOK := parsePreToolUseResponseChecked(out)
			redactPreToolUseResponse(mod, hookCommandEnv(payload))
			// Claude-code parity: exit code 49 means "halt the turn",
			// regardless of stdout. A user can `exit 49` from a one-line
			// shell hook without bothering to emit JSON. Honor it
			// whether or not stdout has additional structure.
			if code == 49 {
				if mod == nil {
					mod = &pubhook.ModifiedPreToolUse{}
				}
				mod.Halt = true
				if mod.HaltReason == "" {
					mod.HaltReason = "halted by hook (exit 49)"
				}
				return forcePreToolUseDeny(mod, mod.HaltReason)
			}
			// Exit 2 is the documented blocking-error convention. Any other
			// non-zero exit (including timeout/start failure) also fails closed:
			// PreToolUse is a policy boundary, so a broken policy program must
			// never accidentally authorize the tool it was evaluating.
			if code == 2 {
				return forcePreToolUseDeny(mod, "blocked by hook (exit 2)")
			}
			if err != nil {
				logHookCommandError("PreToolUse", spec, payload, err)
				reason := "PreToolUse hook failed closed"
				if code != 0 {
					reason = fmt.Sprintf("PreToolUse hook failed closed (exit %d)", code)
				}
				return forcePreToolUseDeny(mod, reason)
			}
			if !responseOK {
				return forcePreToolUseDeny(nil, "PreToolUse hook returned malformed or unknown JSON")
			}
			return mod
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
				logHookCommandError("PostToolUse", spec, payload, err)
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
				logHookCommandError("SessionStart", spec, payload, err)
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
				logHookCommandError("SessionEnd", spec, payload, err)
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
				logHookCommandError("UserPromptSubmit", spec, payload, err)
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
				logHookCommandError("Notification", spec, payload, err)
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
				logHookCommandError("PermissionRequest", spec, payload, err)
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
				logHookCommandError("PermissionDenied", spec, payload, err)
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
				logHookCommandError("CwdChanged", spec, payload, err)
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
				logHookCommandError("PreCompact", spec, payload, err)
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
				logHookCommandError("PostCompact", spec, payload, err)
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
	return redactStructuredHookOutput(out, hookCommandEnv(payload)), err
}

func hookCommandEnv(payload map[string]any) []string {
	event, _ := payload["hook_event_name"].(string)
	// Hook commands are executable policy, but they are still subprocesses of
	// a model-facing application. Do not inherit provider keys, connector
	// tokens, auth-agent sockets, or arbitrary ambient credentials. User hooks
	// retain the ordinary process/toolchain environment plus the explicit event
	// marker; credentials must be obtained by a purpose-built trusted helper.
	return security.RestrictedSubprocessEnv(os.Environ(), "METIS_HOOK_EVENT="+event)
}

// redactStructuredHookOutput protects the JSON control channel without
// rewriting its encoded representation in-place. Exact environment secrets
// may contain quotes, backslashes, or newlines; their JSON form is escaped and
// therefore cannot be found reliably in the raw byte stream. Decode first,
// recursively sanitize string values, then re-encode a valid document.
//
// Non-JSON stdout is never accepted by a structured response parser, but it is
// still returned by this low-level helper for compatibility. Redact that plain
// text directly so a future caller cannot accidentally surface a credential.
func redactStructuredHookOutput(out []byte, env []string) []byte {
	trimmed := trimBOM(out)
	if len(strings.TrimSpace(string(trimmed))) == 0 {
		return trimmed
	}
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return []byte(security.RedactSubprocessTextWithEnv(string(out), env))
	}
	redactDecodedHookStrings(decoded, env)
	redacted, err := json.Marshal(decoded)
	if err != nil {
		// json.Unmarshal only produces JSON-marshalable values. Keep a defensive
		// fail-safe in case that invariant changes in a future decoder wrapper.
		return []byte(security.RedactSubprocessTextWithEnv(string(out), env))
	}
	return redacted
}

func redactDecodedHookStrings(value any, env []string) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if text, ok := child.(string); ok {
				v[key] = security.RedactSubprocessTextWithEnv(text, env)
				continue
			}
			redactDecodedHookStrings(child, env)
		}
	case []any:
		for i, child := range v {
			if text, ok := child.(string); ok {
				v[i] = security.RedactSubprocessTextWithEnv(text, env)
				continue
			}
			redactDecodedHookStrings(child, env)
		}
	}
}

func logHookCommandError(event string, spec config.HookSpec, payload map[string]any, err error) {
	fmt.Fprint(os.Stderr, formatHookCommandError(event, spec, payload, err))
}

func formatHookCommandError(event string, spec config.HookSpec, payload map[string]any, err error) string {
	env := hookCommandEnv(payload)
	command := security.RedactSubprocessTextWithEnv(spec.Command, env)
	errText := security.RedactSubprocessTextWithEnv(err.Error(), env)
	return fmt.Sprintf("hook %s %s: %s\n", event, command, errText)
}

// runHookCommandWithCode is the raw-stdout variant plus an exit-code channel.
// Used by the PreToolUse path to catch the claude-code "exit 49 =
// halt" and "exit 2 = block" conventions. Its caller must parse the raw
// control JSON before redacting display fields.
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
	if err := cctx.Err(); err != nil {
		return nil, 0, err
	}
	shell, shellArgs := hookShellCommand(spec.Command)
	cmd := exec.Command(shell, shellArgs...) //nolint:gosec — user-configured by design
	cmd.Stdin = strings.NewReader(string(body))
	var stdout cappedHookStdout
	var stderr cappedHookStderr
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Hooks run with the same restricted child-process environment as other
	// Metis subprocesses. In particular, provider/API credentials never cross
	// this boundary through ambient environment inheritance.
	cmd.Env = hookCommandEnv(payload)
	// Hooks may spawn grandchildren. Killing only the direct shell leaves those
	// descendants alive and can leave Wait blocked forever on inherited output
	// pipes. Own a process group/tree and cancel the whole tree explicitly.
	jobs.ApplyProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-cctx.Done():
		// taskkill can itself hang on Windows. Keep tree termination off this
		// goroutine so the hook timeout remains a real upper bound.
		go jobs.KillProcessGroup(cmd.Process)
		select {
		case <-done:
			err = cctx.Err()
		case <-time.After(2 * time.Second):
			return nil, 0, fmt.Errorf("hook process tree did not exit after cancellation: %w", cctx.Err())
		}
	}
	out := []byte(stdout.String())
	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		}
	}
	if stdout.Truncated() {
		// A partial control document is unsafe to interpret. Force policy hooks
		// to fail closed and make observer-hook misconfiguration visible.
		err = errors.New("hook stdout exceeded 1 MiB limit")
	}
	if stderr.Len() > 0 || stderr.Truncated() {
		_, _ = fmt.Fprint(os.Stderr, security.RedactSubprocessTextWithEnv(stderr.String(), cmd.Env))
	}
	return out, exitCode, err
}

const maxHookStderrBytes = 256 * 1024
const maxHookStdoutBytes = 1 << 20

type cappedHookStdout struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedHookStdout) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxHookStdoutBytes - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedHookStdout) Truncated() bool { return b.truncated }
func (b *cappedHookStdout) String() string  { return b.buf.String() }

// cappedHookStderr prevents a noisy or wedged hook from retaining unbounded
// stderr while still returning len(p), as required by exec.Cmd's Writer
// contract. The truncation marker is added only when output is rendered.
type cappedHookStderr struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedHookStderr) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxHookStderrBytes - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedHookStderr) Len() int        { return b.buf.Len() }
func (b *cappedHookStderr) Truncated() bool { return b.truncated }
func (b *cappedHookStderr) String() string {
	if !b.truncated {
		return b.buf.String()
	}
	return b.buf.String() + "\n[hook stderr truncated]\n"
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
// The checked parser distinguishes a recognized no-op from malformed or
// unknown non-empty JSON so the PreToolUse policy boundary can fail closed.
func parsePreToolUseResponse(out []byte) *pubhook.ModifiedPreToolUse {
	mod, _ := parsePreToolUseResponseChecked(out)
	return mod
}

// parsePreToolUseResponseChecked separates a recognized no-op such as an
// empty response or {"decision":"allow"} from a malformed/unknown non-empty
// policy response. The runtime must fail the latter closed; a nil pointer alone
// cannot carry that distinction because valid allow responses intentionally
// produce no modification.
func parsePreToolUseResponseChecked(out []byte) (*pubhook.ModifiedPreToolUse, bool) {
	out = trimBOM(out)
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, true
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
		if !validPreToolDecision(hso.PermissionDecision) && hso.ModifiedInput == nil {
			return nil, false
		}
		return preToolFromDecision(hso.PermissionDecision, hso.PermissionDecisionReason, hso.ModifiedInput, false), true
	}
	// Fall through to the flat form.
	var flat struct {
		Decision      string         `json:"decision"`
		Reason        string         `json:"reason"`
		ModifiedInput map[string]any `json:"modified_input"`
		Halt          bool           `json:"halt"`
	}
	if err := json.Unmarshal(out, &flat); err != nil {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil || fields == nil {
		return nil, false
	}
	_, hasDecision := fields["decision"]
	_, hasModifiedInput := fields["modified_input"]
	_, hasHalt := fields["halt"]
	if (!hasDecision || !validPreToolDecision(flat.Decision)) && !hasModifiedInput && !hasHalt {
		return nil, false
	}
	if hasDecision && !validPreToolDecision(flat.Decision) {
		return nil, false
	}
	return preToolFromDecision(flat.Decision, flat.Reason, flat.ModifiedInput, flat.Halt), true
}

func validPreToolDecision(decision string) bool {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "allow", "deny", "ask", "halt":
		return true
	default:
		return false
	}
}

// redactPreToolUseResponse sanitizes only fields that can be rendered into
// the transcript/model context. ModifiedInput is intentionally untouched: it
// is the hook's internal tool-control payload and may contain a credential the
// hook injected specifically so the tool, rather than the model, can use it.
func redactPreToolUseResponse(mod *pubhook.ModifiedPreToolUse, env []string) {
	if mod == nil {
		return
	}
	if mod.Output != nil {
		mod.Output.Content = security.RedactSubprocessTextWithEnv(mod.Output.Content, env)
	}
	mod.HaltReason = security.RedactSubprocessTextWithEnv(mod.HaltReason, env)
	if mod.ModifiedInput != nil {
		mod.PresentationInput = redactedHookInput(mod.ModifiedInput, env)
	}
}

func redactedHookInput(input map[string]any, env []string) map[string]any {
	if input == nil {
		return nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var cloned map[string]any
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return nil
	}
	redactDecodedHookStrings(cloned, env)
	return cloned
}

func forcePreToolUseDeny(mod *pubhook.ModifiedPreToolUse, fallback string) *pubhook.ModifiedPreToolUse {
	if mod == nil {
		mod = &pubhook.ModifiedPreToolUse{}
	}
	if mod.Output == nil || !mod.Output.IsError {
		mod.Output = &pubhook.Output{Content: fallback, IsError: true}
	} else if strings.TrimSpace(mod.Output.Content) == "" {
		mod.Output.Content = fallback
	}
	return mod
}

// preToolFromDecision is the shared decision-to-ModifiedPreToolUse
// translator used by both the claude-code envelope and the flat form.
// Centralizes the "halt overrides allow", "deny becomes Output IsError"
// rules so both shapes behave identically.
func preToolFromDecision(decision, reason string, modifiedInput map[string]any, haltFlag bool) *pubhook.ModifiedPreToolUse {
	mod := &pubhook.ModifiedPreToolUse{}
	switch strings.ToLower(strings.TrimSpace(decision)) {
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
