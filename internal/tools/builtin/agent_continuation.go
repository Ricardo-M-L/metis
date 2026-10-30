package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func (a Agent) executionProviderName() string {
	if a.promptProviderName != "" {
		return a.promptProviderName
	}
	if a.provider != nil {
		return a.provider.Name()
	}
	return ""
}

func agentPermissionRulesHash(gate *permission.Gate) string {
	if gate == nil {
		return ""
	}
	type rulePolicy struct {
		Tool, Match string
		Verb        permission.Decision
		Authority   int
	}
	policies := make([]rulePolicy, 0)
	seen := make(map[rulePolicy]bool)
	rules := gate.Snapshot()
	// Source diagnostic labels change on session restore. Preserve policy
	// authority and last-rule-wins order within each authority, not that label.
	for index := len(rules) - 1; index >= 0; index-- {
		rule := rules[index]
		authority := 40
		switch {
		case strings.HasPrefix(rule.Source, "policy"):
			authority = 100
		case strings.HasPrefix(rule.Source, "cli"):
			authority = 80
		case rule.Source == "interactive" || strings.HasPrefix(rule.Source, "session"):
			authority = 60
		case rule.Source == "persistent":
			authority = 20
		}
		policy := rulePolicy{Tool: rule.Tool, Match: rule.Match, Verb: rule.Verb, Authority: authority}
		if !seen[policy] {
			policies = append(policies, policy)
			seen[policy] = true
		}
	}
	sort.SliceStable(policies, func(i, j int) bool { return policies[i].Authority < policies[j].Authority })
	encoded, _ := json.Marshal(policies)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func (a Agent) subAgentExecution(loop *agent.Loop, cwd, system, profile string, registry *tools.Registry, gate *permission.Gate) agent.SubAgentExecutionConfig {
	names := make([]string, 0)
	for _, tool := range registry.All() {
		names = append(names, tool.Name())
	}
	sort.Strings(names)
	return agent.SubAgentExecutionConfig{
		Version: 1, Provider: a.executionProviderName(), Model: a.model,
		Effort: string(loop.EffortValue()), Fast: loop.FastEnabled(), WorkDir: cwd,
		System: system, Profile: profile, Tools: names,
		PermissionMode: string(gate.Mode()), PermissionRulesHash: agentPermissionRulesHash(a.gate),
	}
}

func (a *Agent) restoreSubAgentExecution(snapshot *agent.SubAgentSnapshot, input map[string]any) error {
	config := snapshot.Execution
	if config == nil {
		// Existing durable mailboxes predate execution descriptors. Their address
		// remains usable only under matching known execution settings. Ordinary
		// legacy transcripts cannot establish the original tool/profile boundary.
		if snapshot.Handoff == nil {
			return fmt.Errorf("legacy sub-agent has no saved execution identity; start a new Agent task instead of silently changing its model, tools, or permissions")
		}
		for _, field := range []string{"subagent_type", "allowed_tools", "disallowed_tools"} {
			text, _ := input[field].(string)
			if strings.TrimSpace(text) != "" || len(stringSliceArg(input, field)) > 0 {
				return fmt.Errorf("legacy durable sub-agent cannot change its profile or tool filters during migration")
			}
		}
		if value, _ := input["isolation"].(string); value != "" && value != "none" {
			return fmt.Errorf("legacy durable sub-agent cannot create a new isolation worktree during migration")
		}
		for field, expected := range map[string]string{"cwd": snapshot.Header.WorkDir, "permission_mode": snapshot.Header.Mode} {
			if value, _ := input[field].(string); value != "" && value != expected {
				return fmt.Errorf("legacy durable sub-agent cannot change its saved %s during migration", field)
			}
		}
		if snapshot.Header.Model != a.model || (snapshot.Header.Provider != "" && snapshot.Header.Provider != a.executionProviderName()) ||
			(snapshot.Header.Mode != "" && permission.CanonicalMode(snapshot.Header.Mode) != a.gate.Mode()) {
			return fmt.Errorf("legacy durable sub-agent requires its original model, provider, and permission mode")
		}
		return nil
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if !agentPermissionModeOverrideAllowed(a.gate.Mode(), permission.CanonicalMode(config.PermissionMode)) {
		return fmt.Errorf("saved child permission mode %q cannot be enforced under current parent mode %q; restore the original parent mode or start a new child", config.PermissionMode, a.gate.Mode())
	}
	if config.PermissionRulesHash != "" && config.PermissionRulesHash != agentPermissionRulesHash(a.gate) {
		return fmt.Errorf("parent permission rules changed; start a new child with the new policy instead of changing this child's saved boundary")
	}
	checks := map[string]string{"cwd": config.WorkDir, "subagent_type": config.Profile, "name": snapshot.Header.TeammateName, "permission_mode": config.PermissionMode}
	for field, expected := range checks {
		if value, _ := input[field].(string); value != "" && value != expected {
			return fmt.Errorf("cannot change %s when continuing an existing child; start a new Agent task", field)
		}
	}
	if value, _ := input["isolation"].(string); value != "" && value != "none" {
		return fmt.Errorf("cannot create a new isolation worktree while continuing an existing child")
	}
	if len(stringSliceArg(input, "allowed_tools")) != 0 || len(stringSliceArg(input, "disallowed_tools")) != 0 {
		return fmt.Errorf("cannot replace the saved child tool filter during continuation; start a new Agent task")
	}
	provider := a.provider
	if a.resolveProvider != nil {
		resolved, err := a.resolveProvider(config.Provider, config.Model)
		if err != nil {
			return fmt.Errorf("restore child provider: %w", err)
		}
		if resolved == nil {
			return fmt.Errorf("saved child provider is unavailable")
		}
		provider = resolved
	} else if config.Provider != a.executionProviderName() {
		return fmt.Errorf("saved child provider %q is unavailable; refusing to substitute %q", config.Provider, a.executionProviderName())
	}
	a.provider, a.model, a.promptProviderName = provider, config.Model, config.Provider
	a.promptWorkDir = config.WorkDir
	a.effort = func() llm.Effort { return llm.Effort(config.Effort) }
	a.fast = func() bool { return config.Fast }
	return nil
}

func decorateAgentLegacyMigration(result *tools.Result, snapshot *agent.SubAgentSnapshot) *tools.Result {
	if result == nil || snapshot == nil || !((snapshot.Execution == nil && snapshot.Handoff != nil) ||
		(snapshot.Execution != nil && snapshot.Execution.LegacyMigration)) {
		return result
	}
	result.Output = "Legacy durable child migration: the original profile and tool filters were not recorded. This continuation uses the current parent's tool boundary and verified saved model/permission settings; it is not a faithful reconstruction of every original setting.\n\n" + result.Output
	if result.Meta == nil {
		result.Meta = make(map[string]any)
	}
	result.Meta["legacy_migration"] = true
	return result
}

func continuationInput(input map[string]any, snapshot *agent.SubAgentSnapshot) map[string]any {
	copy := make(map[string]any, len(input)+2)
	for key, value := range input {
		copy[key] = value
	}
	if snapshot.Execution != nil {
		copy["cwd"] = snapshot.Execution.WorkDir
		copy["subagent_type"] = snapshot.Execution.Profile
	} else if snapshot.Header.WorkDir != "" {
		copy["cwd"] = snapshot.Header.WorkDir
	}
	return copy
}

// exactChildToolRegistry intersects saved capabilities with the current
// parent registry. An explicit empty saved list means no tools, not inherit.
func exactChildToolRegistry(parent *tools.Registry, names []string) *tools.Registry {
	registry := tools.NewRegistry()
	for _, name := range names {
		if tool, ok := parent.Get(name); ok {
			registry.Register(tool)
		}
	}
	return registry
}

func (a Agent) continueLiveSubAgent(ctx context.Context, input map[string]any, teammate *agent.Teammate, prompt string) (*tools.Result, error) {
	header, err := agent.LoadSubAgentHeader(a.sessionDir, teammate.AgentID)
	if err != nil || header.SubAgentOf != a.parentSessionID {
		return &tools.Result{Output: "resume_from refused: only the original direct parent can continue a persisted running sub-agent", IsError: true}, nil
	}
	if a.gate.Mode() == permission.ModePlan && permission.CanonicalMode(header.Mode) != permission.ModePlan {
		return &tools.Result{Output: "plan mode cannot continue an implementation sub-agent; stop it and create a read-only planning child", IsError: true}, nil
	}
	if !agentPermissionModeOverrideAllowed(a.gate.Mode(), permission.CanonicalMode(header.Mode)) {
		return &tools.Result{Output: "current parent permissions cannot enforce the running child's original mode; stop it before creating a new child", IsError: true}, nil
	}
	for _, field := range []string{"cwd", "subagent_type", "allowed_tools", "disallowed_tools", "isolation", "permission_mode"} {
		value, _ := input[field].(string)
		if (strings.TrimSpace(value) != "" && !(field == "isolation" && value == "none")) || len(stringSliceArg(input, field)) > 0 {
			return &tools.Result{Output: "active child continuation accepts a prompt only; do not replace its execution settings", IsError: true}, nil
		}
	}
	receipt, err := teammate.SubmitContinuation(a.parentSessionID, prompt)
	if err != nil {
		guidance := "acceptance was not confirmed; inspect the child's history and status before repeating this prompt"
		if errors.Is(err, agent.ErrSubAgentContinuationClosed) {
			guidance = "the previous run is closing; wait for completion and retry resume_from"
		}
		return &tools.Result{Output: fmt.Sprintf("sub-agent continuation: %v; %s (SubAgentStop remains available)", err, guidance), IsError: true}, nil
	}
	return &tools.Result{
		Output: fmt.Sprintf("parent follow-up durably accepted for the same sub-agent (agent_id=%s, input_id=%s, continuation=%s). No new child was started. Poll SubAgentOutput for the result.", teammate.AgentID, receipt.InputID, receipt.Status),
		Meta:   map[string]any{"agent_id": teammate.AgentID, "input_id": receipt.InputID, "continuation": true, "status": teammate.Snapshot().Status.String(), "delivery": receipt.Status},
	}, nil
}

// Nested delegation must remain within the direct child's identity and tool
// palette. Copying the parent's Agent tool would retain the root session ID
// and its broader registry, even after the outer child palette was narrowed.
func rebindNestedChildAgent(registry *tools.Registry, parentID string, gate *permission.Gate, provider llm.Provider, providerName, model, system, cwd string, effort func() llm.Effort, fast func() bool) {
	tool, ok := registry.Get("Agent")
	if !ok {
		return
	}
	for {
		bound, ok := tool.(agentPermissionBoundTool)
		if !ok {
			break
		}
		tool = bound.inner
	}
	var nested Agent
	switch current := tool.(type) {
	case Agent:
		nested = current
	case *Agent:
		nested = *current
	default:
		return
	}
	nested.registry, nested.gate = registry, gate
	nested.provider, nested.model = provider, model
	nested.promptProviderName, nested.promptWorkDir = providerName, cwd
	nested.parentSessionID = parentID
	nested.system, nested.minimalSystem = system, system
	nested.effort, nested.fast = effort, fast
	registry.Replace(agentPermissionBoundTool{inner: nested, gate: gate})
}
