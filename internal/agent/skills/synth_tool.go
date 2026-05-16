package skills

// synth_tool.go — LLM-facing tool for the dreaming subagent. Lives in
// the skills package (not internal/tools/builtin/) to avoid the import
// cycle that would form if a builtin tool depended on agent state and
// agent then imported builtin to construct the dream registry.
//
// Intentionally NOT auto-registered with the global tools registry —
// only the dreaming fork's registry gets it via
// internal/agent/dream_registry.go, so a regular user-turn never sees
// SkillSynth in its tool list.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

// SkillSynthTool wraps a *Synth as a Tool. The Synth handle is the
// only state — operations route directly to CreateSkill / UpdateSkill,
// validation included.
type SkillSynthTool struct {
	tools.BaseTool
	synth *Synth
}

// NewSkillSynthTool wires the tool to a Synth. Pass nil to make Execute
// fail loudly — useful in tests where the dream fork is configured
// but the skills layer isn't.
func NewSkillSynthTool(synth *Synth) SkillSynthTool {
	return SkillSynthTool{synth: synth}
}

func (SkillSynthTool) Name() string { return "SkillSynth" }
func (SkillSynthTool) Description() string {
	return "Create or update a user-level skill in ~/.metis/skills. ONLY available to the dreaming subagent — the main agent cannot call this tool. Use 'create_skill' for new skills (refuses to overwrite), 'update_skill' to revise an existing skill's body (frontmatter preserved)."
}
func (SkillSynthTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (SkillSynthTool) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"action", "name"},
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "create_skill (refuses if name exists) | update_skill (refuses if name missing) | list_user_skills (no other inputs needed)",
				"enum":        []string{"create_skill", "update_skill", "list_user_skills"},
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Skill name. ASCII alnum/dash/underscore only, 2-64 chars. Will be the filename (~/.metis/skills/<name>.md). Required for create_skill and update_skill; ignored for list_user_skills.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "One-line summary shown in /skills list and the system prompt. Create only.",
			},
			"when_to_use": map[string]any{
				"type":        "string",
				"description": "Positive selector — what kind of task should pick this skill. Create only.",
			},
			"dont_use_when": map[string]any{
				"type":        "string",
				"description": "Negative selector — when NOT to pick this skill. Helps disambiguate similar skills. Create only.",
			},
			"tags": map[string]any{
				"type":        "array",
				"description": "Curated keywords for search. Create only.",
				"items":       map[string]any{"type": "string"},
			},
			"allowed_tools": map[string]any{
				"type":        "array",
				"description": "Restrict tool surface while this skill is active. Empty = no restriction. Create only.",
				"items":       map[string]any{"type": "string"},
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Markdown body shown to the model as the skill's prompt. Required for create_skill and update_skill.",
			},
		},
	}
}

// CanUse — always allow when the tool is in the registry. Scoping is
// enforced by NOT putting SkillSynth into the main registry in the
// first place; once it's reachable, the dreaming fork is the only
// caller and we want it to fire without further friction.
func (SkillSynthTool) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "scoped to dreaming fork by registry restriction"
}

func (t SkillSynthTool) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	if t.synth == nil {
		return &tools.Result{Output: "SkillSynth not configured (no Synth handle)", IsError: true}, nil
	}

	action, _ := in["action"].(string)
	switch action {
	case "create_skill":
		return t.handleCreate(in)
	case "update_skill":
		return t.handleUpdate(in)
	case "list_user_skills":
		names := t.synth.ListUserSkills()
		buf, _ := json.MarshalIndent(map[string]any{
			"dir":   t.synth.Dir(),
			"names": names,
		}, "", "  ")
		return &tools.Result{Output: string(buf)}, nil
	default:
		return &tools.Result{
			Output:  fmt.Sprintf("unknown action %q (want create_skill | update_skill | list_user_skills)", action),
			IsError: true,
		}, nil
	}
}

func (t SkillSynthTool) handleCreate(in map[string]any) (*tools.Result, error) {
	name, _ := in["name"].(string)
	body, _ := in["body"].(string)
	if strings.TrimSpace(body) == "" {
		return &tools.Result{Output: "create_skill: body required", IsError: true}, nil
	}
	meta := SynthSkillMeta{
		Name:         name,
		Description:  asStr(in["description"]),
		WhenToUse:    asStr(in["when_to_use"]),
		DontUseWhen:  asStr(in["dont_use_when"]),
		Tags:         asStrSlice(in["tags"]),
		AllowedTools: asStrSlice(in["allowed_tools"]),
	}
	path, err := t.synth.CreateSkill(meta, body)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	return &tools.Result{Output: fmt.Sprintf("created skill at %s", path)}, nil
}

func (t SkillSynthTool) handleUpdate(in map[string]any) (*tools.Result, error) {
	name, _ := in["name"].(string)
	body, _ := in["body"].(string)
	if strings.TrimSpace(body) == "" {
		return &tools.Result{Output: "update_skill: body required", IsError: true}, nil
	}
	path, err := t.synth.UpdateSkill(name, body)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	return &tools.Result{Output: fmt.Sprintf("updated skill at %s", path)}, nil
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func asStrSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
