// Package skill is the public SDK for Metis's skill marketplace.
//
// A "skill" is a small JSON manifest that bundles a prompt + tool list +
// metadata into a reusable building block. Skills install locally under
// ~/.metis/skills/ and the agent loop pulls them into the system prompt.
//
// 3rd-party authors typically want to do one of two things:
//
//  1. Write a Source — a registry the user can install skills from
//     (e.g. an internal skill server, a private GitHub org). Implement
//     Source and the runtime can wire it into `metis skills install`.
//
//  2. Write a Searcher — a registry that supports query-based lookup.
//     Used by `/skill search` in the chat surface.
//
// Pairs with pkg/tool, pkg/hook, pkg/channel, pkg/llm — the five-pillar
// plugin SDK. Internal/agent/skills hosts the in-tree implementations
// (Store on local filesystem, GitHubSource via raw.githubusercontent).
package skill

import "context"

// Skill is the on-disk / over-the-wire manifest for a single skill.
//
// JSON-serializable (used both as the local file format under ~/.metis/skills
// and the wire format returned by Source.Fetch). Keep this stable —
// breaking changes propagate to every external skill registry.
type Skill struct {
	Name        string   `json:"name"                yaml:"name"`
	Description string   `json:"description"         yaml:"description"`
	Category    string   `json:"category,omitempty"  yaml:"category,omitempty"`
	Prompt      string   `json:"prompt,omitempty"    yaml:"prompt,omitempty"`
	Tools       []string `json:"tools,omitempty"     yaml:"tools,omitempty"`
	Tags        []string `json:"tags,omitempty"      yaml:"tags,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty" yaml:"created_at,omitempty"`
	Uses        int      `json:"uses"                yaml:"uses,omitempty"`

	// AllowedTools restricts what tools the agent can call while a skill
	// is active. Empty = no restriction. Mirrors openclaude's
	// BundledSkillDefinition.allowedTools — lets a skill author limit
	// blast radius (e.g. a doc-writer skill that only needs Read+Write).
	AllowedTools []string `json:"allowed_tools,omitempty" yaml:"allowed_tools,omitempty"`

	// WhenToUse is a free-form hint the agent injects into the system
	// prompt: "use this skill when ...". Helps the LLM pick the right
	// skill on its own without the user having to /skill X.
	WhenToUse string `json:"when_to_use,omitempty" yaml:"when_to_use,omitempty"`

	// DontUseWhen is the negative selector — "do NOT pick this skill
	// when ...". Mirrors claude-code's three-section frontmatter shape
	// (description / when_to_use / dont_use_when). Empirically the LLM
	// confuses similarly-named skills (e.g. "git-rebase" vs
	// "git-cherry-pick") more often than it picks none — telling it
	// when NOT to use a skill is higher-signal than another positive
	// description.
	DontUseWhen string `json:"dont_use_when,omitempty" yaml:"dont_use_when,omitempty"`

	// UserOnly marks a skill that the LLM is NOT allowed to invoke on
	// its own — only a user-typed `/skill X` triggers it. Right for
	// destructive / out-of-band operations the model shouldn't reach
	// for spontaneously: `git-rewrite-history`, `prod-deploy`, etc.
	// Mirrors claude-code's user-invocable concept (negated: there
	// the flag means "model can pick on its own"; here we use the
	// inverse so the safe default is LLM-pickable).
	UserOnly bool `json:"user_only,omitempty" yaml:"user_only,omitempty"`

	// Disabled removes a skill from the system prompt without deleting
	// the manifest. The user toggles via `/skills disable <name>` /
	// `/skills enable <name>`. Useful when iterating on an unstable
	// skill — one keystroke to take it out of the prompt for a turn,
	// then flip it back on without re-installing.
	Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`

	// ActivateOn restricts when this skill appears in the system
	// prompt to working dirs whose path matches one of these patterns.
	// Empty list = always active (legacy behaviour).
	//
	// Patterns are doublestar globs evaluated against the absolute
	// cwd path. Examples:
	//   - "**/frontend/**"     — only when cwd is somewhere under a
	//                            "frontend" directory
	//   - "**/*.go-project"    — only when cwd ends in .go-project
	//   - "/Users/*/work/**"   — under any user's work tree
	//
	// Mirrors claude-code's conditional skill activation
	// (loadSkillsDir.ts: `activateConditionalSkillsForPaths`). The
	// LLM doesn't see frontend skills in a backend repo, which both
	// saves tokens and prevents wrong-skill picks.
	ActivateOn []string `json:"activate_on,omitempty" yaml:"activate_on,omitempty"`

	// ModelOverride pins a specific model id for this skill (e.g. force
	// claude-opus for a code-review skill even if the session is using a
	// faster model). Empty = use the session model.
	ModelOverride string `json:"model_override,omitempty" yaml:"model_override,omitempty"`

	// Version is a free-form string (recommend semver). Surfaced in
	// `metis skills info` so users can tell what's installed.
	Version string `json:"version,omitempty" yaml:"version,omitempty"`

	// ContentHash is the SHA256 of the canonical skill payload (every
	// field except ContentHash, Uses, Source). Marketplace fetches verify
	// this against the hash advertised by the registry before saving —
	// equivalent to Hermes' skills_guard.
	ContentHash string `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`

	// Source records where this skill came from ("local", a github URL,
	// a custom registry id) so `metis skills info` can show provenance.
	Source string `json:"source,omitempty" yaml:"source,omitempty"`

	// Prerequisites declares what the host environment must provide for
	// this skill to be usable. Loader checks these at registration time
	// and surfaces missing items via Skill.Available() — the LLM sees
	// "skill X unavailable: needs `kubectl`" instead of failing during
	// the actual tool call. Mirrors hermes-agent's frontmatter
	// `prerequisites.commands` / `required_environment_variables`
	// (optional-skills/*/SKILL.md).
	Prerequisites *Prerequisites `json:"prerequisites,omitempty" yaml:"prerequisites,omitempty"`

	// TrustLevel records the loader's posture toward this skill's
	// content. Set by the loader when registering, NOT authoritative
	// from the manifest itself (a community skill claiming "builtin"
	// trust must not be honoured). One of:
	//   "builtin"   — shipped in the metis repo (highest)
	//   "trusted"   — installed from an allowlisted source
	//   "community" — third-party, not vetted (lowest; UI shows warning)
	//   "user"      — written by the local user (under ~/.metis/skills/)
	//   "project"   — local to this repo (under .metis/skills/)
	// Mirrors hermes-agent's tools/skills_hub.py:trust_level_for().
	TrustLevel string `json:"trust_level,omitempty" yaml:"trust_level,omitempty"`
}

// Prerequisites describes what the host must provide for a skill to be
// usable. Both fields are optional; nil/empty means "no requirements".
//
// Wire example:
//
//	prerequisites:
//	  commands: [kubectl, helm]
//	  env_vars: [KUBECONFIG]
//	  files:    [~/.kube/config]
//
// Loader resolves these at register time:
//   - commands — looks up via PATH (exec.LookPath)
//   - env_vars — checks os.Getenv(name) != ""
//   - files    — checks os.Stat(name) exists
type Prerequisites struct {
	Commands []string `json:"commands,omitempty" yaml:"commands,omitempty"`
	EnvVars  []string `json:"env_vars,omitempty"  yaml:"env_vars,omitempty"`
	Files    []string `json:"files,omitempty"    yaml:"files,omitempty"`
}

// Trust level constants. String form is what gets serialized so manifests
// stay human-readable.
const (
	TrustBuiltin   = "builtin"
	TrustTrusted   = "trusted"
	TrustUser      = "user"
	TrustProject   = "project"
	TrustCommunity = "community"
)

// Source fetches a skill manifest by name. Local filesystem is one such
// implementation; remote registries (GitHub, https URLs, internal package
// servers) are others. New sources just satisfy this interface and plug
// into the standard Install flow.
type Source interface {
	// Name identifies the source ("github", "https", "internal-registry").
	// Shown in `metis skills info` so users can tell where a skill
	// originated.
	Name() string
	// Fetch returns the manifest for `ref`. The ref shape is source-
	// specific — GitHub uses "<owner>/<repo>:<name>", https uses a URL,
	// etc.
	Fetch(ctx context.Context, ref string) (*Skill, error)
}

// SearchHit is one row in a skill search result. Source is the registry
// the hit came from (matches Source.Name()); Ref is a reference understood
// by that source's Fetch method, so `Store.Install(ctx, src, hit.Ref)`
// works as a one-shot install.
type SearchHit struct {
	Source      string
	Ref         string
	Name        string
	Description string
	URL         string // human-clickable, optional
}

// Searcher is the optional contract a registry implements to support
// `/skill search`. Sources that don't implement Searcher gracefully
// degrade to install-only.
//
// Implementations should respect ctx cancellation and prefer returning
// fewer hits over long blocks (rate-limited remote APIs).
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]SearchHit, error)
}
