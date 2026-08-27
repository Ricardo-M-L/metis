package builtin

// agent_subagent_type_test.go — Q1 (2026-05-15) pins the new
// `subagent_type` schema field semantics:
//
//   - subagent_type explicitly set → loader resolves it; profile's
//     SystemPrompt becomes the sub-loop's base prompt (post role
//     preamble), profile.Tools/DisallowedTools merge with the
//     per-invocation filters.
//   - subagent_type empty + name matches a bundled profile slug →
//     name silently doubles as subagent_type for back-compat with
//     the old (pre-Q1) descriptor's documented behavior.
//   - subagent_type empty + name is a regular label → no profile
//     loaded (Roster identity only); sub-loop inherits parent system.
//   - subagent_type set + loader nil → IsError (clear hint).
//   - subagent_type pointing at unknown profile → IsError.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestAgentTool_SubagentType_BackgroundSetupFailureReleasesRoster(t *testing.T) {
	tests := []struct {
		name   string
		loader AgentProfileLoader
	}{
		{
			name:   "loader not wired",
			loader: nil,
		},
		{
			name: "loader error",
			loader: func(string) (*AgentProfileSpec, error) {
				return nil, errors.New("profile store unavailable")
			},
		},
		{
			name: "profile not found",
			loader: func(string) (*AgentProfileSpec, error) {
				return nil, nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			roster := agent.NewRoster(0)
			tool := NewAgent(
				permission.New(permission.ModeBypass),
				helloProvider(),
				tools.NewRegistry(),
				"model",
				"PARENT-SYSTEM",
			).WithRoster(roster)
			if tc.loader != nil {
				tool = tool.WithProfileLoader(tc.loader)
			}

			res, err := tool.Execute(context.Background(), map[string]any{
				"prompt":            "profile setup must fail before runner start",
				"subagent_type":     "missing",
				"run_in_background": true,
			})
			if err != nil {
				t.Fatalf("Execute returned transport error: %v", err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("expected profile setup error, got %+v", res)
			}
			if got := roster.Count(); got != 0 {
				t.Fatalf("profile setup failure leaked %d live roster entries; want 0", got)
			}

			joinCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			if err := roster.CancelAndWait(joinCtx); err != nil {
				t.Fatalf("CancelAndWait should return immediately after setup failure: %v", err)
			}
		})
	}
}

// stubLoader returns a profile with a recognizable system prompt so
// the test can assert the sub-loop's system was overridden.
func stubLoader(t *testing.T) AgentProfileLoader {
	t.Helper()
	return func(name string) (*AgentProfileSpec, error) {
		switch name {
		case "explore":
			return &AgentProfileSpec{
				Name:         "explore",
				SystemPrompt: "STUB-EXPLORE-PROMPT",
				Tools:        []string{"Read", "Grep"},
			}, nil
		case "verify":
			return &AgentProfileSpec{
				Name:          "verify",
				SystemPrompt:  "STUB-VERIFY-PROMPT",
				InitialPrompt: "Always start by listing recent commits.",
			}, nil
		case "missing":
			return nil, nil // sentinel for "not found"
		}
		return nil, nil
	}
}

func TestAgentTool_SubagentType_ProfileOverridesSystemPrompt(t *testing.T) {
	gate := permission.New(permission.ModeBypass)
	reg := tools.NewRegistry()
	tool := NewAgent(gate, helloProvider(), reg, "model", "PARENT-SYSTEM").
		WithProfileLoader(stubLoader(t))

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":        "do the thing",
		"subagent_type": "explore",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", res.Output)
	}
	// helloProvider doesn't expose what it saw, but the absence of
	// an error after subagent_type lookup is the key observable.
	// The actual system-prompt swap is exercised end-to-end by the
	// tmux smoke test; here we confirm the schema field is honored.
	if !strings.Contains(res.Output, "sub-agent done") {
		t.Errorf("expected hello provider output, got: %q", res.Output)
	}
}

func TestAgentTool_SubagentType_BackCompat_NameMatchesBundledSlug(t *testing.T) {
	// Old behavior: passing name="explore" without subagent_type
	// should still resolve to the explore profile so callers that
	// learned the pre-Q1 docstring don't break.
	gate := permission.New(permission.ModeBypass)
	reg := tools.NewRegistry()
	tool := NewAgent(gate, helloProvider(), reg, "model", "PARENT-SYSTEM").
		WithProfileLoader(stubLoader(t))

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "back-compat path",
		"name":   "explore",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("back-compat (name as bundled slug) should pass; got: %s", res.Output)
	}
}

