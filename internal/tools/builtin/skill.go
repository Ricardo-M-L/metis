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

// underDir reports whether path lives inside dir, comparing with a
// path-separator boundary (so `/a/skills` does not match `/a/skills-x`)
// and absolute-normalizing both sides (so a relative configured skill_dir
// still matches the always-absolute manifest LocalPath).
func underDir(path, dir string) bool {
	ad, err1 := filepath.Abs(dir)
	ap, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.HasPrefix(ap, ad+string(os.PathSeparator))
}

// skillFileBase is the on-disk filename without extension — the key the
// curator's usage store uses (it scans by filename, not frontmatter name).
func skillFileBase(localPath string) string {
	b := filepath.Base(localPath)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// Skill exposes list/get/invoke over the multi-source skill loader.
type Skill struct {
	tools.BaseTool
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

// CatalogLoader exposes the shared read-side catalog to trusted in-process UI
// surfaces. It deliberately returns the Loader, not a copied skill slice, so
// /skills can invalidate after an external package manager changes the disk.
// Tool callers cannot access this method through the JSON tool protocol.
func (s Skill) CatalogLoader() *skillsloader.Loader { return s.loader }

func (Skill) Name() string { return "Skill" }
func (Skill) Description() string {
	return "Inspect or invoke the live local skill catalog. For any request to install skills, call action=plan_install with every requested name before Bash, WebSearch, or git: it checks installed skills first, flags typos/ambiguity without guessing, and returns a deterministic official lifecycle or one discovery step."
}
func (Skill) Concurrency(in map[string]any) tools.Concurrency {
	if action, _ := in["action"].(string); action == "invoke" {
		// A trusted skill may expand inline shell commands. Keep invocation
		// serialized with other state-changing tools; list/get remain safe to
		// fan out with ordinary read-only exploration.
		return tools.ConcurrencyExclusive
	}
	return tools.ConcurrencySafe
}

func (s Skill) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "list | get | invoke | plan_install",
				"enum":        []string{"list", "get", "invoke", "plan_install"},
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Skill name (for get/invoke, or one-name plan_install)",
			},
			"names": map[string]any{
				"type":        "array",
				"description": "All names exactly as the user typed them (for plan_install). Preserve possible typos; the tool resolves them safely.",
				"items": map[string]any{
					"type": "string",
				},
			},
		},
	}
}

func (s Skill) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	action, _ := in["action"].(string)
	if action != "invoke" {
		return tools.PermissionAllow, "skill lookup"
	}
	if s.gate == nil {
		return tools.PermissionDeny, "skill invoke requires a permission gate"
	}
	// `invoke` is not merely a lookup: it updates usage metadata and a
	// trusted skill may execute inline shell while expanding its prompt.
	// Route it through the permission gate with the full structured input so
	// input-aware read-only resolution cannot mistake it for list/get.
	payload, _ := json.Marshal(in)
	d, src := s.gate.Check(context.Background(), "Skill", string(payload))
	return mapDecision(d), src
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
	case "plan_install":
		names := stringSlice(in["names"])
		if len(names) == 0 && name != "" {
			names = []string{name}
		}
		return s.planSkillInstall(names)
	case "get":
		return s.getSkill(name)
	case "invoke":
		// Execute is normally reached only after CanUse, but keep the Plan
		// boundary here too: direct embedders and future dispatch paths must
		// not turn a trusted skill's inline shell into a Plan-mode escape.
		if s.gate != nil && s.gate.Mode() == permission.ModePlan {
			return &tools.Result{
				Output:  "Skill invoke is disabled while the parent is in plan mode; use Skill get to inspect it, then invoke after the plan is approved",
				IsError: true,
			}, nil
		}
		return s.invokeSkill(ctx, name)
	}
	return &tools.Result{
		Output:  "unknown action: " + action + " (use: list | get | invoke | plan_install)",
		IsError: true,
	}, nil
}

func (s Skill) listSkills() (*tools.Result, error) {
	// A package manager may have installed a universal skill during this
	// session. Refresh on explicit list rather than requiring a Metis restart.
	s.loader.Invalidate()
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
	// Record a view for a user-owned skill — distinct from an invoke, so the
	// curator can tell "the agent keeps reading this for reference" from
	// "the agent keeps running it". Keyed by filename base (see invokeSkill).
	if s.userDir != "" && sk.LocalPath != "" && underDir(sk.LocalPath, s.userDir) &&
		(s.gate == nil || s.gate.Mode() != permission.ModePlan) {
		skillsloader.NewUsageStore(s.userDir).RecordView(skillFileBase(sk.LocalPath))
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

	// Record an invoke in the curator's usage store so its lifecycle clock
	// tracks real usage. This replaces an earlier os.Chtimes-on-invoke hack
	// (which mutated the file on every read and couldn't tell use from view
	// from patch). Scoped to userDir-owned skills so we never record for
	// bundled / project / plugin / mcp skills. Keyed by the on-disk filename
	// base (NOT the invoke arg / frontmatter name) so it matches how the
	// curator scans — otherwise a skill whose frontmatter `name:` differs
	// from its filename records under one key and ages under another.
	if s.userDir != "" && sk.LocalPath != "" && underDir(sk.LocalPath, s.userDir) {
		skillsloader.NewUsageStore(s.userDir).RecordUse(skillFileBase(sk.LocalPath))
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
