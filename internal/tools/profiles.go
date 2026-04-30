package tools

import (
	"fmt"
)

// ToolProfile defines a named set of allowed/denied tools.
// Inspired by OpenClaw's tool profiles (minimal, coding, messaging, full).
type ToolProfile struct {
	ID          string
	Name        string
	Description string
	Allowed     []string // if non-empty, only these tools are allowed
	Denied      []string // tools to deny
	Default     bool     // is this the default profile
}

// Predefined tool profiles
var ToolProfiles = []ToolProfile{
	{
		ID:          "minimal",
		Name:        "Minimal",
		Description: "Read-only tools only: Read, Glob, Grep, WebFetch",
		Allowed:     []string{"Read", "Glob", "Grep", "WebFetch"},
		Default:     false,
	},
	{
		ID:          "coding",
		Name:        "Coding",
		Description: "Development tools: all fs tools + terminal",
		Allowed:     []string{"Read", "Write", "Edit", "Glob", "Grep", "LS", "Bash", "Git", "Search"},
		Default:     true,
	},
	{
		ID:          "messaging",
		Name:        "Messaging",
		Description: "Communication tools: Read, WebFetch, Bash (curl)",
		Allowed:     []string{"Read", "Glob", "Grep", "WebFetch", "Bash"},
		Default:     false,
	},
	{
		ID:          "full",
		Name:        "Full",
		Description: "All tools enabled",
		Denied:      []string{},
		Default:     false,
	},
}

// ProfileRegistry manages tool profiles.
type ProfileRegistry struct {
	profiles map[string]*ToolProfile
	default_ *ToolProfile
}

func NewProfileRegistry() *ProfileRegistry {
	r := &ProfileRegistry{
		profiles: make(map[string]*ToolProfile),
	}
	for i := range ToolProfiles {
		p := &ToolProfiles[i]
		r.profiles[p.ID] = p
		if p.Default {
			r.default_ = p
		}
	}
	if r.default_ == nil {
		r.default_ = r.profiles["coding"]
	}
	return r
}

func (r *ProfileRegistry) Get(id string) (*ToolProfile, bool) {
	p, ok := r.profiles[id]
	return p, ok
}

func (r *ProfileRegistry) Default() *ToolProfile {
	return r.default_
}

func (r *ProfileRegistry) List() []*ToolProfile {
	out := make([]*ToolProfile, 0, len(r.profiles))
	for _, p := range r.profiles {
		out = append(out, p)
	}
	return out
}

// IsAllowed checks if a tool is allowed by the profile.
func (p *ToolProfile) IsAllowed(toolName string) bool {
	if len(p.Allowed) > 0 {
		for _, a := range p.Allowed {
			if a == toolName {
				return true
			}
		}
		return false
	}
	for _, d := range p.Denied {
		if d == toolName {
			return false
		}
	}
	return true
}

// ToolFilter applies profile rules to filter tool access.
type ToolFilter struct {
	profile *ToolProfile
}

func NewToolFilter(profileID string) (*ToolFilter, error) {
	r := NewProfileRegistry()
	p, ok := r.Get(profileID)
	if !ok {
		return nil, fmt.Errorf("unknown tool profile: %s", profileID)
	}
	return &ToolFilter{profile: p}, nil
}

func (f *ToolFilter) IsAllowed(toolName string) bool {
	return f.profile.IsAllowed(toolName)
}

func (f *ToolFilter) ProfileName() string {
	return f.profile.Name
}
