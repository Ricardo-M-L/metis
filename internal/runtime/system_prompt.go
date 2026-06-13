package runtime

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
)

// SystemPromptFileName is the filename Metis looks for under ~/.metis/
// when assembling the agent's system prompt.
const SystemPromptFileName = "system.md"

// basePromptTPL is the embedded base prompt template. Lives in
// prompts/base.md so the prompt text is editable without
// recompiling inline string constants. text/template syntax —
// variables: .Model, .ProviderHint (mirror crush's .md.tpl pattern).
//
//go:embed prompts/base.md
var basePromptTPL string

// BasePromptVars is the template context for base.md. Empty
// fields render as their `{{if .X}}...{{end}}` blocks would suggest:
// missing → silent. Add new fields here and their `{{ .Foo }}`
// reference in base.md together.
type BasePromptVars struct {
	Model        string // e.g. "claude-opus-4-7" — surfaced as "powered by ..."
	ProviderHint string // optional provider-specific guidance (Claude→XML, OpenAI→JSON)
}

// compiledBaseTPL caches the parsed template across calls. Parse is
// cheap but unnecessary on every chat boot.
var (
	baseTPLOnce sync.Once
	baseTPL     *template.Template
	baseTPLErr  error
)

func parsedBaseTPL() (*template.Template, error) {
	baseTPLOnce.Do(func() {
		baseTPL, baseTPLErr = template.New("base").Parse(basePromptTPL)
	})
	return baseTPL, baseTPLErr
}

// DefaultBasePrompt returns the embedded base prompt with empty
// template variables — same string the legacy hardcoded `defaultSystem`
// constant used to provide. Use RenderBasePrompt for variable
// injection.
func DefaultBasePrompt() string {
	return RenderBasePrompt(BasePromptVars{})
}

// SimpleBasePrompt returns a one-sentence system prompt + cwd + date.
// Used when METIS_SIMPLE=1 / --simple is active. Mirrors claude-code's
// CLAUDE_CODE_SIMPLE escape hatch: skip the heavy guidance bundle when
// the user is running a short, scripted task that doesn't need it
// (CI prompts, cron loops, one-shot `metis run`).
//
// The user message + tool schemas still carry their own context; this
// just drops the ~5K-char base prompt to a single line.
func SimpleBasePrompt(model string) string {
	cwd, _ := os.Getwd()
	if model == "" {
		model = "an LLM"
	}
	return fmt.Sprintf("You are metis, a fast local-first agent CLI powered by %s. CWD: %s. Date: %s.",
		model, cwd, time.Now().Format("2006-01-02"))
}

// IsSimpleMode returns true when the user opted into the simple-prompt
// escape hatch via the METIS_SIMPLE env var. The CLI flag `--simple`
// in cmd/metis sets this env at boot so both paths produce the same
// result and downstream code only has one signal to read.
func IsSimpleMode() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("METIS_SIMPLE")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// RenderBasePrompt expands the base prompt with the given variables.
// Now backed by the per-section registry (sections.go) instead of the
// monolithic base.md template — the section path produces the same
// final text but lets sub-agents / Bash-less tool sets skip
// irrelevant sections.
//
// The legacy basePromptTPL is still embedded for the rare fallback
// when assembly fails; current code paths never hit that branch.
//
// The {{.ProviderHint}} suffix is preserved as a trailing block when
// vars.ProviderHint is non-empty — callers that want it as a separate
// cacheable section should call AssembleBaseSections directly and
// append a ProviderHint section themselves.
func RenderBasePrompt(vars BasePromptVars) string {
	ctx := PromptCtx{
		Model: vars.Model,
		// HasSkills defaults to true here so the section fires for
		// the legacy "render-everything" call path. Callers that
		// want context-aware behavior should use AssembleBaseSections
		// with a properly-populated PromptCtx directly.
		HasSkills: true,
	}
	base := AssembleBaseString(ctx)
	if vars.ProviderHint != "" {
		return strings.TrimSpace(base) + "\n\n" + strings.TrimSpace(vars.ProviderHint)
	}
	return strings.TrimSpace(base)
}

