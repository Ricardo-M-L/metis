package tui

// Tests for the canonical /skills management surface. Every test gets an
// isolated skillDir; none reaches for the host's ~/.metis/skills.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func newSkillTestREPL(t *testing.T) (*REPL, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "skills")
	return &REPL{skillDir: dir}, dir
}

func managedManifest(dir, name string) string {
	return filepath.Join(dir, name, canonicalSkillManifest)
}

func loadManagedSkill(t *testing.T, dir, name string) *skills.Skill {
	t.Helper()
	sk, err := skills.Load(managedManifest(dir, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return sk
}

func TestSkills_CreateWritesCanonicalMarkdownAndLoaderSeesIt(t *testing.T) {
	r, dir := newSkillTestREPL(t)
	out := r.handleSkillCreate("debug-helper")
	if !strings.Contains(out, "created skill") {
		t.Fatalf("create should confirm; got: %q", out)
	}

	path := managedManifest(dir, "debug-helper")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("canonical SKILL.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "debug-helper.json")); !os.IsNotExist(err) {
		t.Fatalf("create must not write legacy JSON; stat err=%v", err)
	}
	sk := loadManagedSkill(t, dir, "debug-helper")
	if sk.Name != "debug-helper" || strings.TrimSpace(sk.Prompt) == "" {
		t.Fatalf("created manifest must have canonical name + non-empty body: %+v", sk)
	}
	loader := skills.NewLoader(dir, "", nil)
	loaded, err := loader.Get("debug-helper")
	if err != nil || loaded == nil {
		t.Fatalf("live loader did not see created directory skill: sk=%+v err=%v", loaded, err)
	}

	info := r.handleSkillInfo("debug-helper")
	if !strings.Contains(info, "debug-helper") || !strings.Contains(info, "SKILL.md") || !strings.Contains(info, "enabled") {
		t.Errorf("info should render name, canonical path, and state; got:\n%s", info)
	}
}

func TestSkills_CreateRefusesDuplicateAndUnsafeName(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	r.handleSkillCreate("dup")
	if out := r.handleSkillCreate("dup"); !strings.Contains(out, "already exists") {
		t.Errorf("duplicate create should refuse; got: %q", out)
	}
	if out := r.handleSkillCreate("../escape"); !strings.Contains(out, "invalid name") {
		t.Errorf("path-shaped create name should refuse; got: %q", out)
	}
}

func TestSkills_EnableDisableRoundTripPreservesMarkdown(t *testing.T) {
	r, dir := newSkillTestREPL(t)
	r.handleSkillCreate("toggle-me")
	path := managedManifest(dir, "toggle-me")
	before, _ := os.ReadFile(path)
	beforeBody := strings.SplitN(string(before), "---\n\n", 2)
	if len(beforeBody) != 2 {
		t.Fatalf("unexpected scaffold format:\n%s", before)
	}

	out := r.handleSkillDisable("toggle-me")
	if !strings.Contains(out, "disabled skill") {
		t.Fatalf("disable should confirm; got: %q", out)
	}
	sk := loadManagedSkill(t, dir, "toggle-me")
	if !sk.Disabled || sk.Prompt == "" {
		t.Fatalf("disable must update frontmatter without losing body: %+v", sk)
	}
	loader := skills.NewLoader(dir, "", nil)
	if visible, _ := loader.Get("toggle-me"); visible != nil {
		t.Fatalf("disabled skill should be filtered from live loader: %+v", visible)
	}
	if out = r.handleSkillDisable("toggle-me"); !strings.Contains(out, "already disabled") {
		t.Errorf("re-disable should be idempotent; got: %q", out)
	}

	out = r.handleSkillEnable("toggle-me")
	if !strings.Contains(out, "enabled skill") {
		t.Fatalf("enable should confirm; got: %q", out)
	}
	sk = loadManagedSkill(t, dir, "toggle-me")
	if sk.Disabled || !strings.Contains(sk.Prompt, "concrete procedure") {
		t.Fatalf("enable must remove disabled flag and preserve body: %+v", sk)
	}
	loader = skills.NewLoader(dir, "", nil)
	if visible, _ := loader.Get("toggle-me"); visible == nil {
		t.Fatal("enabled skill should return to live loader")
	}
}

func TestSkills_EnableDisablePreservesUnknownFrontmatter(t *testing.T) {
	r, dir := newSkillTestREPL(t)
	path := managedManifest(dir, "custom-meta")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "---\nname: custom-meta\ndescription: custom\nx-publisher-field: keep-me\n---\n\nDo the work.\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	r.handleSkillDisable("custom-meta")
	r.handleSkillEnable("custom-meta")
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "x-publisher-field: keep-me") || !strings.Contains(string(after), "Do the work.") {
		t.Fatalf("toggle destroyed unknown metadata or body:\n%s", after)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Fatalf("toggle changed file mode: %o", st.Mode().Perm())
	}
}

func TestSkills_ToggleRepairsCanonicalManifestWithoutName(t *testing.T) {
	r, dir := newSkillTestREPL(t)
	path := managedManifest(dir, "nameless")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: no explicit name\n---\n\nRun these steps.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := r.handleSkillDisable("nameless"); !strings.Contains(out, "disabled skill") {
		t.Fatalf("toggle should locate canonical directory even before name repair: %s", out)
	}
	sk := loadManagedSkill(t, dir, "nameless")
	if sk.Name != "nameless" || !sk.Disabled {
		t.Fatalf("toggle should add directory name to frontmatter: %+v", sk)
	}
}

func TestSkills_EnableUnknown(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	if out := r.handleSkillEnable("ghost-skill"); !strings.Contains(out, "no locally managed skill") {
		t.Errorf("expected local not-found result; got: %q", out)
	}
}

func TestSkills_RemoveDeletesCanonicalDirectoryAndAssets(t *testing.T) {
	r, dir := newSkillTestREPL(t)
	r.handleSkillCreate("remove-me")
	asset := filepath.Join(dir, "remove-me", "example.txt")
	if err := os.WriteFile(asset, []byte("asset"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := r.handleSkillUninstall("remove-me")
	if !strings.Contains(out, "removed skill") {
		t.Fatalf("remove should confirm; got: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "remove-me")); !os.IsNotExist(err) {
		t.Fatalf("skill directory/assets survived remove; err=%v", err)
	}
}

func TestSkills_EditOpensCanonicalManifest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test editor fixture uses a POSIX script")
	}
	r, dir := newSkillTestREPL(t)
	r.handleSkillCreate("edit-me")
	root := t.TempDir()
	logPath := filepath.Join(root, "editor-path")
	editor := filepath.Join(root, "editor")
	script := "#!/bin/sh\nprintf '%s' \"$1\" > \"$METIS_TEST_EDITOR_LOG\"\n"
	if err := os.WriteFile(editor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", editor)
	t.Setenv("EDITOR", "")
	t.Setenv("METIS_TEST_EDITOR_LOG", logPath)
	out := r.handleSkillEdit("edit-me")
	if !strings.Contains(out, "saved") || !strings.Contains(out, "SKILL.md") {
		t.Fatalf("edit should save canonical manifest; got: %q", out)
	}
	opened, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(opened), managedManifest(dir, "edit-me"); got != want {
		t.Fatalf("editor opened %q, want %q", got, want)
	}
}

func TestSkills_SearchLocalAndGuardedDiscovery(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	r.handleSkillCreate("alpha-helper")
	r.handleSkillCreate("beta-helper")
	r.handleSkillCreate("gamma-thing")

	out := r.handleSkillSearchLocal("helper")
	if !strings.Contains(out, "alpha-helper") || !strings.Contains(out, "beta-helper") || strings.Contains(out, "gamma-thing") {
		t.Fatalf("unexpected local search results:\n%s", out)
	}

	out = r.handleSkillSearchLocal("unresolved-skill-name")
	if !strings.Contains(out, "No local skills match") || !strings.Contains(out, "npx skills find") {
		t.Fatalf("miss should give one bounded registry discovery:\n%s", out)
	}
	if strings.Contains(out, "api.github.com") || strings.Contains(out, "raw.githubusercontent.com") {
		t.Fatalf("search must not route to legacy arbitrary GitHub JSON search:\n%s", out)
	}
}

func TestSkills_InstallUsesExistingGuardedPlannerWithoutWriting(t *testing.T) {
	r, dir := newSkillTestREPL(t)
	out := r.handleSkillInstall("anti-ui-slop")
	if !strings.Contains(out, "No files were downloaded") ||
		!strings.Contains(out, "npx skills add https://uizze.com --skill anti-ui-slop") {
		t.Fatalf("install should reuse official lifecycle planner:\n%s", out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("guarded install planner unexpectedly wrote files: %+v", entries)
	}
}

func TestSkills_DispatcherUnknownAndInfoRoute(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	if out := cmdSkills(r, "frobnicate foo"); !strings.Contains(out, "unknown") {
		t.Errorf("expected unknown subcommand error; got: %q", out)
	}
	r.handleSkillCreate("router-test")
	if out := cmdSkills(r, "info router-test"); !strings.Contains(out, "router-test") || !strings.Contains(out, "SKILL.md") {
		t.Errorf("expected dispatcher to route to modern info handler; got:\n%s", out)
	}
}

func runTUISkillsCommand(t *testing.T, dir, command string) *Model {
	t.Helper()
	m := newSlashTestModel(t)
	m.skillDir = dir
	m.input.SetValue(command)
	pressEnter(t, m)
	return m
}

// This drives the real Bubble Tea submit path, not cmdSkills directly. It
// protects against the slash registry shadowing the canonical REPL handler.
func TestSkills_CanonicalTUIRouteUsesDirectoryManifests(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	m := runTUISkillsCommand(t, dir, "/skills create tui-route")
	if _, ok := m.activeScreen.(*screen.BodyScreen); !ok {
		t.Fatalf("explicit /skills create should render its result; got %T", m.activeScreen)
	}
	if _, err := os.Stat(managedManifest(dir, "tui-route")); err != nil {
		t.Fatalf("real TUI route did not create canonical manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tui-route.json")); !os.IsNotExist(err) {
		t.Fatalf("real TUI route wrote legacy JSON; err=%v", err)
	}

	m = runTUISkillsCommand(t, dir, "/skills list")
	if !strings.Contains(m.activeScreen.View(), "tui-route") {
		t.Fatalf("/skills list did not see canonical manifest:\n%s", m.activeScreen.View())
	}
	m = runTUISkillsCommand(t, dir, "/skills info tui-route")
	if !strings.Contains(m.activeScreen.View(), "SKILL.md") {
		t.Fatalf("/skills info did not route to canonical manifest:\n%s", m.activeScreen.View())
	}
	if runtime.GOOS != "windows" {
		editor := filepath.Join(t.TempDir(), "editor")
		if err := os.WriteFile(editor, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VISUAL", editor)
		m = runTUISkillsCommand(t, dir, "/skills edit tui-route")
		if !strings.Contains(m.activeScreen.View(), "SKILL.md") {
			t.Fatalf("/skills edit did not open canonical manifest:\n%s", m.activeScreen.View())
		}
	}
	runTUISkillsCommand(t, dir, "/skills disable tui-route")
	if !loadManagedSkill(t, dir, "tui-route").Disabled {
		t.Fatal("/skills disable through real TUI route did not persist")
	}
	runTUISkillsCommand(t, dir, "/skills enable tui-route")
	if loadManagedSkill(t, dir, "tui-route").Disabled {
		t.Fatal("/skills enable through real TUI route did not persist")
	}

	// Bare /skills remains the interactive picker rather than becoming a list
	// or management action as the subcommands are modernized.
	m = runTUISkillsCommand(t, dir, "/skills")
	if _, ok := m.activeScreen.(*screen.PickerScreen); !ok {
		t.Fatalf("bare /skills should remain PickerScreen; got %T", m.activeScreen)
	}
	m = runTUISkillsCommand(t, dir, "/skills search tui-route")
	if !strings.Contains(m.activeScreen.View(), "local match") {
		t.Fatalf("/skills search did not route to local catalog:\n%s", m.activeScreen.View())
	}
	m = runTUISkillsCommand(t, dir, "/skills install anti-ui-slop")
	if !strings.Contains(m.activeScreen.View(), "No files were downloaded") {
		t.Fatalf("/skills install did not route to guarded planner:\n%s", m.activeScreen.View())
	}

	runTUISkillsCommand(t, dir, "/skills remove tui-route")
	if _, err := os.Stat(filepath.Join(dir, "tui-route")); !os.IsNotExist(err) {
		t.Fatalf("/skills remove through real TUI route left directory; err=%v", err)
	}
}
