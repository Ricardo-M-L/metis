package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// Exercise the CLI's real first-run writer: its template table is not a user
// adding a Desktop connection. A saved connection without a key still is.
func TestDesktopProviderMembershipIgnoresStarterTemplate(t *testing.T) {
	isolateResumeRuntimeTest(t)
	t.Chdir(t.TempDir())
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(key, "")
	}
	if err := writeStarterConfig(); err != nil {
		t.Fatal(err)
	}
	initial, err := config.ReadProviderSettingsForWorkspace(false)
	if err != nil {
		t.Fatal(err)
	}
	var openAI config.ProviderSettingsProvider
	for _, provider := range initial.Providers {
		if provider.Registered || provider.Removable {
			t.Fatalf("starter template registered a Desktop connection: %s", provider.ID)
		}
		if provider.ID == "openai" {
			openAI = provider
		}
	}
	if openAI.ID == "" {
		t.Fatal("internal protocol defaults missing")
	}
	saved, err := config.SaveProviderSettings(config.ProviderSettingsMutation{
		Action: "upsert", ExpectedRevision: initial.Revision, Provider: &openAI,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, provider := range saved.Providers {
		if provider.ID == "openai" {
			found = true
			if !provider.Registered || !provider.Removable || provider.HasCredential {
				t.Fatal("explicitly saved keyless connection lost its membership")
			}
		}
	}
	if !found {
		t.Fatal("explicitly saved keyless connection missing")
	}
	if _, err := config.SaveProviderSettings(config.ProviderSettingsMutation{
		Action: "delete", ID: "openai", ExpectedRevision: saved.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.ReadProviderSettingsForWorkspace(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range reloaded.Providers {
		if provider.Registered {
			t.Fatalf("removed connection or template reappeared: %s", provider.ID)
		}
	}
}
