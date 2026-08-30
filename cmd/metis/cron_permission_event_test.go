package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type cronPermissionStream struct {
	events []llm.StreamEvent
	index  int
}

func (s *cronPermissionStream) Recv() (llm.StreamEvent, error) {
	if s.index >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*cronPermissionStream) Close() error { return nil }

type cronPermissionProvider struct {
	inputJSON string
	toolName  string
	calls     int
}

func (*cronPermissionProvider) Name() string          { return "cron-permission" }
func (*cronPermissionProvider) ModelID() string       { return "cron-permission-model" }
func (*cronPermissionProvider) MaxContextTokens() int { return 100_000 }
func (*cronPermissionProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("cron permission provider only supports Stream")
}
func (p *cronPermissionProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls++
	if p.calls == 1 {
		return &cronPermissionStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "cron-policy-1", ToolName: p.toolName},
			{Type: "tool_input_delta", ToolUseID: "cron-policy-1", InputDelta: p.inputJSON},
			{Type: "tool_use_stop", ToolUseID: "cron-policy-1"},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	return &cronPermissionStream{events: []llm.StreamEvent{
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}, nil
}

type cronPermissionProbeTool struct {
	tools.BaseTool
	name       string
	permission tools.Permission
	readOnly   bool
	seen       chan map[string]any
}

func (t *cronPermissionProbeTool) Name() string      { return t.name }
func (*cronPermissionProbeTool) Description() string { return "cron permission policy test tool" }
func (*cronPermissionProbeTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (*cronPermissionProbeTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (t *cronPermissionProbeTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return t.permission, "probe permission"
}
func (t *cronPermissionProbeTool) IsReadOnly(map[string]any) bool { return t.readOnly }
func (t *cronPermissionProbeTool) Execute(_ context.Context, input map[string]any) (*tools.Result, error) {
	t.seen <- input
	return &tools.Result{Output: "ok"}, nil
}

func newCronPermissionRuntime(
	t *testing.T,
	toolName string,
	input map[string]any,
	decision tools.Permission,
	readOnly bool,
	sessionDir string,
) (*runtime, *cronPermissionProbeTool) {
	t.Helper()
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	provider := &cronPermissionProvider{inputJSON: string(raw), toolName: toolName}
	probe := &cronPermissionProbeTool{
		name: toolName, permission: decision, readOnly: readOnly,
		seen: make(chan map[string]any, 1),
	}
	registry := tools.NewRegistry()
	registry.Register(probe)
	gate := permission.New(permission.ModeDefault)
	loop := agent.NewLoop(provider, registry, gate, agent.NewHookRegistry(), "system", 4)
	loop.Model = provider.ModelID()
	return &runtime{
		cfg:       &config.Config{Session: config.Session{Dir: sessionDir}},
		registry:  registry,
		gate:      gate,
		loop:      loop,
		sessionID: "cron-permission-session",
	}, probe
}

func TestExecuteCronJobUsesPolicyInputButPersistsOnlyPresentationInput(t *testing.T) {
	// executeCronJob emits a user notification when a denial is recorded.
	// Force the test channel off so this integration test cannot touch Desktop.
	t.Setenv("METIS_NOTIFY_CHANNEL", "off")

	t.Run("exact allow matches raw credential-bearing input", func(t *testing.T) {
		const secret = "deploy-token-for-cron"
		const toolName = "mcp__cron_test__mutate"
		sessionDir := t.TempDir()
		rt, probe := newCronPermissionRuntime(t, toolName, map[string]any{
			"command": `API_KEY='` + secret + `' deploy production`,
		}, tools.PermissionAsk, false, sessionDir)
		job := &agent.CronJob{
			ID: "raw-allow", Name: "raw allow", Prompt: "deploy", Silent: true,
			AllowTools: []string{toolName + "(" + secret + ")"},
		}

		if err := executeCronJob(context.Background(), rt, job, map[string][]llm.Message{}, map[string][]llm.Message{}); err != nil {
			t.Fatalf("executeCronJob: %v", err)
		}
		select {
		case input := <-probe.seen:
			if got := input["command"]; got != `API_KEY='`+secret+`' deploy production` {
				t.Fatalf("executed input = %q, want exact raw command", got)
			}
		default:
			t.Fatal("raw scoped allow did not execute tool")
		}
		denials, err := agent.ListCronDenials(filepath.Join(sessionDir, "cron"), job.ID)
		if err != nil || len(denials) != 0 {
			t.Fatalf("allowed call denials = %#v, %v", denials, err)
		}
		assertCronAuditHasNoSecret(t, sessionDir, job.ID, secret)
	})

	t.Run("CanUse allow MCP executes only with scoped cron rule", func(t *testing.T) {
		const toolName = "mcp__cron_test__mutate"
		sessionDir := t.TempDir()
		rt, probe := newCronPermissionRuntime(t, toolName, map[string]any{
			"operation": "approved-mutation",
		}, tools.PermissionAllow, false, sessionDir)
		job := &agent.CronJob{
			ID: "mcp-scoped-allow", Name: "mcp scoped allow", Prompt: "mutate", Silent: true,
			AllowTools: []string{toolName + "(approved-mutation)"},
		}

		if err := executeCronJob(context.Background(), rt, job, map[string][]llm.Message{}, map[string][]llm.Message{}); err != nil {
			t.Fatalf("executeCronJob: %v", err)
		}
		select {
		case input := <-probe.seen:
			if input["operation"] != "approved-mutation" {
				t.Fatalf("executed input = %#v", input)
			}
		default:
			t.Fatal("scoped MCP allow did not execute")
		}
		denials, err := agent.ListCronDenials(filepath.Join(sessionDir, "cron"), job.ID)
		if err != nil || len(denials) != 0 {
			t.Fatalf("scoped MCP allow denials = %#v, %v", denials, err)
		}
	})

	t.Run("dangerous bytes hidden by presentation redaction still deny", func(t *testing.T) {
		const secret = "cron-secret-sentinel"
		const toolName = "mcp__cron_test__mutate"
		sessionDir := t.TempDir()
		rt, probe := newCronPermissionRuntime(t, toolName, map[string]any{
			"command": `PASSWORD='` + secret + ` rm -rf /' remote-exec`,
		}, tools.PermissionAllow, false, sessionDir)
		job := &agent.CronJob{
			ID: "dangerous-deny", Name: "dangerous deny", Prompt: "run", Silent: true,
			AllowTools: []string{"*"},
		}

		if err := executeCronJob(context.Background(), rt, job, map[string][]llm.Message{}, map[string][]llm.Message{}); err != nil {
			t.Fatalf("executeCronJob: %v", err)
		}
		select {
		case input := <-probe.seen:
			t.Fatalf("dangerous tool executed with input %#v", input)
		default:
		}
		cronRoot := filepath.Join(sessionDir, "cron")
		denials, err := agent.ListCronDenials(cronRoot, job.ID)
		if err != nil || len(denials) != 1 {
			t.Fatalf("denials = %#v, %v; want one", denials, err)
		}
		if !strings.HasPrefix(denials[0].Reason, "dangerous_pattern:") {
			t.Fatalf("denial reason = %q, want dangerous_pattern", denials[0].Reason)
		}
		if strings.Contains(denials[0].Input, secret) || !strings.Contains(denials[0].Input, "[REDACTED]") {
			t.Fatalf("persisted denial input = %q; want redacted without secret", denials[0].Input)
		}
		denialBytes, err := os.ReadFile(filepath.Join(cronRoot, "denied", job.ID+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(denialBytes), secret) {
			t.Fatalf("denial file leaked secret: %s", denialBytes)
		}
		assertCronAuditHasNoSecret(t, sessionDir, job.ID, secret)
	})

	t.Run("CanUse allow MCP is blocked without cron allow rule", func(t *testing.T) {
		const toolName = "mcp__cron_test__mutate"
		sessionDir := t.TempDir()
		rt, probe := newCronPermissionRuntime(t, toolName, map[string]any{
			"operation": "mutate",
		}, tools.PermissionAllow, false, sessionDir)
		job := &agent.CronJob{ID: "empty-allow", Name: "empty allow", Prompt: "mutate", Silent: true}

		if err := executeCronJob(context.Background(), rt, job, map[string][]llm.Message{}, map[string][]llm.Message{}); err != nil {
			t.Fatalf("executeCronJob: %v", err)
		}
		select {
		case input := <-probe.seen:
			t.Fatalf("unlisted MCP tool executed with input %#v", input)
		default:
		}
		denials, err := agent.ListCronDenials(filepath.Join(sessionDir, "cron"), job.ID)
		if err != nil || len(denials) != 1 || denials[0].Reason != "unauthorized" {
			t.Fatalf("denials = %#v, %v; want one unauthorized denial", denials, err)
		}
	})

	t.Run("credential field is absent from denial and audit", func(t *testing.T) {
		const secret = "hunter2"
		const toolName = "mcp__cron_test__mutate"
		sessionDir := t.TempDir()
		rt, probe := newCronPermissionRuntime(t, toolName, map[string]any{
			"operation": "mutate",
			"password":  secret,
		}, tools.PermissionAllow, false, sessionDir)
		job := &agent.CronJob{ID: "password-deny", Name: "password deny", Prompt: "mutate", Silent: true}

		if err := executeCronJob(context.Background(), rt, job, map[string][]llm.Message{}, map[string][]llm.Message{}); err != nil {
			t.Fatalf("executeCronJob: %v", err)
		}
		select {
		case input := <-probe.seen:
			t.Fatalf("credential-bearing unlisted MCP tool executed with input %#v", input)
		default:
		}
		cronRoot := filepath.Join(sessionDir, "cron")
		denials, err := agent.ListCronDenials(cronRoot, job.ID)
		if err != nil || len(denials) != 1 {
			t.Fatalf("denials = %#v, %v; want one", denials, err)
		}
		if strings.Contains(denials[0].Input, secret) || !strings.Contains(denials[0].Input, "[REDACTED]") {
			t.Fatalf("denial input = %q, want redacted credential", denials[0].Input)
		}
		denialBytes, err := os.ReadFile(filepath.Join(cronRoot, "denied", job.ID+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(denialBytes), secret) {
			t.Fatalf("denial file leaked secret: %s", denialBytes)
		}
		assertCronAuditHasNoSecret(t, sessionDir, job.ID, secret)
	})

	t.Run("declared read-only tool keeps existing automatic semantics", func(t *testing.T) {
		const toolName = "ReadOnlyCronLookup"
		sessionDir := t.TempDir()
		rt, probe := newCronPermissionRuntime(t, toolName, map[string]any{
			"query": "status",
		}, tools.PermissionAllow, true, sessionDir)
		job := &agent.CronJob{ID: "read-only", Name: "read only", Prompt: "lookup", Silent: true}

		if err := executeCronJob(context.Background(), rt, job, map[string][]llm.Message{}, map[string][]llm.Message{}); err != nil {
			t.Fatalf("executeCronJob: %v", err)
		}
		select {
		case <-probe.seen:
		default:
			t.Fatal("read-only tool did not execute")
		}
		denials, err := agent.ListCronDenials(filepath.Join(sessionDir, "cron"), job.ID)
		if err != nil || len(denials) != 0 {
			t.Fatalf("read-only denials = %#v, %v; want none", denials, err)
		}
	})
}

func TestEvaluateCronPermissionEventFailsClosedWithoutPrivatePolicyInput(t *testing.T) {
	allow, reason := evaluateCronPermissionEvent(&agent.CronJob{AllowTools: []string{"*"}}, agent.Event{
		Kind:            agent.EventPermissionRequest,
		PermissionTool:  "RemoteExec",
		PermissionInput: map[string]any{"command": "harmless presentation"},
	})
	if allow || reason != "missing_policy_input" {
		t.Fatalf("unbound event = allow %v, reason %q; want fail-closed", allow, reason)
	}
}

func assertCronAuditHasNoSecret(t *testing.T, sessionDir, jobID, secret string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(sessionDir, "cron", "audit", jobID, "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("audit paths = %v, %v; want one", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("cron audit leaked secret: %s", raw)
	}
}
