package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func writeRuntimeResumeSession(t *testing.T, id string) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.WriteHeaderFull(session.Header{
		ID:       id,
		Provider: "openai",
		Model:    "stored-model",
		System:   "stored system prompt",
		Mode:     "ask",
	}); err != nil {
		t.Fatalf("WriteHeaderFull: %v", err)
	}
	if err := store.AppendMessage(id, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "stored turn"}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
}

func isolateResumeRuntimeTest(t *testing.T) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key")
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	t.Setenv("METIS_SIMPLE", "")
	t.Setenv("METIS_COORDINATOR_MODE", "")
}

func TestSetupRuntime_ResumeRestoresHeaderProviderModelAndSystem(t *testing.T) {
	isolateResumeRuntimeTest(t)
	const id = "resume-runtime-header"
	writeRuntimeResumeSession(t, id)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := setupRuntime(ctx, &cliFlags{
		resumeID: id, bare: true, noAuthWizard: true,
	})
	if err != nil {
		t.Fatalf("setupRuntime: %v", err)
	}
	defer rt.Cleanup()

	if rt.providerName != "openai" || rt.provider.Name() != "openai" {
		t.Fatalf("provider = %q/%q, want restored openai", rt.providerName, rt.provider.Name())
	}
	if rt.model != "stored-model" || rt.loop.Model != "stored-model" {
		t.Fatalf("model = %q/%q, want stored-model", rt.model, rt.loop.Model)
	}
	if rt.loop.System != "stored system prompt" {
		t.Fatalf("system = %q, want exact stored prompt", rt.loop.System)
	}
	if len(rt.loop.SystemSections) != 0 {
		t.Fatalf("restored final system should not be assembled a second time: %+v", rt.loop.SystemSections)
	}
	if len(rt.loop.Messages) != 1 || rt.loop.Messages[0].Content[0].Text != "stored turn" {
		t.Fatalf("messages not restored: %+v", rt.loop.Messages)
	}
}

func TestSetupRuntime_ResumeManagedDefaultRebindsFinalToolRegistry(t *testing.T) {
	isolateResumeRuntimeTest(t)
	const id = "resume-managed-default-tools"
	store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{
		ID: id, Provider: "openai", Model: "stored-model",
		System:           "stale managed prompt with WebSearch routing",
		SystemPromptKind: session.SystemPromptKindDefault,
	}); err != nil {
		t.Fatal(err)
	}

	rt, err := setupRuntime(context.Background(), &cliFlags{
		resumeID: id, bare: true, noAuthWizard: true, tools: "Read",
	})
	if err != nil {
		t.Fatalf("setupRuntime: %v", err)
	}
	defer rt.Cleanup()
	if strings.Contains(rt.loop.System, "stale managed prompt") || strings.Contains(rt.loop.System, "WebSearch") {
		t.Fatalf("managed default was not rebuilt from final Read-only registry:\n%s", rt.loop.System)
	}
	if len(rt.loop.SystemSections) == 0 {
		t.Fatal("managed default resume lost typed prompt sections")
	}
}

func TestSetupRuntime_ResumeModeDoesNotChangeFreshSessionBaseline(t *testing.T) {
	isolateResumeRuntimeTest(t)
	const id = "resume-mode-fresh-baseline"
	store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{
		ID: id, Provider: "openai", Model: "stored-model", Mode: string(permission.ModeAcceptEdits),
	}); err != nil {
		t.Fatal(err)
	}

	rt, err := setupRuntime(context.Background(), &cliFlags{resumeID: id, bare: true, noAuthWizard: true})
	if err != nil {
		t.Fatalf("setupRuntime: %v", err)
	}
	defer rt.Cleanup()
	if got := rt.gate.Mode(); got != permission.ModeAcceptEdits {
		t.Fatalf("restored active mode = %q, want acceptEdits", got)
	}
	if got := rt.defaultPermissionMode; got != permission.ModeDefault {
		t.Fatalf("fresh-session baseline = %q, want invocation default", got)
	}
}

