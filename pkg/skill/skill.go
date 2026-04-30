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
}

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
