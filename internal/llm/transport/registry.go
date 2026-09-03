package transport

// Transport registry. Each provider subpackage registers a constructor
// for one or more transport names via init(); runtime/provider.go
// calls Lookup(transport) to dispatch without an ever-growing switch.
//
// Why init-time registration vs an explicit switch:
//   - Adding a new provider becomes "create the subpackage, call
//     transport.Register in init()" — no edit to runtime/provider.go
//     or any central file.
//   - 3rd-party plugins built as Go modules can register their own
//     transport without forking metis. (Plugin discovery itself isn't
//     wired yet — that's a future phase — but the contract is here.)
//
// Trade-off: init() side-effects make the dependency graph implicit.
// The runtime/provider.go BuildProvider reads from a map populated by
// import side effects, so adding `_ "metis/internal/llm/azure"` is
// what wires Azure in — easy to miss when refactoring. We mitigate by
// having runtime/provider.go import every blessed subpackage with a
// blank import explicitly.

import (
	"fmt"
	"sort"
	"sync"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// BuildOpts are the per-call inputs every transport constructor
// shares. We pass by struct rather than positional args so adding a
// new field (e.g. context cancellation, logger) doesn't break every
// constructor signature.
type BuildOpts struct {
	APIKey          string
	BaseURL         string
	Model           string
	CatalogProvider string
	MaxTokens       int
	Timeout         int // seconds; 0 = use provider default
	// Extra is a flat string/string bag for transport-specific
	// settings (Azure api_version, Vertex project/region, Bedrock
	// secret_key/session_token, etc.) — keeps the registry signature
	// agnostic to one transport's quirks. Subpackages document the
	// keys they read.
	Extra map[string]string
	// ContextWindow override (forwarded to the provider's MaxContextTokens
	// field). 0 means "let the provider's heuristic decide".
	ContextWindow int
}

// DefaultMaxOutputTokens is the effective transport reservation when a
// profile omits max_tokens. Constructors and runtime metadata must share this
// value; otherwise the wire reserves 8192 while compaction incorrectly sees 0.
const DefaultMaxOutputTokens = 8192

// Result wraps the built provider plus the resolved model id (after
// the provider's internal default-fill). Callers mostly want the
// provider client; some downstream paths (compactor, agent loop) also
// need the resolved model.
type Result struct {
	Provider        provider.Provider
	Model           string
	MaxOutputTokens int
}

// Constructor is the signature every transport registers under its
// transport name. Returning the typed Provider interface keeps the
// registry a true abstraction — callers can't reach into transport-
// specific struct fields after construction.
type Constructor func(opts BuildOpts) (*Result, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Constructor{}
)

// Register installs a constructor under name. Subsequent registrations
// for the same name overwrite — useful for tests, but log-worthy in
// production builds. We keep it silent here and trust the caller (the
// builder for runtime/provider.go) to validate.
func Register(name string, ctor Constructor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if registry == nil {
		registry = map[string]Constructor{}
	}
	registry[name] = ctor
}

// Lookup returns the constructor for name, or ok=false. Callers (only
// runtime/provider.go in practice) handle the not-found case with a
// helpful "available transports: …" error.
func Lookup(name string) (Constructor, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	c, ok := registry[name]
	return c, ok
}

// Names returns all currently-registered transport names, sorted.
// Used by the "unknown transport" error message and by `metis
// models` to flag catalog entries whose npm field doesn't map to
// anything we've registered.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// MustBuild is the runtime-side entrypoint. Returns a wrapped error
// pointing at the transport name + the registry's known set so users
// debugging a typo see what's available.
func MustBuild(name string, opts BuildOpts) (*Result, error) {
	ctor, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("transport %q is not registered. Available: %v", name, Names())
	}
	return ctor(opts)
}
