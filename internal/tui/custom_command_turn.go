package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/slash"
)

// customCommandFromInput resolves the command metadata that Registry.Parse
// intentionally leaves out of its compact return tuple. Aliases resolve through
// Registry.Get, so the same frontmatter applies whichever spelling was used.
func customCommandFromInput(reg *slash.Registry, input string) *slash.Cmd {
	if reg == nil {
		return nil
	}
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	head := strings.TrimPrefix(input, "/")
	if i := strings.IndexAny(head, " \t\r\n"); i >= 0 {
		head = head[:i]
	}
	cmd, ok := reg.Get(head)
	if !ok || !cmd.Custom {
		return nil
	}
	return cmd
}

// prepareCustomCommandTurn validates and attaches trusted frontmatter to one
// Loop.Run. Model overrides cannot be faked by changing Loop.Model because the
// provider owns its selected model; until the user switches providers/models
// normally, only an exact match (or `inherit`) is accepted.
func prepareCustomCommandTurn(ctx context.Context, cmd *slash.Cmd, loop *agent.Loop) (context.Context, []string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cmd == nil {
		return ctx, nil, nil
	}
	if !cmd.Trusted {
		var warnings []string
		if strings.TrimSpace(cmd.Model) != "" && !strings.EqualFold(strings.TrimSpace(cmd.Model), "inherit") {
			warnings = append(warnings, fmt.Sprintf(
				"/%s: ignored model frontmatter from an untrusted project command; use /model explicitly if desired",
				cmd.Name,
			))
		}
		if len(cmd.AllowedTools) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"/%s: ignored allowed-tools frontmatter from an untrusted project command; normal permission checks remain active",
				cmd.Name,
			))
		}
		return ctx, warnings, nil
	}

	requestedModel := strings.TrimSpace(cmd.Model)
	if requestedModel != "" && !strings.EqualFold(requestedModel, "inherit") {
		currentModel := ""
		if loop != nil {
			currentModel = strings.TrimSpace(loop.Model)
		}
		if requestedModel != currentModel {
			return ctx, nil, fmt.Errorf(
				"/%s requests model %q, but this session is using %q; run /model %s, then retry the command",
				cmd.Name, requestedModel, currentModel, requestedModel,
			)
		}
	}

	rules, warnings := customCommandAllowRules(cmd, loop)
	if len(rules) > 0 {
		ctx = agent.WithTurnPermissionRules(ctx, rules)
	}
	return ctx, warnings, nil
}

// customCommandNeedsFreshTurn reports whether trusted metadata needs a new
// Loop.Run boundary. Prompt-only commands (and a model value already equal to
// the active model) remain safe to inject as ordinary mid-turn steering.
func customCommandNeedsFreshTurn(cmd *slash.Cmd, loop *agent.Loop) bool {
	if cmd == nil || !cmd.Trusted {
		return false
	}
	if len(cmd.AllowedTools) > 0 {
		return true
	}
	requested := strings.TrimSpace(cmd.Model)
	if requested == "" || strings.EqualFold(requested, "inherit") {
		return false
	}
	current := ""
	if loop != nil {
		current = strings.TrimSpace(loop.Model)
	}
	return requested != current
}

func customCommandAllowRules(cmd *slash.Cmd, loop *agent.Loop) ([]permission.Rule, []string) {
	if cmd == nil || len(cmd.AllowedTools) == 0 {
		return nil, nil
	}
	var rules []permission.Rule
	var warnings []string
	for _, raw := range cmd.AllowedTools {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// ParseToolRule deliberately treats an unterminated parenthesis as a
		// bare tool name. For command frontmatter, surface that typo instead of
		// pretending the rule was installed.
		if strings.Contains(raw, "(") && !strings.HasSuffix(raw, ")") {
			warnings = append(warnings, fmt.Sprintf("/%s: ignored malformed allowed-tools entry %q", cmd.Name, raw))
			continue
		}
		toolName, match := permission.ParseToolRule(raw)
		toolName = strings.TrimSpace(toolName)
		if toolName == "" || strings.ContainsAny(toolName, "()") {
			warnings = append(warnings, fmt.Sprintf("/%s: ignored malformed allowed-tools entry %q", cmd.Name, raw))
			continue
		}
		canonical := toolName
		if toolName != "*" {
			if loop == nil || loop.Registry == nil {
				warnings = append(warnings, fmt.Sprintf("/%s: cannot apply allowed-tools entry %q because the tool registry is unavailable", cmd.Name, raw))
				continue
			}
			tool, ok := loop.Registry.Get(toolName)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("/%s: ignored unknown allowed-tools entry %q", cmd.Name, raw))
				continue
			}
			canonical = tool.Name()
		}
		rules = append(rules, permission.Rule{
			Tool: canonical, Match: match, Verb: permission.DecisionAllow,
		})
	}
	return rules, warnings
}