// renderInlineTemplate runs an arbitrary template string through the
// same engine RenderBasePrompt uses. Helper for the per-section
// registry (sections.go) — each section body may contain its own
// {{.Var}} markers and we parse them on demand rather than embedding
// the whole file twice.
//
// Returns an error so the caller can fall back to the un-expanded body
// instead of silently emitting garbage.
func renderInlineTemplate(body string, vars BasePromptVars) (string, error) {
	tpl, err := template.New("section").Parse(body)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// PromptMode picks one of three pre-set scopes for AssembleSystemPrompt.
// Mirrors openclaw's full / medium / minimal prompt-mode trio.
//
//   - PromptFull    base + env + project_context + addendum (default)
//   - PromptMedium  base + env + project_context (no addendum) — use
//     for coordinator workers that should respect the
//     project's CLAUDE.md but don't need the user's
//     personal preferences (~/.metis/system.md).
//   - PromptMinimal base + env only — use for narrow sub-agents whose
//     task is small enough that broader context just
//     wastes tokens.
type PromptMode int

const (
	PromptFull PromptMode = iota
	PromptMedium
	PromptMinimal
)

// AssembleOptions controls AssembleSystemPrompt. Zero value
// (PromptFull) matches the pre-existing behaviour for the main loop.
// Sub-agents pass PromptMedium / PromptMinimal to trim context.
type AssembleOptions struct {
	// Mode picks the scope. Zero = PromptFull.
	Mode PromptMode
	// Minimal is the legacy bool that maps to PromptMinimal. Kept for
	// callers that haven't migrated to Mode yet — when true and Mode
	// is the zero value, it wins.
	Minimal bool
	// SkipEnv skips the <env> block. Rare — used by tests that want a
	// deterministic prompt without today's date.
	SkipEnv bool

	// Overlays are extra prompt sections contributed by providers,
	// extensions, or runtime hooks. They're inserted between `base`
	// and `env` in the assembled output so a stable provider note
	// stays inside the cached prefix. Each overlay's Cache /
	// Volatile flags decide its own caching posture independently.
	//
	// Example use cases:
	//   - Provider self-introduction: "I'm Claude; XML in arguments"
	//   - Plugin-contributed system block: "this session is a code
	//     review; refuse non-review tool calls"
	//   - Daemon-injected runtime fact (e.g. "context budget = 50%")
	//
	// Mirrors openclaw's prompt-overlay extension point.
	Overlays []SystemPromptSection
}

// resolvedMode returns the effective mode after legacy-flag fallback.
func (o AssembleOptions) resolvedMode() PromptMode {
	if o.Minimal {
		return PromptMinimal
	}
	return o.Mode
}

// SystemPromptSection is one piece of the assembled system prompt.
// Mirrors claude-code's systemPromptSection (constants/systemPromptSections.ts):
// instead of one giant string with embedded boundary markers, the
// prompt is a typed list. Each section declares whether it's eligible
// for prompt-cache.
//
// Why typed sections instead of `string`:
//   - Per-section cache control without parsing markers (Anthropic
//     provider can read sec.Cache directly)
//   - Volatile sections (e.g. live diagnostic injection from a hook)
//     can opt OUT of cache so a cache miss doesn't spread
//   - Future per-section transforms (token-counting, redaction,
//     overlay insertion) act on structured input, not free string
//
// Name is for diagnostics — "base", "env", "project_context",
// "addendum", or a custom overlay tag like "provider:claude".
type SystemPromptSection struct {
	Name     string
	Body     string
	Cache    bool // emit cache_control at the end of this section's block
	Volatile bool // explicitly DISABLE cache even if Cache is true (wins over Cache)
}

// AssembleSystemPromptSectionsCtx is the section-aware constructor.
// Unlike AssembleSystemPromptSections (which wraps a pre-rendered
// `base string` as one giant section), this calls the per-section
// registry from sections.go so each base sub-section keeps its own
// cache flag and the assembler can skip irrelevant sections per ctx.
//
// Output ordering:
//
//	identity / privacy / style / tool_redirects / working_efficiently
//	/ skills / reversibility   (all Cache=true, conditionally fired)
//	provider_hint              (Cache=true, when ctx.ProviderName set)
//	overlays                   (caller-supplied; CoordinatorOverlay,
//	                            PlanOverlay, etc.)
//	project_context            (Cache=false; cwd-dependent)
//	addendum                   (Cache=true; user-level)
//	env                        (Cache=false, Volatile=true)
//
// Same cache-ordering rules as AssembleSystemPromptSections — see the
// CACHE-C comment there.
//
// Callers in main.go should prefer this; AssembleSystemPromptSections
// is kept for backward compat with tests + agent-profile paths that
// already compose their own base string.
func AssembleSystemPromptSectionsCtx(ctx PromptCtx, opts AssembleOptions) []SystemPromptSection {
	mode := opts.resolvedMode()
	secs := AssembleBaseSections(ctx)

	// Provider hint as its own cached section (so a provider switch
	// at runtime doesn't bust the upstream base cache, and so the
	// hint is visually distinct in --prompt-dump output).
	if hint := ProviderHintFor(ctx.ProviderName, ctx.Model); hint != "" {
		secs = append(secs, SystemPromptSection{
			Name:  "provider_hint",
			Body:  hint,
			Cache: true,
		})
	}

	for _, ov := range opts.Overlays {
		secs = append(secs, ov)
	}
	if mode != PromptMinimal {
		if proj := loadProjectContext(); proj != "" {
			secs = append(secs, SystemPromptSection{Name: "project_context", Body: proj, Cache: false})
		}
	}
	if mode == PromptFull {
		if addendum := loadSystemPromptAddendum(); addendum != "" {
			secs = append(secs, SystemPromptSection{Name: "addendum", Body: addendum, Cache: true})
		}
	}
	if !opts.SkipEnv {
		secs = append(secs, SystemPromptSection{Name: "env", Body: buildEnvBlock(), Cache: false, Volatile: true})
		secs = appendGitContextSection(secs, mode)
	}
	return secs
}

// appendGitContextSection adds the git workspace snapshot (claude-code's
// getGitStatus attachment, context.ts:124-142). Volatile like env — it
// changes every boot — and skipped for minimal (sub-agent) prompts,
// whose parents already distilled the relevant workspace state into
// the task. Shared by both Assemble* constructors so the gating can't
// drift between them.
func appendGitContextSection(secs []SystemPromptSection, mode PromptMode) []SystemPromptSection {
	if mode == PromptMinimal {
		return secs
	}
	if gitCtx := buildGitContext(); gitCtx != "" {
		secs = append(secs, SystemPromptSection{Name: "git_context", Body: gitCtx, Cache: false, Volatile: true})
	}
	return secs
}

// AssembleSystemPromptSections is the array-form constructor. Returns
// the same content AssembleSystemPromptWithOptions would produce, but
// as a typed list the anthropic provider can iterate without re-
// splitting on boundary markers.
//
// The returned slice is in render order. Stable ordering:
//
//	base (Cache=true)
//	env  (Cache=false; today's date kills the cache anyway)
//	project_context (Cache=false; cwd-dependent)
//	addendum (Cache=true; user-level, stable across cwd)
//
// Plus any overlays the caller injects via opts.Overlays (OC-A).
func AssembleSystemPromptSections(base string, opts AssembleOptions) []SystemPromptSection {
	mode := opts.resolvedMode()
	var secs []SystemPromptSection
	if base != "" {
		secs = append(secs, SystemPromptSection{Name: "base", Body: base, Cache: true})
	}
	// Provider/extension overlays — OC-A. Inserted between base and
	// the static-per-project sections so they're cached alongside base
	// when their bodies are stable (provider name doesn't change
	// mid-session).
	for _, ov := range opts.Overlays {
		secs = append(secs, ov)
	}
	// IMPORTANT: ordering policy (2026-05-11, CACHE-C).
	// All sections whose contents are stable across requests come
	// FIRST so they share a byte-stable cache prefix:
	//   base → overlays → project_context → addendum
	// then volatile env LAST. This matters for both providers:
	//   - Anthropic: lets the cache_control marker on the last static
	//     section (addendum or project_context) cache everything up
	//     to that point.
	//   - OpenAI / DeepSeek / Kimi / MiniMax-OpenAI-mode: implicit
	//     prefix matching only credits the longest byte-stable prefix;
	//     moving env to the front truncates that prefix on every
	//     date/branch change. With env last, only the env block itself
	//     is uncacheable, and the entire base + project body still
	//     hits cache.
	// Prior layout (base → overlays → env → project_context → addendum)
	// was wrong: env's volatility nuked everything after it on
	// implicit-caching providers.
	if mode != PromptMinimal {
		if proj := loadProjectContext(); proj != "" {
			secs = append(secs, SystemPromptSection{Name: "project_context", Body: proj, Cache: false})
		}
	}
	if mode == PromptFull {
		if addendum := loadSystemPromptAddendum(); addendum != "" {
			secs = append(secs, SystemPromptSection{Name: "addendum", Body: addendum, Cache: true})
		}
	}
	// Env LAST — see comment above.
	if !opts.SkipEnv {
		secs = append(secs, SystemPromptSection{Name: "env", Body: buildEnvBlock(), Cache: false, Volatile: true})
		secs = appendGitContextSection(secs, mode)
	}
	return secs
}

// RenderSections joins sections back into the legacy string form,
// preserving cache boundary markers so existing callers that consume
// `string` (and use the anthropic provider's marker-based splitter)
// keep working unchanged. New code should pass the []Section directly
// to anthropic.BuildSystemBlocksFromSections instead.
func RenderSections(secs []SystemPromptSection) string {
	var b strings.Builder
	for i, s := range secs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s.Body)
		// Boundary marker after each cached section (except the last
		// one — trailing marker confuses the parser). Two markers max
		// because anthropic.buildSystemBlocks understands at most two.
		if s.Cache && !s.Volatile && i < len(secs)-1 {
			next := i + 1
			if next < len(secs) && !secs[next].Cache {
				b.WriteString("\n\n")
				b.WriteString(anthropic.SystemPromptCacheBoundary)
			} else if next < len(secs) && secs[next].Cache {
				// Section after this is cacheable too — use boundary 2
				// to mark the addendum-style separation.
				b.WriteString("\n\n")
				b.WriteString(anthropic.SystemPromptCacheBoundary2)
			}
		}
	}
	return b.String()
}

