package builtin

// skill_template_test.go — end-to-end coverage for the SKILL.md template-
// var + inline-shell expansion path borrowed from claude-code. Pins:
//   - ${METIS_SKILL_DIR} resolves to the skill's own directory
//   - ${CLAUDE_SKILL_DIR} alias works (paste-compat with claude-code)
//   - ${METIS_SESSION_ID} resolves when WithSessionIDFn is wired
//   - !`cmd` inline-shell runs only when trust ≥ user
//   - community-trust skill does NOT execute inline shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	skillsloader "github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func writeSkill(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name+".md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSkillInvoke_RecordsUsage guards the signal the skill curator relies
// on: invoking a user-layer skill records a use event (count + timestamp)
// in the usage store so an actively-used skill stays "active" and isn't
// archived — without mutating the file on disk (the old mtime-bump hack).
func TestSkillInvoke_RecordsUsage(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "hot", `---
name: hot
description: frequently used
---
body
`)
	loader := skillsloader.NewLoader(dir, "", nil)
	tool := NewSkill(permission.New(permission.ModeBypass), loader, dir)

	res, err := tool.Execute(context.Background(), map[string]any{"action": "invoke", "name": "hot"})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("invoke failed: err=%v res=%+v", err, res)
	}
	rec, ok := skillsloader.NewUsageStore(dir).Get("hot")
	if !ok {
		t.Fatal("invoke should have created a usage record")
	}
	if rec.UseCount != 1 || rec.LastUsedAt == "" {
		t.Errorf("want UseCount=1 + LastUsedAt set, got %+v", rec)
	}

	// A `get` records a VIEW, kept distinct from a use.
	if _, err := tool.Execute(context.Background(), map[string]any{"action": "get", "name": "hot"}); err != nil {
		t.Fatal(err)
	}
	rec, _ = skillsloader.NewUsageStore(dir).Get("hot")
	if rec.ViewCount != 1 || rec.UseCount != 1 {
		t.Errorf("view should increment ViewCount only; got UseCount=%d ViewCount=%d", rec.UseCount, rec.ViewCount)
	}
}

// TestSkillInvoke_UsageKeyedByFilename guards the fix for the curator/usage
// key mismatch: usage must be recorded under the on-disk FILENAME base, not
// the frontmatter `name:` (which the curator can't see when it scans by
// filename). Here the file is on-disk-name.md but frontmatter name differs.
func TestSkillInvoke_UsageKeyedByFilename(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "on-disk-name", `---
name: friendly-display-name
description: x
---
body
`)
	loader := skillsloader.NewLoader(dir, "", nil)
	tool := NewSkill(permission.New(permission.ModeBypass), loader, dir)
	// Invoke by the loader-resolved (frontmatter) name.
	if _, err := tool.Execute(context.Background(), map[string]any{"action": "invoke", "name": "friendly-display-name"}); err != nil {
		t.Fatal(err)
	}
	store := skillsloader.NewUsageStore(dir)
	if _, ok := store.Get("on-disk-name"); !ok {
		t.Error("usage must be keyed by the on-disk filename base")
	}
	if _, ok := store.Get("friendly-display-name"); ok {
		t.Error("usage must NOT be keyed by the frontmatter name (curator can't see it)")
	}
}

func TestSkill_PlanAllowsInspectionButRejectsInvoke(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "must-not-exist")
	writeSkill(t, dir, "plan-safe", `---
name: plan-safe
description: plan boundary regression
---
result=!`+"`"+`touch `+marker+"`"+`
`)
	gate := permission.New(permission.ModePlan)
	tool := NewSkill(gate, skillsloader.NewLoader(dir, "", nil), dir)

	for _, action := range []string{"list", "get"} {
		in := map[string]any{"action": action, "name": "plan-safe"}
		got, source := tool.CanUse(context.Background(), in)
		if got != tools.PermissionAllow {
			t.Fatalf("Skill %s in Plan = %v (%s), want allow", action, got, source)
		}
	}

	invoke := map[string]any{"action": "invoke", "name": "plan-safe"}
	got, source := tool.CanUse(context.Background(), invoke)
	if got != tools.PermissionDeny {
		t.Fatalf("Skill invoke in Plan = %v (%s), want deny", got, source)
	}
	res, err := tool.Execute(context.Background(), invoke)
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("direct Execute must retain Plan guard: err=%v res=%+v", err, res)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Plan invoke executed inline shell; marker stat err=%v", err)
	}
}

