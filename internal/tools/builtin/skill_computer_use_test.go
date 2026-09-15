package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	skillsloader "github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestSkillComputerUseLoadsOnDemand(t *testing.T) {
	// No local skill directories or native helper: exercise the actual Skill
	// tool's discovery and instruction-loading path independently of CU input.
	loader := skillsloader.NewLoader("", "", nil)
	sk, err := loader.Get("computer-use")
	if err != nil || sk == nil || sk.Prompt == "" {
		t.Fatalf("missing bundled skill: %v", err)
	}
	tool := NewSkill(permission.New(permission.ModeBypass), loader, "")
	ctx := context.Background()
	listed, err := tool.Execute(ctx, map[string]any{"action": "list"})
	if err != nil || listed == nil || listed.IsError {
		t.Fatalf("list failed: %v, %+v", err, listed)
	}
	if !strings.Contains(listed.Output, "**computer-use**: "+sk.Description) || strings.Contains(listed.Output, sk.Prompt) {
		t.Fatal("list must expose the skill summary without its instruction body")
	}
	got, err := tool.Execute(ctx, map[string]any{"action": "get", "name": sk.Name})
	if err != nil || got == nil || got.IsError {
		t.Fatalf("get failed: %v, %+v", err, got)
	}
	var decoded skillsloader.Skill
	if err := json.Unmarshal([]byte(got.Output), &decoded); err != nil || decoded.Prompt != sk.Prompt {
		t.Fatalf("get did not return the bundled instruction body: %v", err)
	}
	invoked, err := tool.Execute(ctx, map[string]any{"action": "invoke", "name": sk.Name})
	if err != nil || invoked == nil || invoked.IsError {
		t.Fatalf("invoke failed: %v, %+v", err, invoked)
	}
	if !strings.HasPrefix(invoked.Output, "## Skill: computer-use\n\n") || !strings.Contains(invoked.Output, sk.Prompt) {
		t.Fatal("invoke must return the instruction body as tool output")
	}
}
