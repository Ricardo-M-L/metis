package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	mainPromptBudgetBytes     = 12_000
	subAgentPromptBudgetBytes = 5_000
	computerUseBudgetBytes    = 1_000
)

func TestStablePromptGoldenSnapshots(t *testing.T) {
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "prompts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	cases := []struct {
		name     string
		ctx      PromptCtx
		mode     PromptMode
		overlays []SystemPromptSection
	}{
		{
			name: "main-standard",
			ctx: PromptCtx{
				EnabledTools: map[string]bool{"Bash": true},
				HasSkills:    true,
			},
			mode: PromptFull,
		},
		{
			name: "main-computer-use",
			ctx: PromptCtx{
				EnabledTools:         map[string]bool{"Bash": true},
				HasSkills:            true,
				ComputerUseAvailable: true,
			},
			mode: PromptFull,
		},
		{
			name: "main-no-skills",
			ctx: PromptCtx{
				EnabledTools: map[string]bool{"Bash": true},
				HasSkills:    false,
			},
			mode: PromptFull,
		},
		{
			name: "subagent-minimal",
			ctx: PromptCtx{
				EnabledTools: map[string]bool{"Read": true, "Grep": true},
				HasSkills:    true,
				IsSubAgent:   true,
			},
			mode: PromptMinimal,
		},
		{
			name: "plan-mode",
			ctx: PromptCtx{
				EnabledTools: map[string]bool{"Read": true, "Grep": true},
				HasSkills:    true,
			},
			mode:     PromptFull,
			overlays: []SystemPromptSection{PlanOverlay(true)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sections := AssembleSystemPromptSectionsCtx(tc.ctx, AssembleOptions{
				Mode:     tc.mode,
				SkipEnv:  true,
				Overlays: tc.overlays,
			})
			got := RenderSections(sections)
			assertPromptGolden(t, filepath.Join(goldenDir, tc.name+".expected.md"), got)
		})
	}
}

func TestPromptSizeBudgets(t *testing.T) {
	main := AssembleBaseString(PromptCtx{
		EnabledTools: map[string]bool{"Bash": true},
		HasSkills:    true,
	})
	if len(main) > mainPromptBudgetBytes {
		t.Fatalf("main prompt is %d bytes; budget is %d", len(main), mainPromptBudgetBytes)
	}

	subAgent := AssembleBaseString(PromptCtx{
		EnabledTools: map[string]bool{"Read": true, "Grep": true},
		IsSubAgent:   true,
	})
	if len(subAgent) > subAgentPromptBudgetBytes {
		t.Fatalf("sub-agent prompt is %d bytes; budget is %d", len(subAgent), subAgentPromptBudgetBytes)
	}

	withComputerUse := AssembleBaseString(PromptCtx{
		EnabledTools:         map[string]bool{"Bash": true},
		HasSkills:            true,
		ComputerUseAvailable: true,
	})
	delta := len(withComputerUse) - len(main)
	if delta > computerUseBudgetBytes {
		t.Fatalf("computer-use adds %d bytes; budget is %d", delta, computerUseBudgetBytes)
	}
}

func assertPromptGolden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("UPDATE_PROMPT_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_PROMPT_GOLDEN=1 to create it)", path, err)
	}
	if got != string(want) {
		t.Fatalf("prompt drifted from %s; review the diff and regenerate intentionally with UPDATE_PROMPT_GOLDEN=1", path)
	}
}
