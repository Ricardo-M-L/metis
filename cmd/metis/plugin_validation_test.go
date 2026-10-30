package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginValidationCLIRejectsUnsupportedInfoAndMarksInvalidList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("METIS_NO_BUNDLED_PLUGINS", "1")
	root := filepath.Join(home, "plugins", "invalid-fixture")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin.toml"), []byte("manifest_version = 1\nname = \"invalid-fixture\"\nversion = \"0.1.0\"\nschedules = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureUpdateOutput(t, func() error { return cmdPluginList(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "invalid") || !strings.Contains(output, "schedules") {
		t.Fatalf("invalid capability not explained: %s", output)
	}
	output, err = captureUpdateOutput(t, func() error { return cmdPluginInfo(context.Background(), "invalid-fixture") })
	if err == nil || !strings.Contains(err.Error(), "schedules") || output != "" {
		t.Fatalf("invalid info = %q, %v", output, err)
	}
}

func TestPluginValidationCLIValidSkillInfoAndList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("METIS_NO_BUNDLED_PLUGINS", "1")
	root := filepath.Join(home, "plugins", "valid-fixture")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin.toml"), []byte("manifest_version = 1\nname = \"valid-fixture\"\nversion = \"0.1.0\"\nskills = [\"SKILL.md\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("Read the project."), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, run := range []func() error{
		func() error { return cmdPluginList(context.Background()) },
		func() error { return cmdPluginInfo(context.Background(), "valid-fixture") },
	} {
		output, err := captureUpdateOutput(t, run)
		if err != nil || !strings.Contains(output, "valid-fixture") || strings.Contains(output, "invalid manifest") {
			t.Fatalf("valid plugin = %q, %v", output, err)
		}
	}
}

func TestPluginValidationCLIInfoHidesCredentialValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	root := filepath.Join(home, "plugins", "credential-fixture")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `name = "credential-fixture"
[mcp_server]
command = "not-started"
args = ["synthetic-private-argument"]
env = { API_KEY = "synthetic-private-env" }
[[mcp_servers]]
name = "remote"
url = "https://user:synthetic-private-url@example.invalid/mcp?token=synthetic-private-query"
headers = { Authorization = "synthetic-private-header" }
`
	if err := os.WriteFile(filepath.Join(root, "plugin.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureUpdateOutput(t, func() error { return cmdPluginInfo(context.Background(), "credential-fixture") })
	if err != nil || strings.Contains(output, "synthetic-private-") || !strings.Contains(output, "API_KEY") || !strings.Contains(output, "values hidden") {
		t.Fatalf("plugin info exposes credential fields or loses useful keys: %q, %v", output, err)
	}
}
