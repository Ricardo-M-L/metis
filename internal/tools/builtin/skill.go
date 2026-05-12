package builtin

// skill.go — LLM-facing Skill tool.
//
// Rebuilt 2026-04-30 to fix the integration gap that the E2E test caught
// (bug #3): the previous version only read `<skillDir>/*.json` and
// silently bypassed the 5-layer loader (`internal/agent/skills.Loader`),
// leaving the 22 bundled skills, project skills, plugin skills, and MCP
// skills all invisible to the agent. After this rewrite the tool delegates
// list/get/invoke to the loader so all sources show up.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	skillsloader "github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubskill "github.com/Ricardo-M-L/metis/pkg/skill"
)

// Skill exposes list/get/invoke over the multi-source skill loader.
type Skill struct {
	loader *skillsloader.Loader
	// userDir is preserved so `invoke` can still bump the per-skill `Uses`
	// counter on disk for skills that came from the user layer (we don't
	// write back to bundled / project / plugin / mcp layers).
	userDir string
	gate    *permission.Gate
	// sessionIDFn returns the current chat session id used to expand
	// ${METIS_SESSION_ID} / ${CLAUDE_SESSION_ID} in the skill prompt
	// body. Nil = always empty (placeholder left intact). Wired by
	// the runtime so internal/tools/builtin doesn't take a back-
	// dependency on internal/runtime.
	sessionIDFn func() string
}

// NewSkill is the runtime constructor. The loader is built by the caller
// (BuildToolRegistry) so it can include plugin sources known only at
// runtime. Pass nil loader to disable the tool entirely (Execute will
// surface a clear error rather than panicking).
func NewSkill(gate *permission.Gate, loader *skillsloader.Loader, userDir string) Skill {
	return Skill{loader: loader, userDir: userDir, gate: gate}
}

// WithSessionIDFn returns a copy of the Skill tool wired with a
// session-id getter. Used by runtime.BuildToolRegistry to plumb
// CurrentSessionID without creating an import cycle.
func (s Skill) WithSessionIDFn(fn func() string) Skill {
	s.sessionIDFn = fn
	return s
}

func (Skill) Name() string { return "Skill" }
func (Skill) Description() string {
	return "Look up or invoke a named skill from any registered source (bundled, user, project, plugin, mcp)."
}
func (Skill) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (s Skill) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "list | get | invoke",
				"enum":        []string{"list", "get", "invoke"},
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Skill name (for get/invoke actions)",
			},
		},
	}
}

func (s Skill) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "skill lookup"
}

func (s Skill) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if s.loader == nil {
		return &tools.Result{
			Output:  "Skill: loader not initialized (this is a metis bug — please report)",
			IsError: true,
		}, nil
	}
	action, _ := in["action"].(string)
	name, _ := in["name"].(string)

	switch action {
	case "list":
		return s.listSkills()
	case "get":
		return s.getSkill(name)
	case "invoke":
		return s.invokeSkill(ctx, name)
	}
	return &tools.Result{
		Output:  "unknown action: " + action + " (use: list | get | invoke)",
		IsError: true,
	}, nil
}

func (s Skill) listSkills() (*tools.Result, error) {
	skills, err := s.loader.List()
	if err != nil {
		return &tools.Result{Output: "list error: " + err.Error(), IsError: true}, nil
	}
	if len(skills) == 0 {
		return &tools.Result{
			Output: "(no skills available — this is unusual; bundled layer should always have some)",
		}, nil
	}
	var b strings.Builder
	for _, sk := range skills {
		b.WriteString("- **")
		b.WriteString(sk.Name)
		b.WriteString("**")
		if sk.Description != "" {
			b.WriteString(": ")
			b.WriteString(sk.Description)
		}
		b.WriteByte('\n')
	}
	return &tools.Result{Output: b.String()}, nil
}

