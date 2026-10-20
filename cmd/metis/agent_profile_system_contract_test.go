package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestSetupRuntimeAgentProfileSystemContract(t *testing.T) {
	const profileBody = "PROFILE-SYSTEM-CONTRACT-7dc9e1e5\nAct only on the supplied event. Read its declared input and return the requested result."
	const overrideBody = "CLI-SYSTEM-OVERRIDE-7dc9e1e5\nFollow this explicit command-line system contract."
	const globalAddendum = "GLOBAL-SYSTEM-ADDENDUM-7dc9e1e5"
	const defaultWorkspaceInstruction = "Inspect the relevant code, instructions, configuration, and workspace state"
	cases := []struct {
		name           string
		body           string
		omitClaudeMd   bool
		systemOverride string
		wantBody       string
		wantKind       string
	}{
		{name: "profile-body-replaces-default", body: profileBody, wantBody: profileBody, wantKind: session.SystemPromptKindCustom},
		{name: "frontmatter-only-keeps-default", wantKind: session.SystemPromptKindDefault},
		{name: "explicit-system-wins-profile", body: profileBody, systemOverride: overrideBody, wantBody: overrideBody, wantKind: session.SystemPromptKindCustom},
		{name: "omit-claude-md-preserves-profile", body: profileBody, omitClaudeMd: true, wantBody: profileBody, wantKind: session.SystemPromptKindCustom},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			workspace, home := t.TempDir(), t.TempDir()
			t.Chdir(workspace)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("METIS_HOME", home)
			t.Setenv("ANTHROPIC_API_KEY", "test-agent-profile-system-contract-key")
			t.Setenv("METIS_AUTO_MEMORY", "0")
			t.Setenv("METIS_SIMPLE", "")
			t.Setenv("METIS_COORDINATOR_MODE", "")
			agents := filepath.Join(workspace, ".metis", "agents")
			if err := os.MkdirAll(agents, 0700); err != nil {
				t.Fatal(err)
			}
			profile := "---\nname: scoped-contract\ntools: [Read]\n"
			if test.omitClaudeMd {
				profile += "omit_claude_md: true\n"
			}
			profile += "---\n" + test.body + "\n"
			if err := os.WriteFile(filepath.Join(agents, "scoped-contract.md"), []byte(profile), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "system.md"), []byte(globalAddendum), 0600); err != nil {
				t.Fatal(err)
			}
			flags := &cliFlags{bare: true, noAuthWizard: true, agentProfile: "scoped-contract"}
			if test.systemOverride != "" {
				flags.system, flags.systemSet = test.systemOverride, true
			}
			rt, err := setupRuntime(context.Background(), flags)
			if err != nil {
				t.Fatalf("actual setupRuntime with workspace profile: %v", err)
			}
			defer rt.Cleanup()
			if _, ok := rt.registry.GetForModel("Read"); !ok {
				t.Fatal("actual profile lost its allowed Read tool")
			}
			if _, ok := rt.registry.GetForModel("Bash"); ok {
				t.Fatal("actual profile did not restrict tools to its workspace allowlist")
			}
			header, _, err := rt.store.LoadHeader(rt.sessionID)
			if err != nil {
				t.Fatalf("load actual freshly persisted session header: %v", err)
			}
			if rt.systemPromptKind != test.wantKind || header.SystemPromptKind != test.wantKind {
				t.Fatalf("prompt provenance differs: runtime=%q header=%q want=%q", rt.systemPromptKind, header.SystemPromptKind, test.wantKind)
			}
			if header.System != rt.loop.System {
				t.Fatal("actual session header system differs from the model loop system")
			}
			if test.wantBody != "" {
				if !strings.Contains(rt.loop.System, test.wantBody) || !strings.Contains(header.System, test.wantBody) {
					t.Fatalf("profile or explicit system was marked custom but overwritten during section assembly: runtime_contains_body=%v header_contains_body=%v runtime_kind=%q header_kind=%q", strings.Contains(rt.loop.System, test.wantBody), strings.Contains(header.System, test.wantBody), rt.systemPromptKind, header.SystemPromptKind)
				}
				if strings.Count(rt.loop.System, test.wantBody) != 1 || strings.Count(header.System, test.wantBody) != 1 {
					t.Fatal("opaque custom system contract must appear exactly once in runtime and persisted header")
				}
				if strings.Contains(rt.loop.System, defaultWorkspaceInstruction) {
					t.Fatal("opaque custom profile inherited default workspace-scanning instructions")
				}
				sectionHasBody := false
				for _, section := range rt.loop.SystemSections {
					if strings.Contains(section.Body, defaultWorkspaceInstruction) {
						t.Fatalf("custom profile section %q retained default workspace-scanning instructions", section.Name)
					}
					sectionHasBody = sectionHasBody || strings.Contains(section.Body, test.wantBody)
				}
				if !test.omitClaudeMd && (len(rt.loop.SystemSections) == 0 || !sectionHasBody) {
					t.Fatal("provider section form dropped the opaque custom profile body")
				}
			} else if !strings.Contains(rt.loop.System, defaultWorkspaceInstruction) {
				t.Fatal("frontmatter-only profile unexpectedly replaced the default prompt")
			}
			if test.systemOverride != "" && strings.Contains(rt.loop.System, profileBody) {
				t.Fatal("profile body overrode the explicit --system contract")
			}
			if test.omitClaudeMd && strings.Contains(rt.loop.System, globalAddendum) {
				t.Fatal("omit_claude_md profile unexpectedly loaded the global system addendum")
			}
		})
	}
}