// AssembleSystemPrompt joins the built-in default system prompt with
// (a) the runtime environment block, (b) any project context file
// found by walking cwd → git root, and (c) the user's optional
// ~/.metis/system.md addendum.
//
// Order is intentional:
//
//  1. base                       — agent identity / tool primer
//  2. <env>...</env> block       — concrete cwd / HOME / OS so the
//     LLM doesn't hallucinate paths like
//     /home/user/.claude on macOS (real bug
//     report 2026-04-30, MiniMax kept guessing
//     Linux paths)
//  3. <project_context>...</     — first hit walking cwd → git root,
//     scanned for prompt-injection markers
//     before being trusted
//  4. user's system.md addendum  — wins last so "always reply in
//     Chinese" / "use shadcn" etc. override
//     behaviour without losing the core primer
func AssembleSystemPrompt(base string) string {
	return AssembleSystemPromptWithOptions(base, AssembleOptions{})
}

// AssembleSystemPromptWithOptions is the option-bearing variant.
func AssembleSystemPromptWithOptions(base string, opts AssembleOptions) string {
	// Layout (top → bottom):
	//
	//   base
	//   <<<__METIS_CACHE_BOUNDARY__>>>     ← split point for prompt cache
	//   <env>...</env>                     ← cwd, hostname, today's date
	//   <project_context>...</               ← CLAUDE.md / AGENTS.md / METIS.md
	//   <user-addendum>                      ← ~/.metis/system.md
	//
	// Why split here: `base` is the agent identity + tool primer — it's
	// stable across sessions, users, and time. Everything below the
	// boundary changes per-call (cwd from `cd elsewhere`, today's date,
	// project file edits). The Anthropic provider's buildSystemBlocks
	// reads this marker and emits `[static (cache_control), dynamic]`
	// so the static prefix gets ~10% billing on cache hit.
	mode := opts.resolvedMode()
	out := base
	if !opts.SkipEnv {
		out += "\n\n" + anthropic.SystemPromptCacheBoundary + "\n\n" + buildEnvBlock()
	}
	// Medium and Full include project_context. Only Minimal skips it.
	if mode != PromptMinimal {
		if proj := loadProjectContext(); proj != "" {
			out += "\n\n" + proj
		}
	}
	// Only Full includes the user addendum. Medium drops it because
	// "personal preferences" are out of scope for a coordinator
	// worker dispatching to a narrowly-scoped sub-agent — the
	// project_context belongs to the project (everyone sees it),
	// the addendum belongs to the human at the keyboard.
	if mode == PromptFull {
		if addendum := loadSystemPromptAddendum(); addendum != "" {
			// Second cache boundary: addendum is user-level (~/.metis/
			// system.md) and stays stable across cwd changes, so caching
			// it independently lets the cache survive `cd otherproj`
			// even when project_context above invalidates.
			out += "\n\n" + anthropic.SystemPromptCacheBoundary2 + "\n\n" + addendum
		}
	}
	return out
}

