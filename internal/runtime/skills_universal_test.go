package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentskills "github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	toolregistry "github.com/Ricardo-M-L/metis/internal/tools"
)

type staticPluginSkills struct{ entries []agentskills.Skill }

func (staticPluginSkills) Name() string                  { return "test-plugin" }
func (s staticPluginSkills) Skills() []agentskills.Skill { return s.entries }

func TestUniversalSkillsDirHonorsMetisHomeIsolation(t *testing.T) {
	metisHome := t.TempDir()
	t.Setenv("METIS_HOME", metisHome)
	t.Setenv("HOME", t.TempDir())

	want := filepath.Join(metisHome, ".agents", "skills")
	if got := universalSkillsDir(); got != want {
		t.Fatalf("universalSkillsDir = %q, want isolated %q", got, want)
	}
}

func TestRegisterSkillToolUsesIsolatedUniversalCatalog(t *testing.T) {
	metisHome := t.TempDir()
	hostHome := t.TempDir()
	t.Setenv("METIS_HOME", metisHome)
	t.Setenv("HOME", hostHome)

	write := func(root, name string) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := "---\nname: " + name + "\ndescription: test\n---\nbody"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(metisHome, ".agents", "skills"), "isolated-universal")
	write(filepath.Join(hostHome, ".agents", "skills"), "must-not-leak")

	cfg := &config.Config{Session: config.Session{SkillDir: filepath.Join(metisHome, "skills")}}
	reg := toolregistry.NewRegistry()
	loader := RegisterSkillTool(reg, ToolRegistryOptions{
		Cfg:  cfg,
		Gate: permission.New(permission.ModeBypassPermissions),
	})
	all, err := loader.List()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sk := range all {
		seen[sk.Name] = true
	}
	if !seen["isolated-universal"] {
		t.Fatal("METIS_HOME universal skill missing")
	}
	if seen["must-not-leak"] {
		t.Fatal("isolated runtime scanned HOME/.agents instead of METIS_HOME")
	}
}

func TestRegisterSkillToolKeepsLoaderPointerWhenPluginsArrive(t *testing.T) {
	metisHome := t.TempDir()
	t.Setenv("METIS_HOME", metisHome)
	cfg := &config.Config{Session: config.Session{SkillDir: filepath.Join(metisHome, "skills")}}
	reg := toolregistry.NewRegistry()
	gate := permission.New(permission.ModeBypassPermissions)

	before := RegisterSkillTool(reg, ToolRegistryOptions{Cfg: cfg, Gate: gate})
	after := RegisterSkillTool(reg, ToolRegistryOptions{
		Cfg:  cfg,
		Gate: gate,
		PluginSources: []agentskills.PluginSkillSource{staticPluginSkills{entries: []agentskills.Skill{{
			Name: "late-plugin-skill", Description: "loaded during phase two", Prompt: "body",
		}}}},
	})
	if before != after {
		t.Fatal("phase-two plugin registration replaced the loader pointer")
	}
	got, err := before.Get("late-plugin-skill")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("shared loader did not expose the late plugin skill")
	}
}

func TestHasInstalledSkillsRecognizesUniversalRoot(t *testing.T) {
	metisHome := t.TempDir()
	t.Setenv("METIS_HOME", metisHome)
	dir := filepath.Join(metisHome, ".agents", "skills", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: demo\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasInstalledSkills() {
		t.Fatal("universal Agent Skill was not detected")
	}
}

func TestSkillsPromptUsesLiveToolWithoutMandatoryListRoundTrip(t *testing.T) {
	for name, body := range map[string]string{
		"section":    readSection("06_skills.md"),
		"monolithic": basePromptTPL,
	} {
		if !strings.Contains(body, "cross-agent `~/.agents/skills`") || !strings.Contains(body, "do not call `list` first") {
			t.Errorf("%s skill prompt missing live-catalog/install guidance", name)
		}
		if strings.Contains(body, "injected") || strings.Contains(body, "<available_skills>") {
			t.Errorf("%s skill prompt claims a catalog attachment the runtime does not inject", name)
		}
		if strings.Contains(body, "Before answering anything non-trivial, call the `Skill` tool") {
			t.Errorf("%s skill prompt still mandates a list tool round trip", name)
		}
	}
}