func TestAgentTool_SubagentType_NameAsLabel_NoProfileLoad(t *testing.T) {
	// name="alice" is not a bundled slug — must NOT trigger profile
	// loading. Confirms the new clean separation: name is identity,
	// subagent_type is role.
	gate := permission.New(permission.ModeBypass)
	reg := tools.NewRegistry()
	loaderCalls := 0
	loader := func(name string) (*AgentProfileSpec, error) {
		loaderCalls++
		return nil, nil
	}
	tool := NewAgent(gate, helloProvider(), reg, "model", "PARENT-SYSTEM").
		WithProfileLoader(loader)

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "regular task",
		"name":   "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("name-only (label) path should pass; got: %s", res.Output)
	}
	if loaderCalls != 0 {
		t.Errorf("loader must NOT be invoked when name is just a label; got %d calls", loaderCalls)
	}
}

func TestAgentTool_SubagentType_ExplicitWinsOverNameBackCompat(t *testing.T) {
	// When BOTH name (matches a bundled slug) and subagent_type are
	// set, subagent_type wins. Loader is invoked exactly once with
	// the subagent_type value, never the name.
	gate := permission.New(permission.ModeBypass)
	reg := tools.NewRegistry()
	var seen []string
	loader := func(name string) (*AgentProfileSpec, error) {
		seen = append(seen, name)
		return &AgentProfileSpec{Name: name, SystemPrompt: "X"}, nil
	}
	tool := NewAgent(gate, helloProvider(), reg, "model", "PARENT-SYSTEM").
		WithProfileLoader(loader)

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":        "explicit wins",
		"name":          "explore", // bundled slug
		"subagent_type": "verify",  // explicit override
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("explicit subagent_type should pass; got: %s", res.Output)
	}
	if len(seen) != 1 || seen[0] != "verify" {
		t.Errorf("loader should have been called exactly once with %q; got: %v", "verify", seen)
	}
}

func TestAgentTool_SubagentType_LoaderNotWired_IsError(t *testing.T) {
	gate := permission.New(permission.ModeBypass)
	reg := tools.NewRegistry()
	// No WithProfileLoader → field is nil; subagent_type request
	// must fail with a clear hint, not silently use parent prompt.
	tool := NewAgent(gate, helloProvider(), reg, "model", "PARENT-SYSTEM")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":        "no loader",
		"subagent_type": "explore",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError when subagent_type set but loader nil")
	}
	if !strings.Contains(res.Output, "subagent_type") || !strings.Contains(res.Output, "wired") {
		t.Errorf("error should explain that loader isn't wired; got: %s", res.Output)
	}
}

func TestAgentTool_SubagentType_UnknownProfile_IsError(t *testing.T) {
	gate := permission.New(permission.ModeBypass)
	reg := tools.NewRegistry()
	tool := NewAgent(gate, helloProvider(), reg, "model", "PARENT-SYSTEM").
		WithProfileLoader(stubLoader(t))

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":        "unknown profile",
		"subagent_type": "missing", // loader returns (nil, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected IsError when profile not found")
	}
	if !strings.Contains(res.Output, "no such profile") {
		t.Errorf("error should say 'no such profile'; got: %s", res.Output)
	}
}

// TestAgentTool_SubagentType_PreservesBaseEnv pins Fix 4 (2026-05-15):
// the profile body must NOT replace the parent's base prompt outright,
// because base carries the <env> block (Working directory, today's
// date) that sub-agents need to construct correct absolute paths.
// The 200-iter long-Wall test exposed this: sub-agents with
// subagent_type=explore lost <env> and started guessing /home/user/code,
// /workspace/metis, etc.
func TestAgentTool_SubagentType_PreservesBaseEnv(t *testing.T) {
	const sentinelBase = "BASE-WITH-ENV-Working directory: /Users/test/proj"
	const sentinelProfile = "PROFILE-EXPLORE-BODY"

	// Drive BuildSubPrompt directly with the same inputs the
	// Agent.Execute path would assemble. Both base (env carrier) and
	// profile body must end up in the result; Fix 4 regression-tests
	// against the earlier behavior where profile silently replaced
	// base.
	got := agent.BuildSubPrompt(agent.SubPromptInputs{
		Mode:                agent.SubPromptAgent,
		Base:                sentinelBase,
		ProfileSystemPrompt: sentinelProfile,
	})
	if !strings.Contains(got, sentinelBase) {
		t.Errorf("subSystem must contain base (env carrier); got:\n%s", got)
	}
	if !strings.Contains(got, sentinelProfile) {
		t.Errorf("subSystem must contain profile body; got:\n%s", got)
	}
}

func TestAgentTool_Schema_HasSubagentTypeField(t *testing.T) {
	tool := Agent{}
	schema := tool.InputSchema()
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["subagent_type"]; !ok {
		t.Fatal("schema must expose a `subagent_type` property after Q1")
	}
	if _, ok := props["name"]; !ok {
		t.Fatal("name property must still exist")
	}
	// subagent_type must NOT be in `required` — it's optional.
	if req, ok := schema["required"].([]string); ok {
		for _, r := range req {
			if r == "subagent_type" {
				t.Error("subagent_type must be optional, not required")
			}
		}
	}
}