// projectContextCandidates is the ordered list of filenames metis looks
// for as project context. CLAUDE.md is first because it's the de-facto
// cross-tool convention; AGENTS.md is the OpenAI/codex convention;
// METIS.md is metis-specific. .metis/CLAUDE.md is checked so users can
// stash the file in a hidden dir.
//
// Note: .claude/CLAUDE.md is NOT in this list — that's Claude Code's
// global instruction file (~/.claude/CLAUDE.md or ./.claude/CLAUDE.md)
// and reading it makes metis report the user's claude-code instructions
// as if they were metis-project instructions, which confuses the LLM
// (real bug 2026-05-05: metis answered "your project instructions are
// in .claude/CLAUDE.md" while running in a non-project cwd).
var projectContextCandidates = []string{
	"CLAUDE.md",
	"AGENTS.md",
	"METIS.md",
	filepath.Join(".metis", "CLAUDE.md"),
}

// loadProjectContext walks from cwd UP to the nearest git root (or
// $HOME, whichever comes first) and gathers EVERY matching project-
// context file found along the way. Originally returned just the
// first hit; 2026-05-09 changed to multi-file aggregation so a
// monorepo like:
//
//	repo/CLAUDE.md                  ← repo-wide conventions
//	repo/services/foo/AGENTS.md     ← service-specific overrides
//
// can both inform the model when cwd=repo/services/foo. Mirrors
// hermes-agent's `subdirectory_hints` accumulator and claude-code's
// loadCLAUDE.md walking.
//
// Order: most-specific first (cwd, then parents). The model treats
// later (less-specific) entries as background context and earlier
// (more-specific) ones as the local truth — same precedence the
// user would intuit when reading them top-down.
//
// Each file's body is scanned for prompt-injection markers; flagged
// bodies are replaced with a warning so a malicious AGENTS.md can't
// jailbreak the agent silently.
//
// Dedup: same realpath only counted once. A subdirectory CLAUDE.md
// symlinking back to the repo root would otherwise inject the same
// text twice.
func loadProjectContext() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	var blocks []string
	seen := make(map[string]bool)
	for dir := cwd; ; {
		for _, name := range projectContextCandidates {
			path := filepath.Join(dir, name)
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			body := strings.TrimSpace(string(b))
			if body == "" {
				continue
			}
			canon := canonicalPath(path)
			if seen[canon] {
				continue
			}
			seen[canon] = true
			body = scanForInjection(body, path)
			// Wrap in a labeled block so the LLM knows where this
			// content came from (helps it cite "per CLAUDE.md...").
			blocks = append(blocks, "<project_context source=\""+path+"\">\n"+body+"\n</project_context>")
		}
		// Stop walking up when we hit:
		//   - the git root (we want repo-level CLAUDE.md, not the
		//     user's home CLAUDE.md leaking across projects)
		//   - $HOME (defensive — if no .git, never walk into home)
		//   - filesystem root
		if isGitRoot(dir) || dir == home || dir == "/" || dir == filepath.Dir(dir) {
			break
		}
		dir = filepath.Dir(dir)
	}
	return strings.Join(blocks, "\n\n")
}

