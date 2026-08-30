package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func TestConfigSlashOpensBuiltinScreenInsteadOfEditor(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModel(t)
	m.input.SetValue("/config")
	cmd := pressEnter(t, m)
	if cmd != nil {
		t.Fatal("/config should open the built-in screen without launching $EDITOR")
	}
	if _, ok := m.activeScreen.(*screen.ConfigScreen); !ok {
		t.Fatalf("active screen = %T, want *screen.ConfigScreen", m.activeScreen)
	}
	if !strings.Contains(m.activeScreen.View(), "Settings") {
		t.Fatalf("config screen missing settings view:\n%s", m.activeScreen.View())
	}
}

func TestConfigSettingsSnapshotToleratesMissingGate(t *testing.T) {
	m := &Model{cfg: &config.Config{Permission: config.Permission{Mode: "plan"}}}
	settings := m.configSettingsSnapshot()
	if len(settings) == 0 || settings[0].Value != "plan" {
		t.Fatalf("permission snapshot = %+v", settings)
	}
}

func TestConfigSettingsSnapshotOnlyExposesBubbleTUIEffectiveSettings(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := &Model{cfg: &config.Config{}}
	settings := m.configSettingsSnapshot()
	keys := make(map[string]bool, len(settings))
	for _, setting := range settings {
		keys[setting.Key] = true
	}
	for _, unsupported := range []string{"ui.theme", "ui.markdown", "ui.show_tokens", "ui.show_tool_json", "ui.streamlined_output"} {
		if keys[unsupported] {
			t.Errorf("Bubble TUI panel exposes non-effective setting %q", unsupported)
		}
	}
	for _, supported := range []string{"permission.mode", "ui.thinking_display", "session.max_iterations", "ui.performance.reduced_motion"} {
		if !keys[supported] {
			t.Errorf("Bubble TUI panel omitted supported setting %q", supported)
		}
	}
}

func TestConfigSettingsSnapshotLocksEffectiveEnvironmentOverride(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_MOUSE_WHEEL_LINES", "7")
	m := &Model{cfg: &config.Config{UI: config.UI{Performance: config.UIPerformance{MouseWheelLines: 2}}}}
	settings := m.configSettingsSnapshot()
	for _, setting := range settings {
		if setting.Key == "ui.performance.mouse_wheel_lines" {
			if !strings.Contains(setting.LockedReason, "METIS_MOUSE_WHEEL_LINES") {
				t.Fatalf("environment override is editable: %+v", setting)
			}
			return
		}
	}
	t.Fatal("mouse wheel setting missing")
}

func TestConfigScreenEscapeRollsBackMemoryAndDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	original := "[ui]\nmarkdown = true\nthinking_display = \"auto\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newSlashTestModel(t)
	m.cfg = &config.Config{UI: config.UI{Markdown: true, ThinkingDisplay: "auto"}}
	m.openConfigScreen()
	w := m.activeScreen.(*screen.ConfigScreen)
	w.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // stage a permission-mode preview
	w.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m.applyScreenResult(w)
	if !m.cfg.UI.Markdown || m.thinkingDisplay != "" {
		t.Fatalf("cancel mutated in-memory config: %+v thinking=%q", m.cfg.UI, m.thinkingDisplay)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("cancel mutated config file:\n%s", got)
	}
}

func TestConfigScreenApplyPersistsUserOnlyAndUpdatesLiveState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := newSlashTestModel(t)
	m.cfg = &config.Config{UI: config.UI{Markdown: true, ThinkingDisplay: "auto"}}
	m.openConfigScreen()
	w := m.activeScreen.(*screen.ConfigScreen)
	// First setting is permission mode. Move to thinking_display and preview hide.
	for range 1 {
		w.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	w.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.applyScreenResult(w)
	if m.cfg.UI.ThinkingDisplay != "hide" || m.thinkingDisplay != "hide" {
		t.Fatalf("thinking state not applied: cfg=%q live=%q", m.cfg.UI.ThinkingDisplay, m.thinkingDisplay)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `thinking_display = "hide"`) {
		t.Fatalf("user config missing applied value:\n%s", data)
	}
	if strings.Contains(strings.ToLower(string(data)), "api_key") {
		t.Fatalf("config panel unexpectedly persisted a credential field:\n%s", data)
	}
}

func TestConfigScreenApplyFailureKeepsMemoryAndDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	original := "ui = { markdown = true }\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newSlashTestModel(t)
	m.cfg = &config.Config{UI: config.UI{Markdown: true, ThinkingDisplay: "auto"}}
	m.openConfigScreen()
	w := m.activeScreen.(*screen.ConfigScreen)
	for range 1 {
		w.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	w.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.applyScreenResult(w)
	if !m.cfg.UI.Markdown {
		t.Fatal("failed save mutated in-memory setting")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != original {
		t.Fatalf("failed save changed disk: got=%q err=%v", got, err)
	}
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].Role != "error" {
		t.Fatalf("failed save did not surface an error: %+v", m.messages)
	}
}

