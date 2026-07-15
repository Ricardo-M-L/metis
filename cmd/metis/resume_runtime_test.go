package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
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
