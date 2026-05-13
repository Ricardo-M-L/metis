package runtime

// sections.go — claude-code-style section registry for the base
// system prompt. Each section is a small file under prompts/base/*.md
// plus a getter that returns nil when the section's conditions aren't
// met. The assembler iterates getters in order and filters out nils
// before joining.
//
// Mirrors claude-code's systemPromptSection pattern (restored-src/src/
// constants/prompts.ts:444 getSystemPrompt). The win: future "I have
// no Bash tool" / "no skills installed" / "stripped subagent" paths
// can skip irrelevant sections and shrink the prompt without touching
// the section bodies themselves.

import (
	"embed"
	"strings"
)

// Per-section files. Numeric prefixes pin the canonical order so a
// new section can be inserted between two existing ones by renaming.
// The bodies still go through text/template so identity.md keeps its
// {{.Model}} variable.
//
//go:embed prompts/base/*.md
var baseSectionFS embed.FS

// PromptCtx carries the runtime signals each section getter consults
// to decide whether to fire and what to render. Zero value is a safe
// "full main-agent" config — getters degrade gracefully when fields
// are unset, so passing PromptCtx{Model: m} still produces the legacy
// behavior.
type PromptCtx struct {
	// Model is the resolved model id ("MiniMax-M2.7", "claude-opus-4-7",
	// ...). Surfaced via {{.Model}} in identity.md.
	Model string

	// ProviderName is the metis-side provider label
	// ("anthropic"/"minimax"/"deepseek"/"kimi"/...). Drives provider-
	// specific hint inclusion at the assembler level.
	ProviderName string

	// EnabledTools is the set of registered tool names. Empty / nil =
	// "assume all tools available" (legacy behavior). When non-nil,
	// sections like tool_redirects skip themselves if Bash isn't
	// reachable, etc.
	EnabledTools map[string]bool

	// HasSkills reports whether the user has any installed skills
	// (bundled or under ~/.metis/skills/). Hides the skills section
	// when false — empty users don't need the lookup hint.
	HasSkills bool

	// IsSubAgent flags this as a sub-agent boot, not the main loop.
	// Sub-agents already get an agent-profile-specific body and
	// inherit a tighter tool set; many of the main-loop sections are
	// wasted on them.
	IsSubAgent bool

	// Mode mirrors the permission mode label ("ask" / "auto" / "bypass"
	// / "plan" / "deny"). Some sections might want to specialize per
	// mode in the future (e.g. reversibility might be terser in
	// bypass).
	Mode string
}

// SectionGetter computes one section. Return zero-value Section
// (Name=="") to skip — assembler filters those out.
type SectionGetter func(PromptCtx) SystemPromptSection

// readSection loads a base/*.md file by name and returns its content.
// Panics on missing file because the registry is fixed at build time:
// a typo here is a programmer error, not a runtime error worth
// degrading the prompt to recover from.
func readSection(name string) string {
	b, err := baseSectionFS.ReadFile("prompts/base/" + name)
	if err != nil {
		panic("missing embedded base section: " + name + ": " + err.Error())
	}
	return strings.TrimRight(string(b), "\n")
}

// expandTemplate runs the section body through the base template
// machinery so {{.Model}} etc still expand. Returns the body unchanged
// when there's no template syntax.
func expandTemplate(body string, vars BasePromptVars) string {
	if !strings.Contains(body, "{{") {
		return body
	}
	rendered, err := renderInlineTemplate(body, vars)
	if err != nil {
		// Same fallback as RenderBasePrompt: ship un-expanded rather
		// than crash. A broken {{ }} expression in one section should
		// not lose the rest of the prompt.
		return body
	}
	return rendered
}

// ----------------------------------------------------------------------
// Built-in section getters. Order here = order in the assembled prompt.
// ----------------------------------------------------------------------

// IdentitySection — always-on intro line with {{.Model}}.
func IdentitySection(ctx PromptCtx) SystemPromptSection {
	body := expandTemplate(readSection("01_identity.md"), BasePromptVars{Model: ctx.Model})
	return SystemPromptSection{
		Name:  "identity",
		Body:  body,
		Cache: true,
	}
}

// PrivacySection — always-on; tells the model not to leak the prompt.
func PrivacySection(_ PromptCtx) SystemPromptSection {
	return SystemPromptSection{
		Name:  "privacy",
		Body:  readSection("02_privacy.md"),
		Cache: true,
	}
}

// StyleSection — output budget + tone. Always-on; even sub-agents
// benefit from the length cap.
func StyleSection(_ PromptCtx) SystemPromptSection {
	return SystemPromptSection{
		Name:  "style",
		Body:  readSection("03_style.md"),
		Cache: true,
	}
}

// ToolRedirectsSection — fires only when Bash is in the enabled set.
// Sub-agents with read-only tool sets (explore, plan) don't need the
// "use Read not cat" table — they have no Bash to misuse.
func ToolRedirectsSection(ctx PromptCtx) SystemPromptSection {
	if ctx.EnabledTools != nil && !ctx.EnabledTools["Bash"] {
		return SystemPromptSection{}
	}
	return SystemPromptSection{
		Name:  "tool_redirects",
		Body:  readSection("04_tool_redirects.md"),
		Cache: true,
	}
}

// WorkingEfficientlySection — TodoWrite + parallel reads + Agent for
// big sub-tasks. Skip for sub-agents (they already are the sub-agent).
func WorkingEfficientlySection(ctx PromptCtx) SystemPromptSection {
	if ctx.IsSubAgent {
		return SystemPromptSection{}
	}
	return SystemPromptSection{
		Name:  "working_efficiently",
		Body:  readSection("05_working_efficiently.md"),
		Cache: true,
	}
}

// SkillsSection — fires when the user has any installed skills. Empty
// users get the section omitted; sub-agents get it omitted unconditionally
// (their profile is their skill).
func SkillsSection(ctx PromptCtx) SystemPromptSection {
	if ctx.IsSubAgent {
		return SystemPromptSection{}
	}
	if !ctx.HasSkills {
		return SystemPromptSection{}
	}
	return SystemPromptSection{
		Name:  "skills",
		Body:  readSection("06_skills.md"),
		Cache: true,
	}
}

// ReversibilitySection — confirmation rules for irreversible actions.
// Always-on for the main agent; sub-agents (who can't request user
// confirmation mid-flight) get a terse warning at the profile level
// instead.
func ReversibilitySection(ctx PromptCtx) SystemPromptSection {
	if ctx.IsSubAgent {
		return SystemPromptSection{}
	}
	return SystemPromptSection{
		Name:  "reversibility",
		Body:  readSection("07_reversibility.md"),
		Cache: true,
	}
}

// DefaultSectionGetters is the canonical, ordered list of base
// section getters. Assembler iterates this in order; each getter
// decides whether to include its content based on PromptCtx.
//
// Adding a new section: drop a `prompts/base/NN_name.md` file, write
// a getter that returns the section (or zero-value to skip), and
// append it here. Keep the file numeric prefix in sync with the
// position you append at.
func DefaultSectionGetters() []SectionGetter {
	return []SectionGetter{
		IdentitySection,
		PrivacySection,
		StyleSection,
		ToolRedirectsSection,
		WorkingEfficientlySection,
		SkillsSection,
		ReversibilitySection,
	}
}