func TestConfigScreenSaveFailureRestoresPlanLineage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")

	m := newSlashTestModel(t)
	m.cfg = &config.Config{Permission: config.Permission{Mode: string(permission.ModePlan)}}
	m.gate.SetMode(permission.ModePlan)
	m.loop.SetPlanMode(true)
	m.loop.SetPrePlanMode(string(permission.ModeDefault))
	m.openConfigScreen()
	w := m.activeScreen.(*screen.ConfigScreen)
	w.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // plan -> acceptEdits
	w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	// Fail only after the live preview has been staged. A directory at the
	// config-file path makes the transactional read fail portably.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	m.applyScreenResult(w)

	if got := m.gate.Mode(); got != permission.ModePlan || !m.loop.IsPlanMode() {
		t.Fatalf("failed save did not restore plan posture: gate=%q plan=%v", got, m.loop.IsPlanMode())
	}
	if got := m.loop.PrePlanMode(); got != string(permission.ModeDefault) {
		t.Fatalf("failed save rewrote plan lineage to %q", got)
	}
}

func TestConfigScreenRejectedBypassIsNotPersisted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := newSlashTestModel(t)
	m.cfg = &config.Config{Permission: config.Permission{Mode: "default"}}
	m.gate.SetMode(permission.ModeDefault)
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "off", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	m.ext.Sandbox = manager
	m.openConfigScreen()
	w := m.activeScreen.(*screen.ConfigScreen)
	w.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // default wraps to bypassPermissions
	w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.applyScreenResult(w)

	if got := m.gate.Mode(); got != permission.ModeDefault {
		t.Fatalf("rejected bypass changed gate to %s", got)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("rejected bypass persisted config: %v", err)
	}
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].Role != "error" || !strings.Contains(m.messages[len(m.messages)-1].Content, "unchanged") {
		t.Fatalf("rejected bypass did not surface an error: %+v", m.messages)
	}
}

func TestConfigScreenEditorActionReturnsManagedCommand(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModel(t)
	m.openConfigScreen()
	w := m.activeScreen.(*screen.ConfigScreen)
	w.Update(tea.KeyPressMsg{Text: "e", Code: 'e'})
	if cmd := m.applyScreenResult(w); cmd == nil {
		t.Fatal("editor secondary action must return Bubble Tea managed command")
	}
}

func TestConfigScreenProjectOverrideIsReadOnlyAndNotReportedApplied(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte("[ui]\nthinking_display = \"hide\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	m := newSlashTestModel(t)
	m.cfg = &config.Config{UI: config.UI{ThinkingDisplay: "hide"}}
	m.openConfigScreen()
	w := m.activeScreen.(*screen.ConfigScreen)
	view := w.View()
	if !strings.Contains(view, "project config") {
		t.Fatalf("project override is not visible:\n%s", view)
	}
	for range 1 {
		w.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	w.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.applyScreenResult(w)
	if len(m.messages) == 0 || strings.Contains(m.messages[len(m.messages)-1].Content, "applied") {
		t.Fatalf("override was falsely reported applied: %+v", m.messages)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("locked override unexpectedly wrote user config: %v", err)
	}
}

func TestConfigScreenRestartSettingPersistsWithoutMutatingLiveSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := newSlashTestModel(t)
	m.cfg = &config.Config{Session: config.Session{MaxIterations: 100}}
	m.openConfigScreen()
	w := m.activeScreen.(*screen.ConfigScreen)
	for range 5 {
		w.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	w.Update(tea.KeyPressMsg{Text: "i", Code: 'i'})
	for range 3 {
		w.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	for _, r := range "250" {
		w.Update(tea.KeyPressMsg{Text: string(r), Code: r})
	}
	w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.applyScreenResult(w)
	if m.cfg.Session.MaxIterations != 100 {
		t.Fatalf("restart-required value falsely changed running snapshot: %d", m.cfg.Session.MaxIterations)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "max_iterations = 250") {
		t.Fatalf("restart setting was not persisted:\n%s", data)
	}
	if len(m.messages) == 0 || !strings.Contains(m.messages[len(m.messages)-1].Content, "restart") {
		t.Fatalf("restart requirement not surfaced: %+v", m.messages)
	}
}

func TestConfigScreenCandidateReloadFailureDoesNotChangeDiskOrMemory(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(home, "config.toml")
	original := "[ui]\nthinking_display = \"auto\"\n"
	if err := os.WriteFile(userPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newSlashTestModel(t)
	m.cfg = &config.Config{UI: config.UI{ThinkingDisplay: "auto"}}
	m.openConfigScreen()
	w := m.activeScreen.(*screen.ConfigScreen)
	for range 1 {
		w.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	w.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	// Simulate the higher-precedence file becoming invalid after the panel
	// opened but before Apply.
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte("[ui\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.applyScreenResult(w)
	got, readErr := os.ReadFile(userPath)
	if readErr != nil || string(got) != original {
		t.Fatalf("failed candidate validation changed disk: got=%q err=%v", got, readErr)
	}
	if m.cfg.UI.ThinkingDisplay != "auto" || m.thinkingDisplay != "" {
		t.Fatalf("failed candidate validation changed live state: cfg=%q live=%q", m.cfg.UI.ThinkingDisplay, m.thinkingDisplay)
	}
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].Role != "error" {
		t.Fatalf("candidate validation failure not surfaced: %+v", m.messages)
	}
}