func (s Skill) getSkill(name string) (*tools.Result, error) {
	if name == "" {
		return &tools.Result{Output: "Skill get: name required", IsError: true}, nil
	}
	sk, err := s.loader.Get(name)
	if err != nil {
		return &tools.Result{Output: "skill lookup error: " + err.Error(), IsError: true}, nil
	}
	// Loader returns (nil, nil) on miss — error path above only fires for
	// layer scan failures. Surface a friendly "not found" instead of
	// json-marshalling nil into the literal string "null".
	if sk == nil {
		return &tools.Result{Output: "skill not found: " + name, IsError: true}, nil
	}
	out, _ := json.MarshalIndent(sk, "", "  ")
	return &tools.Result{Output: string(out)}, nil
}

func (s Skill) invokeSkill(ctx context.Context, name string) (*tools.Result, error) {
	if name == "" {
		return &tools.Result{Output: "Skill invoke: name required", IsError: true}, nil
	}
	sk, err := s.loader.Get(name)
	if err != nil {
		return &tools.Result{Output: "skill lookup error: " + err.Error(), IsError: true}, nil
	}
	if sk == nil {
		return &tools.Result{Output: "skill not found: " + name, IsError: true}, nil
	}

	// Best-effort uses-counter increment: only meaningful for skills the
	// user actually owns on disk. Bundled skills are embedded read-only;
	// plugin skills live under ~/.metis/plugins/<name>/skills/ and are
	// considered immutable from the agent's POV. So we only persist when
	// a same-named JSON file exists in userDir.
	if s.userDir != "" {
		path := filepath.Join(s.userDir, sanitize(name)+".json")
		if data, err := os.ReadFile(path); err == nil {
			var saved pubskill.Skill
			if json.Unmarshal(data, &saved) == nil {
				saved.Uses++
				if b, mErr := json.MarshalIndent(saved, "", "  "); mErr == nil {
					_ = os.WriteFile(path, b, 0o644)
				}
			}
		}
	}

	var b strings.Builder
	if sk.Prompt != "" {
		// Template-variable expansion (${METIS_SKILL_DIR} /
		// ${METIS_SESSION_ID} + claude-code aliases) so a skill author
		// can reference their own dir and the session id from inside
		// the prompt body. Mirrors claude-code's loadSkillsDir.ts
		// substitution step. Empty values leave the placeholder
		// intact — claude-code parity.
		var skillDir, sessionID string
		if sk.LocalPath != "" {
			skillDir = filepath.Dir(sk.LocalPath)
		}
		if s.sessionIDFn != nil {
			sessionID = s.sessionIDFn()
		}
		body := skillsloader.ExpandTemplateVars(sk.Prompt, skillDir, sessionID)
		// Inline-shell expansion (!`cmd` and ```! blocks) — gated by
		// the loader-stamped TrustLevel so a community / MCP-source
		// skill can't smuggle shell into the prompt. Mirrors
		// claude-code's MCP-skill carve-out in executeShellCommandsInPrompt.
		if skillsloader.ShouldRunInlineShell(sk.TrustLevel) {
			body = skillsloader.ExpandInlineShell(ctx, body, skillDir)
		}
		fmt.Fprintf(&b, "## Skill: %s\n\n", sk.Name)
		b.WriteString(body)
		b.WriteByte('\n')
	}
	if len(sk.AllowedTools) > 0 {
		b.WriteString("\n**Allowed tools (this skill restricts the agent to):** ")
		b.WriteString(strings.Join(sk.AllowedTools, ", "))
		b.WriteByte('\n')
	} else if len(sk.Tools) > 0 {
		b.WriteString("\n**Suggested tools:** ")
		b.WriteString(strings.Join(sk.Tools, ", "))
		b.WriteByte('\n')
	}
	if sk.WhenToUse != "" {
		b.WriteString("\n**When to use:** ")
		b.WriteString(sk.WhenToUse)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		b.WriteString("(skill has no prompt content)")
	}
	return &tools.Result{Output: b.String()}, nil
}

func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '.' || r == 0 {
			return -1
		}
		return r
	}, name)
}
