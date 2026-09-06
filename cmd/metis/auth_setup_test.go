package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func TestModelAfterProviderSelection(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		before   string
		after    string
		explicit bool
		custom   bool
		want     string
	}{
		{name: "same provider keeps implicit model", model: "stored-model", before: "anthropic", after: "anthropic", want: "stored-model"},
		{name: "changed provider clears resumed model", model: "claude-resumed", before: "anthropic", after: "sensenova", want: ""},
		{name: "changed provider preserves explicit model", model: "cli-model", before: "anthropic", after: "sensenova", explicit: true, want: "cli-model"},
		{name: "changed provider clears profile-derived model", model: "profile-model", before: "anthropic", after: "sensenova", want: ""},
		{name: "same custom provider uses model reconfigured by wizard", model: "old-resumed-model", before: "sensenova", after: "sensenova", custom: true, want: ""},
		{name: "explicit model wins over same custom provider wizard", model: "cli-model", before: "sensenova", after: "sensenova", explicit: true, custom: true, want: "cli-model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelAfterProviderSelection(tt.model, tt.before, tt.after, tt.explicit, tt.custom); got != tt.want {
				t.Fatalf("modelAfterProviderSelection() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRefreshConfigSnapshotAfterAuthOnlyReloadsWizardChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())

	cfg, _, snap, err := config.LoadWithSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("[provider]\ndefault = \"openai\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if snap.Diff().Empty() {
		t.Fatal("test setup did not make the original snapshot stale")
	}

	gotCfg, gotSnap, err := refreshConfigSnapshotAfterAuth(cfg, snap, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotCfg != cfg || gotSnap != snap {
		t.Fatal("ordinary auth path absorbed an unrelated config edit")
	}

	gotCfg, gotSnap, err = refreshConfigSnapshotAfterAuth(cfg, snap, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotCfg.Provider.Default != "openai" {
		t.Fatalf("refreshed provider = %q, want openai", gotCfg.Provider.Default)
	}
	if !gotSnap.Diff().Empty() {
		t.Fatalf("wizard-auth refresh returned a stale baseline: %#v", gotSnap.Diff())
	}
}

func TestRefreshConfigSnapshotAfterAuthReappliesProviderTrust(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[provider]\ndefault = \"openai\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte("[provider]\ndefault = \"gemini\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, snap, err := config.LoadWithSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	untrusted, _, err := refreshConfigSnapshotAfterAuth(cfg, snap, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if untrusted.Provider.Default != "openai" {
		t.Fatalf("untrusted refresh provider = %q, want openai", untrusted.Provider.Default)
	}
	trusted, _, err := refreshConfigSnapshotAfterAuth(cfg, snap, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if trusted.Provider.Default != "gemini" {
		t.Fatalf("trusted refresh provider = %q, want gemini", trusted.Provider.Default)
	}
}
