package agent

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// SubAgentExecutionConfig preserves the effective child identity needed for a
// parent-owned continuation. Provider is a configured provider name, never a
// credential, endpoint token, or serialized provider client. Tools is the exact
// visible tool list; an explicit empty list grants no tools.
type SubAgentExecutionConfig struct {
	Version             int      `json:"version"`
	Provider            string   `json:"provider,omitempty"`
	Model               string   `json:"model"`
	Effort              string   `json:"effort,omitempty"`
	Fast                bool     `json:"fast,omitempty"`
	WorkDir             string   `json:"workDir"`
	System              string   `json:"system"`
	Profile             string   `json:"profile,omitempty"`
	Tools               []string `json:"tools"`
	PermissionMode      string   `json:"permissionMode"`
	PermissionRulesHash string   `json:"permissionRulesHash,omitempty"`
	// LegacyMigration distinguishes an explicitly reported reconstruction of an
	// old durable mailbox from a descriptor captured at its original spawn.
	LegacyMigration bool `json:"legacyMigration,omitempty"`
}

// Validate rejects an incomplete or unsupported execution identity rather than
// allowing a caller to resume it with broader current-parent defaults.
func (c SubAgentExecutionConfig) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported subagent execution version %d", c.Version)
	}
	if strings.TrimSpace(c.Model) == "" || c.Model != strings.TrimSpace(c.Model) {
		return fmt.Errorf("subagent execution requires a model")
	}
	if !filepath.IsAbs(c.WorkDir) {
		return fmt.Errorf("subagent execution requires an absolute work directory")
	}
	mode, ok := permission.ParseMode(c.PermissionMode)
	if !ok || string(mode) != c.PermissionMode {
		return fmt.Errorf("subagent execution requires a canonical permission mode")
	}
	if c.Tools == nil {
		return fmt.Errorf("subagent execution requires an explicit tool list")
	}
	seen := make(map[string]struct{}, len(c.Tools))
	for _, name := range c.Tools {
		if name == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("subagent execution contains an invalid tool name")
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("subagent execution repeats tool %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func cloneSubAgentExecution(c *SubAgentExecutionConfig) *SubAgentExecutionConfig {
	if c == nil {
		return nil
	}
	copy := *c
	copy.Tools = append([]string{}, c.Tools...)
	return &copy
}

// AppendExecution durably binds one immutable execution identity. Repeating
// the same binding is idempotent; changing it requires a new child session.
func (t *SubAgentTranscript) AppendExecution(config SubAgentExecutionConfig) error {
	if t == nil {
		return fmt.Errorf("durable subagent transcript unavailable")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return fmt.Errorf("subagent transcript is closed")
	}
	if t.execution != nil {
		if !reflect.DeepEqual(*t.execution, config) {
			return fmt.Errorf("subagent execution identity cannot change on resume")
		}
		return t.syncLocked()
	}
	bound := cloneSubAgentExecution(&config)
	if err := t.writeEntryLocked(subagentEntry{Type: "execution", Execution: bound}); err != nil {
		return err
	}
	if err := t.syncLocked(); err != nil {
		return err
	}
	t.execution = bound
	return nil
}

func readSubAgentExecution(current *SubAgentExecutionConfig, next *SubAgentExecutionConfig) (*SubAgentExecutionConfig, error) {
	if next == nil {
		return nil, fmt.Errorf("missing subagent execution identity")
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if current != nil && !reflect.DeepEqual(*current, *next) {
		return nil, fmt.Errorf("subagent execution identity changed")
	}
	return cloneSubAgentExecution(next), nil
}
