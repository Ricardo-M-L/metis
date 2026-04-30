// Package channel exposes the public Adapter contract a 3rd-party plugin
// implements to ship messages to a chat platform (Slack-alternative, custom
// internal IRC, voice transport, etc.).
//
// Pairs with pkg/tool, pkg/hook, and pkg/llm — the four-pillar plugin SDK.
// Authoring a new platform adapter is a two-method job:
//
//	type MyChat struct{ token string }
//	func (a *MyChat) Name() string                                             { return "mychat" }
//	func (a *MyChat) Configured() bool                                         { return a.token != "" }
//	func (a *MyChat) Send(ctx context.Context, target string, m channel.Message) error {
//	    // POST to the platform's API
//	}
//
// The runtime constructs a Registry, calls Register on each Adapter you
// hand it, and routes outgoing messages by `<platform>:<target>` prefix.
package channel

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Message is the platform-agnostic payload accepted by every Adapter.
// Adapters that don't support a field (e.g. Title on Slack) ignore it.
type Message struct {
	Text     string
	Title    string
	Markdown bool
}

// Adapter sends a Message to a target on a single platform.
//
//   - Name() returns the platform identifier used in channel prefixes
//     ("slack", "telegram", "mychat", etc.).
//   - Configured() reports whether enough credentials exist for Send to
//     succeed. Registry uses this to gate which platforms get registered.
//   - Send is the network call; ctx cancellation MUST be honoured.
type Adapter interface {
	Name() string
	Configured() bool
	Send(ctx context.Context, target string, msg Message) error
}

// Registry holds the active adapters keyed by Name(). Safe for concurrent
// use — adapters are added once at startup, read many times during a run.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

// Register adds a configured adapter. Adapters whose Configured() returns
// false are silently dropped so the SendMessage tool description stays
// honest about what's actually wired up.
func (r *Registry) Register(a Adapter) {
	if a == nil || !a.Configured() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Name()] = a
}

func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}

// Names returns the configured platform identifiers, sorted for stable
// SendMessage tool descriptions.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for k := range r.adapters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Send routes msg to the adapter matching the channel prefix. `channel` is
// either "<platform>:<target>" (e.g. "slack:#general") or just "<target>",
// in which case `defaultPlatform` is used.
//
// Returns errors.New("...") for unconfigured platforms — callers (the
// SendMessage tool) surface those as a tool error so the LLM can decide
// whether to retry.
func (r *Registry) Send(ctx context.Context, channel, defaultPlatform string, msg Message) error {
	platform, target := ParseChannel(channel, defaultPlatform)
	if platform == "" {
		return errors.New("channels: no platform specified and no default configured")
	}
	a, ok := r.Get(platform)
	if !ok {
		return errors.New("channels: platform not configured: " + platform)
	}
	return a.Send(ctx, target, msg)
}

// ParseChannel splits "<platform>:<target>" into its parts. When no `:`
// prefix is present, returns (defaultPlatform, s).
//
// Exposed publicly because the SendMessage tool description benefits from
// having a single, plugin-agnostic parser everyone agrees on.
func ParseChannel(s, defaultPlatform string) (platform, target string) {
	if i := strings.IndexByte(s, ':'); i > 0 {
		return s[:i], s[i+1:]
	}
	return defaultPlatform, s
}