func TestSetupRuntime_ExplicitProviderModelAndSystemWinOnResume(t *testing.T) {
	isolateResumeRuntimeTest(t)
	const id = "resume-runtime-cli-overrides"
	writeRuntimeResumeSession(t, id)

	flags, _, err := parseFlags([]string{
		"--resume", id, "--bare", "--no-auth-wizard",
		"--provider", "anthropic", "--model", "cli-model", "--system", "cli system prompt",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := setupRuntime(ctx, flags)
	if err != nil {
		t.Fatalf("setupRuntime: %v", err)
	}
	defer rt.Cleanup()

	if rt.providerName != "anthropic" || rt.provider.Name() != "anthropic" {
		t.Fatalf("provider = %q/%q, want explicit anthropic", rt.providerName, rt.provider.Name())
	}
	if rt.model != "cli-model" || rt.loop.Model != "cli-model" {
		t.Fatalf("model = %q/%q, want cli-model", rt.model, rt.loop.Model)
	}
	if !strings.Contains(rt.loop.System, "cli system prompt") || strings.Contains(rt.loop.System, "stored system prompt") {
		t.Fatalf("explicit system did not win: %q", rt.loop.System)
	}
}

func TestSetupRuntime_ExplicitProviderUsesItsDefaultModelOnResume(t *testing.T) {
	isolateResumeRuntimeTest(t)
	const id = "resume-runtime-provider-default-model"
	writeRuntimeResumeSession(t, id)

	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	wantModel := cfg.Provider.Anthropic.Model
	flags, _, err := parseFlags([]string{
		"--resume", id, "--bare", "--no-auth-wizard", "--provider", "anthropic",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := setupRuntime(ctx, flags)
	if err != nil {
		t.Fatalf("setupRuntime: %v", err)
	}
	defer rt.Cleanup()

	if rt.providerName != "anthropic" || rt.provider.Name() != "anthropic" {
		t.Fatalf("provider = %q/%q, want anthropic", rt.providerName, rt.provider.Name())
	}
	if rt.model != wantModel || rt.loop.Model != wantModel {
		t.Fatalf("model = %q/%q, want anthropic default %q (not resumed %q)", rt.model, rt.loop.Model, wantModel, "stored-model")
	}
}

func TestParseFlags_TracksExplicitResumeOverrides(t *testing.T) {
	flags, _, err := parseFlags([]string{"--provider", "openai", "-m", "gpt-test", "--system", "prompt"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !flags.providerSet || !flags.modelSet || !flags.systemSet {
		t.Fatalf("explicit markers not recorded: %+v", flags)
	}
}

func TestSetupRuntime_ResumeBypassNeverStartsAuthWizard(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "resume-bypass-no-auth-ui"
	if err := store.WriteHeaderFull(session.Header{
		ID: id, Provider: "openai", Model: "stored-model", Mode: "bypassPermissions",
	}); err != nil {
		t.Fatal(err)
	}

	oldTTY, oldWizard := authGateIsTTY, authGateRunWizard
	t.Cleanup(func() {
		authGateIsTTY, authGateRunWizard = oldTTY, oldWizard
	})
	authGateIsTTY = func() bool { return true }
	wizardCalls := 0
	authGateRunWizard = func() (*rtpkg.WizardResult, error) {
		wizardCalls++
		return nil, rtpkg.ErrWizardCancelled
	}

	rt, err := setupRuntime(context.Background(), &cliFlags{resumeID: id, bare: true})
	if rt != nil {
		rt.Cleanup()
	}
	if err == nil {
		t.Fatal("missing credential unexpectedly allowed startup")
	}
	if wizardCalls != 0 {
		t.Fatalf("stored bypass session launched auth wizard %d times", wizardCalls)
	}
}
