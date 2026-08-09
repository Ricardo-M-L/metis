package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestRenderToolsList_NilLoopFallsBack(t *testing.T) {
	got := renderToolsList(nil)
	if !strings.Contains(got, "registry") {
		t.Errorf("nil-loop output should mention registry; got %q", got)
	}
}

func TestRenderToolsList_ShowsBuiltins(t *testing.T) {
	reg := tools.NewRegistry()
	gate := permission.New(permission.ModeAcceptEdits)
	// Register builtin set so the listing has real content. We feed in
	// a zero-value config — builtin defaults are tolerant of unset fields.
	builtin.Register(reg, &config.Config{}, gate)
	loop := agent.NewLoop(nil, reg, gate, nil, "sys", 5)
	got := renderToolsList(loop)
	if !strings.Contains(got, "Tools · ") || !strings.Contains(got, "registered") {
		t.Errorf("tools listing missing box header (Tools · N registered): %s", got)
	}
	if !strings.Contains(got, "Read") {
		t.Errorf("Read tool should appear in builtin listing: %s", got)
	}
}

func TestRenderSessionsList_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewStore(dir)
	got := renderSessionsList(store, 5)
	if !strings.Contains(got, "no sessions") {
		t.Errorf("empty store should hint no sessions; got %q", got)
	}
}

func TestRenderSessionsList_RendersTitleWhenSet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	store, _ := session.NewStore(dir)
	id := "session-x"
	_ = store.WriteHeader(id, "claude-opus-4-7", "sys")
	_ = store.SetTitle(id, "refactor sprint")
	got := renderSessionsList(store, 5)
	if !strings.Contains(got, "refactor sprint") {
		t.Errorf("title not surfaced in listing: %s", got)
	}
}

func TestRenderCurrentSession_NoSession(t *testing.T) {
	got := renderCurrentSession(nil, "", nil, "", "")
	if !strings.Contains(got, "no active session") {
		t.Errorf("expected no-active-session hint; got %q", got)
	}
}

func TestRenderCurrentSession_WithLoopShowsTurns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	store, _ := session.NewStore(dir)
	id := "session-current"
	_ = store.WriteHeader(id, "m", "")
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.ContextWindow = 100_000
	loop.Fast = true
	loop.SetEffort("high")
	loop.AppendUser("u1")
	loop.AppendUser("u2")
	// v2: lipgloss styles each label/value separately, so a literal
	// "turns: 2" substring no longer exists — the label "turns:" and
	// the value "2" each get their own ANSI sequence wrapper. Strip
	// styles before substring search to keep the assertion meaningful.
	got := ansi.Strip(renderCurrentSession(store, id, loop, "claude-opus-4-7", "auto"))
	if !strings.Contains(got, "session-current") {
		t.Errorf("session id missing: %s", got)
	}
	if !strings.Contains(got, "turns:") || !strings.Contains(got, "2") {
		t.Errorf("turn count missing: %s", got)
	}
	if !strings.Contains(got, "auto") {
		t.Errorf("mode missing: %s", got)
	}
	for _, field := range []string{"transcript:", "working dir:", "effort:", "high", "fast mode:", "true", "context:", "100,000", "loop iterations:"} {
		if !strings.Contains(got, field) {
			t.Errorf("richer status missing %q: %s", field, got)
		}
	}
}

func TestRenderSkillsList_NoUserDirStillShowsBundled(t *testing.T) {
	// After the multi-source loader landed, the bundled layer always
	// contributes ~20+ skills regardless of skillDir. Empty skillDir is
	// no longer a "no skills" state — it just means "no user-overridden
	// skills". The listing should still render bundled.
	got := renderSkillsList(nil, "")
	if !strings.Contains(got, "Skills · ") {
		t.Errorf("empty user-dir should still list bundled skills; got %q", got)
	}
	if !strings.Contains(got, "code-review") {
		t.Errorf("bundled code-review should appear; got %q", got)
	}
}

func TestRenderSkillsList_UserDirAddedToBundled(t *testing.T) {
	dir := t.TempDir()
	// Empty user dir: just bundled.
	got := renderSkillsList(nil, dir)
	if !strings.Contains(got, "Skills · ") {
		t.Errorf("expected bundled skills; got %q", got)
	}
}

func TestRenderSkillsList_UsesLiveUniversalCatalog(t *testing.T) {
	universal := t.TempDir()
	dir := filepath.Join(universal, "shared-demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "---\nname: shared-demo\ndescription: universal test skill\n---\nbody"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := skills.NewLoaderWithUniversal("", "", "", universal, nil)
	reg := tools.NewRegistry()
	reg.Register(builtin.NewSkill(permission.New(permission.ModeBypassPermissions), loader, ""))
	loop := agent.NewLoop(nil, reg, permission.New(permission.ModeBypassPermissions), nil, "sys", 5)

	got := renderSkillsList(loop, "")
	if !strings.Contains(got, "shared-demo") {
		t.Fatalf("live universal skill missing from /skills output: %s", got)
	}
}

func TestRenderVersion_FormatMatchesCLI(t *testing.T) {
	got := renderVersion()
	if !strings.Contains(got, "(Metis)") {
		t.Errorf("version should include (Metis) suffix; got %q", got)
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abcdefgh-1234"); got != "abcdefgh" {
		t.Errorf("shortID = %q, want abcdefgh", got)
	}
	if got := shortID("xyz"); got != "xyz" {
		t.Errorf("short input should pass through; got %q", got)
	}
}