// canonicalPath resolves symlinks for project-context dedup. Mirrors
// the helper used by skills.dirLayer — same idempotence story
// (broken symlinks fall through unchanged).
func canonicalPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// isGitRoot reports whether dir contains a .git directory or file.
// Cheap stat — runs once per loadProjectContext walk iteration.
func isGitRoot(dir string) bool {
	if st, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		_ = st
		return true
	}
	return false
}

// scanForInjection runs a cheap pattern scan on project-context content
// and either returns the body unchanged or replaces it with a warning
// when it looks like an attempt to override the agent's core
// instructions. Mirrors hermes-agent's prompt_builder.py:36-73 threat
// model: project context is user-supplied; an attacker who landed a
// malicious CLAUDE.md in a repo can otherwise jailbreak the agent.
//
// Patterns matched (case-insensitive):
//
//   - "ignore (previous|prior|all|above) instructions"
//   - "disregard ... instructions"
//   - "you are now (a|an) ..."
//   - "system prompt"     — bare mention is suspicious in a project
//     context file (legit docs would say "the system prompt", "system
//     prompts in general"; bare imperative is the jailbreak shape)
//   - "act as (a|an|if) ..."
//   - "pretend ..."
//   - role-impersonation: "I am (the )?(developer|admin|operator)"
//   - data-exfil keywords near credential paths: "send .ssh/", "exfil"
//
// Detection isn't airtight — anyone determined enough can paraphrase.
// The goal is the same as bash_security_rules: catch the obvious
// shapes and force a deliberate paraphrase, raising the floor.
func scanForInjection(body, path string) string {
	hits := detectInjectionPatterns(body)
	if len(hits) == 0 {
		return body
	}
	// Replace the body with a warning. The original is still on disk;
	// the user can review. Showing the patterns hit helps the user
	// audit what tripped the scanner.
	var b strings.Builder
	b.WriteString("⚠️ project context at ")
	b.WriteString(path)
	b.WriteString(" was rejected by the prompt-injection scanner.\n")
	b.WriteString("Patterns matched: ")
	b.WriteString(strings.Join(hits, ", "))
	b.WriteString("\n")
	b.WriteString("Review the file or rephrase to drop the suspicious patterns. ")
	b.WriteString("If this is a false positive, set METIS_DISABLE_INJECTION_SCAN=1.")
	if os.Getenv("METIS_DISABLE_INJECTION_SCAN") == "1" {
		// Honour the override but still log to the model so it knows
		// the file was suspect.
		return "<!-- metis: injection scanner bypassed via METIS_DISABLE_INJECTION_SCAN -->\n" + body
	}
	return b.String()
}

