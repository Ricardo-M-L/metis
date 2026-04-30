package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
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
	gate := permission.New(permission.ModeAuto)
	// Register builtin set so the listing has real content. We feed in
	// a zero-value config — builtin defaults are tolerant of unset fields.
	builtin.Register(reg, &config.Config{}, gate)
	loop := agent.NewLoop(nil, reg, gate, nil, "sys", 5)
	got := renderToolsList(loop)
	if !strings.Contains(got, "tools registered") {
		t.Errorf("tools listing missing header line: %s", got)
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
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAuto), nil, "sys", 5)
	loop.AppendUser("u1")
	loop.AppendUser("u2")
	got := renderCurrentSession(store, id, loop, "claude-opus-4-7", "auto")
	if !strings.Contains(got, "session-current") {
		t.Errorf("session id missing: %s", got)
	}
	if !strings.Contains(got, "turns: 2") {
		t.Errorf("turn count missing: %s", got)
	}
	if !strings.Contains(got, "auto") {
		t.Errorf("mode missing: %s", got)
	}
}

func TestRenderSkillsList_NoUserDirStillShowsBundled(t *testing.T) {
	// After the multi-source loader landed, the bundled layer always
	// contributes ~20+ skills regardless of skillDir. Empty skillDir is
	// no longer a "no skills" state — it just means "no user-overridden
	// skills". The listing should still render bundled.
	got := renderSkillsList("")
	if !strings.Contains(got, "skills available") {
		t.Errorf("empty user-dir should still list bundled skills; got %q", got)
	}
	if !strings.Contains(got, "code-review") {
		t.Errorf("bundled code-review should appear; got %q", got)
	}
}

func TestRenderSkillsList_UserDirAddedToBundled(t *testing.T) {
	dir := t.TempDir()
	// Empty user dir: just bundled.
	got := renderSkillsList(dir)
	if !strings.Contains(got, "skills available") {
		t.Errorf("expected bundled skills; got %q", got)
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