func TestSkillGet_PlanDoesNotWriteUsageTelemetry(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "inspect-only", `---
name: inspect-only
description: read-only lookup
---
body
`)
	gate := permission.New(permission.ModePlan)
	tool := NewSkill(gate, skillsloader.NewLoader(dir, "", nil), dir)

	res, err := tool.Execute(context.Background(), map[string]any{"action": "get", "name": "inspect-only"})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("Plan get failed: err=%v res=%+v", err, res)
	}
	if _, ok := skillsloader.NewUsageStore(dir).Get("inspect-only"); ok {
		t.Fatal("Plan get must not write a usage/view record")
	}
}

func TestSkillCapability_IsInputAware(t *testing.T) {
	var tool Skill
	if !tool.IsReadOnly(map[string]any{"action": "get"}) {
		t.Fatal("Skill get should be read-only")
	}
	if tool.IsReadOnly(map[string]any{"action": "invoke"}) {
		t.Fatal("Skill invoke must not be read-only")
	}
	if tool.Concurrency(map[string]any{"action": "invoke"}) != tools.ConcurrencyExclusive {
		t.Fatal("Skill invoke must serialize with state-changing tools")
	}
}

func TestSkillInvoke_TemplateAndShellExpansion_UserTrust(t *testing.T) {
	dir := t.TempDir()
	// `user` trust → inline shell allowed.
	writeSkill(t, dir, "dirinfo", `---
name: dirinfo
description: smoke skill
---

dir=${METIS_SKILL_DIR}
session=${METIS_SESSION_ID}
date=!`+"`"+`date -u +%Y`+"`"+`
`)
	loader := skillsloader.NewLoader(dir, "", nil)
	gate := permission.New(permission.ModeBypass)
	tool := NewSkill(gate, loader, dir).WithSessionIDFn(func() string { return "session-abc" })

	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "invoke", "name": "dirinfo",
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("invoke failed: err=%v res=%+v", err, res)
	}
	out := res.Output
	if !strings.Contains(out, "dir="+dir) {
		t.Errorf("METIS_SKILL_DIR not substituted; got:\n%s", out)
	}
	if !strings.Contains(out, "session=session-abc") {
		t.Errorf("METIS_SESSION_ID not substituted; got:\n%s", out)
	}
	if !strings.Contains(out, "date=20") {
		t.Errorf("inline shell didn't run (expected `date -u +%%Y` output starting with 20XX); got:\n%s", out)
	}
	// Variable placeholders must be gone.
	if strings.Contains(out, "${METIS_SKILL_DIR}") || strings.Contains(out, "${METIS_SESSION_ID}") {
		t.Errorf("placeholders leaked through; got:\n%s", out)
	}
}

func TestSkillInvoke_ClaudeAliasesWork(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "claude-alias", `---
name: claude-alias
description: paste from claude-code
---

dir=${CLAUDE_SKILL_DIR}
session=${CLAUDE_SESSION_ID}
`)
	loader := skillsloader.NewLoader(dir, "", nil)
	gate := permission.New(permission.ModeBypass)
	tool := NewSkill(gate, loader, dir).WithSessionIDFn(func() string { return "s-1" })

	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "invoke", "name": "claude-alias",
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("invoke failed: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Output, "dir="+dir) {
		t.Errorf("CLAUDE_SKILL_DIR alias not substituted; got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "session=s-1") {
		t.Errorf("CLAUDE_SESSION_ID alias not substituted; got:\n%s", res.Output)
	}
}

func TestSkillInvoke_CommunityTrustBlocksInlineShell(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "untrusted", `---
name: untrusted
description: simulated 3rd-party skill
trust_level: community
---

result=!`+"`"+`whoami`+"`"+`
`)
	// Lie about the dir's nature: pass it as a "plugin" layer (which
	// stamps community trust). Easiest is to construct a Loader and
	// force trust via a custom layer instead — bypass NewLoader so we
	// can pin the community posture.
	loader := &skillsloader.Loader{
		Layers: []skillsloader.Layer{
			{
				Name: "fake-community", Priority: 99, Trust: "community",
				Scan: func() ([]skillsloader.Skill, error) {
					sk, err := skillsloader.Load(filepath.Join(dir, "untrusted.md"))
					if err != nil {
						return nil, err
					}
					return []skillsloader.Skill{*sk}, nil
				},
			},
		},
		Logger: func(string, ...any) {},
		Cwd:    "",
	}
	loader.Invalidate()

	gate := permission.New(permission.ModeBypass)
	tool := NewSkill(gate, loader, dir)
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "invoke", "name": "untrusted",
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("invoke failed: err=%v res=%+v", err, res)
	}
	// Inline shell must NOT have run — the literal !`whoami` token
	// should still be in the output.
	if !strings.Contains(res.Output, "!`whoami`") {
		t.Errorf("community-trust skill SHOULD NOT execute inline shell; got:\n%s", res.Output)
	}
}