// detectInjectionPatterns is the regex-free pattern list. Returns the
// set of pattern names that matched. Case-insensitive substring
// matches; cheap and predictable.
func detectInjectionPatterns(body string) []string {
	low := strings.ToLower(body)
	type pat struct {
		name    string
		needles []string // any-of within a single name
	}
	patterns := []pat{
		{"ignore-instructions", []string{
			"ignore previous instructions",
			"ignore prior instructions",
			"ignore all instructions",
			"ignore the above instructions",
			"ignore previous",
			"disregard previous instructions",
			"disregard the above",
			"disregard all prior",
		}},
		{"role-override", []string{
			"you are now a ",
			"you are now an ",
			"act as a ",
			"act as an ",
			"pretend you are ",
			"pretend to be ",
		}},
		{"system-prompt-mention", []string{
			"reveal your system prompt",
			"print your system prompt",
			"output the system prompt",
			"what is your system prompt",
		}},
		{"impersonation", []string{
			"i am the developer",
			"i am the admin",
			"i am the operator",
			"as the developer",
			"as the admin",
		}},
		{"exfil-keywords", []string{
			"exfiltrate",
			"send .ssh/",
			"upload ~/.ssh",
			"cat ~/.ssh/id_rsa",
			"post .env to",
		}},
	}
	var hits []string
	for _, p := range patterns {
		for _, n := range p.needles {
			if strings.Contains(low, n) {
				hits = append(hits, p.name)
				break
			}
		}
	}
	return hits
}

// buildEnvBlock collects the runtime facts the LLM should see verbatim
// before answering anything path-y. Cheap to compute (a few syscalls)
// so we do it on every chat boot rather than caching — that way a
// /clear after `cd otherproj` picks up the new cwd.
func buildEnvBlock() string {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	uname := ""
	if u, err := user.Current(); err == nil {
		uname = u.Username
	}
	host, _ := os.Hostname()

	var b strings.Builder
	b.WriteString("<env>\n")
	if cwd != "" {
		fmt.Fprintf(&b, "Working directory: %s\n", cwd)
	}
	if home != "" {
		fmt.Fprintf(&b, "Home directory: %s\n", home)
	}
	if uname != "" {
		fmt.Fprintf(&b, "User: %s\n", uname)
	}
	if host != "" {
		fmt.Fprintf(&b, "Hostname: %s\n", host)
	}
	fmt.Fprintf(&b, "Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Today's date: %s\n", time.Now().Format("2006-01-02"))
	if branch := currentGitBranch(); branch != "" {
		fmt.Fprintf(&b, "Git branch: %s\n", branch)
	}
	b.WriteString("</env>")
	return b.String()
}

// currentGitBranch returns the active branch name, or "" when the cwd
// isn't a git repo. Cached implicitly per chat boot via the
// buildEnvBlock callsite. Goes through gitOut so a frozen NFS mount /
// gc-ing monorepo can't hang chat boot — pre-2026-06-11 this was a
// bare exec.Command with no deadline.
func currentGitBranch() string {
	ctx, cancel := context.WithTimeout(context.Background(), gitContextTimeout)
	defer cancel()
	return gitOut(ctx, "rev-parse", "--abbrev-ref", "HEAD")
}

func loadSystemPromptAddendum() string {
	path := filepath.Join(config.Home(), SystemPromptFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		return "" // missing / unreadable → no addendum
	}
	s := strings.TrimSpace(string(b))
	return s
}
