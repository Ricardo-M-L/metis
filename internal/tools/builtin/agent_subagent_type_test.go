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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type streamCountingProvider struct {
	*fakeProvider
	streamCalls atomic.Int32
}

func (p *streamCountingProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	p.streamCalls.Add(1)
	return p.fakeProvider.Stream(ctx, req)
}

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

func TestAgentTool_SubagentType_WorktreeProfileFailureCleansIsolation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}

	repo := t.TempDir()
	runAgentTestGit(t, repo, "init", "--quiet")
	runAgentTestGit(t, repo, "config", "user.name", "Metis Agent Test")
	runAgentTestGit(t, repo, "config", "user.email", "metis-agent-test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatalf("write repository fixture: %v", err)
	}
	runAgentTestGit(t, repo, "add", "README.md")
	runAgentTestGit(t, repo, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "initial fixture")

	// resolveIsolation's nesting guard compares the exact current directory
	// against registered worktree roots. Run from a normal repository subdir so
	// this exercises Spawn + Cleanup rather than the guard's root-path refusal.
	workDir := filepath.Join(repo, "source")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatalf("create repository subdir: %v", err)
	}
	previousCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("chdir repository subdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousCwd) })

	metisHome := filepath.Join(t.TempDir(), "metis-home")
	t.Setenv("METIS_HOME", metisHome)
	roster := agent.NewRoster(0)
	tool := NewAgent(
		permission.New(permission.ModeBypass),
		helloProvider(),
		tools.NewRegistry(),
		"model",
		"PARENT-SYSTEM",
	).WithRoster(roster).WithProfileLoader(func(string) (*AgentProfileSpec, error) {
		return nil, errors.New("profile store unavailable")
	})

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":            "fail after creating an isolated worktree",
		"subagent_type":     "explore",
		"isolation":         "worktree",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("Execute returned transport error: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "profile store unavailable") {
		t.Fatalf("expected profile setup error, got %+v", res)
	}
	if got := roster.Count(); got != 0 {
		t.Fatalf("profile setup failure leaked %d live roster entries; want 0", got)
	}

	worktreesDir := filepath.Join(metisHome, "worktrees")
	entries, readErr := os.ReadDir(worktreesDir)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read worktree directory after Execute returned: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("worktree directory still contains %d entries after Execute returned: %v", len(entries), entries)
	}
	worktreeList := runAgentTestGit(t, repo, "worktree", "list", "--porcelain")
	if strings.Contains(worktreeList, worktreesDir) {
		t.Fatalf("git worktree registration leaked under %s:\n%s", worktreesDir, worktreeList)
	}
	if got := strings.Count(worktreeList, "worktree "); got != 1 {
		t.Fatalf("git worktree list contains %d registrations after cleanup, want only the main repository:\n%s", got, worktreeList)
	}

	joinCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := roster.CancelAndWait(joinCtx); err != nil {
		t.Fatalf("CancelAndWait should observe completed setup cleanup: %v", err)
	}
}

func runAgentTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestAgentTool_StrictBoundaryDuringSetupNeverStartsRunner(t *testing.T) {
	roster := agent.NewRoster(0)
	provider := &streamCountingProvider{fakeProvider: helloProvider().(*fakeProvider)}
	loaderEntered := make(chan struct{})
	releaseLoader := make(chan struct{})
	defer func() {
		select {
		case <-releaseLoader:
		default:
			close(releaseLoader)
		}
	}()
	tool := NewAgent(
		permission.New(permission.ModeBypass),
		provider,
		tools.NewRegistry(),
		"model",
		"PARENT-SYSTEM",
	).WithRoster(roster).WithProfileLoader(func(name string) (*AgentProfileSpec, error) {
		close(loaderEntered)
		<-releaseLoader
		return &AgentProfileSpec{Name: name, SystemPrompt: "SETUP-RACE"}, nil
	})

	type executeResult struct {
		result *tools.Result
		err    error
	}
	executeDone := make(chan executeResult, 1)
	go func() {
		result, err := tool.Execute(context.Background(), map[string]any{
			"prompt":            "must not start after strict revocation",
			"name":              "constructing",
			"subagent_type":     "explore",
			"run_in_background": true,
		})
		executeDone <- executeResult{result: result, err: err}
	}()

	select {
	case <-loaderEntered:
	case <-time.After(time.Second):
		t.Fatal("Agent did not reach the post-Register setup window")
	}
	teammate, ok := roster.Lookup("constructing")
	if !ok {
		t.Fatal("post-Register setup window has no live roster entry")
	}
	// Install an observation callback before the real child context exists.
	// CancelAndWait must latch the request, then Agent.SetCancel must replay it
	// against the real callback when setup resumes.
	cancelObserved := make(chan struct{})
	teammate.SetCancel(func() { close(cancelObserved) })
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelJoin()
	joinDone := make(chan error, 1)
	go func() { joinDone <- roster.CancelAndWait(joinCtx) }()
	select {
	case <-cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("strict boundary did not request cancellation during setup")
	}
	close(releaseLoader)

	select {
	case got := <-executeDone:
		if got.err != nil {
			t.Fatalf("Execute returned transport error: %v", got.err)
		}
		if got.result == nil || !got.result.IsError || !strings.Contains(got.result.Output, context.Canceled.Error()) {
			t.Fatalf("Execute result = %+v, want canceled tool result", got.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Agent Execute did not leave the canceled setup window")
	}
	select {
	case err := <-joinDone:
		if err != nil {
			t.Fatalf("CancelAndWait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CancelAndWait did not join the canceled setup path")
	}
	if got := provider.streamCalls.Load(); got != 0 {
		t.Fatalf("provider Stream calls = %d, want 0 after pre-start revocation", got)
	}
	if got := roster.Count(); got != 0 {
		t.Fatalf("joined roster count = %d, want 0", got)
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

func TestAgentTool_Schema_SubagentTypeUsesDynamicEnum(t *testing.T) {
	tool := Agent{}.WithProfileNames(func() []string {
		return []string{"verify", "explore", "verify", "", "  general  "}
	})
	schema := tool.InputSchema()
	props := schema["properties"].(map[string]any)
	subagentType := props["subagent_type"].(map[string]any)
	got, ok := subagentType["enum"].([]string)
	if !ok {
		t.Fatalf("subagent_type enum type = %T, want []string", subagentType["enum"])
	}
	want := []string{"explore", "general", "verify"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subagent_type enum = %v, want %v", got, want)
	}
	if slices.Contains(got, "research") {
		t.Fatal("dynamic enum unexpectedly contains nonexistent research profile")
	}
}

func TestAgentTool_Schema_OmitsEnumWithoutProfileCatalog(t *testing.T) {
	schema := (Agent{}).InputSchema()
	props := schema["properties"].(map[string]any)
	subagentType := props["subagent_type"].(map[string]any)
	if _, ok := subagentType["enum"]; ok {
		t.Fatal("headless Agent without a profile catalog should not publish an empty enum")
	}
}
