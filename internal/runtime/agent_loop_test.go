package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// stubProvider is a minimal Provider for BuildAgentLoop tests.
// Compactor only calls MaxContextTokens(); we never run the loop.
type stubProvider struct {
	mu     sync.Mutex
	maxCtx int
}

func (p *stubProvider) Name() string          { return "stub" }
func (p *stubProvider) MaxContextTokens() int { p.mu.Lock(); defer p.mu.Unlock(); return p.maxCtx }
func (p *stubProvider) ModelID() string       { return "" }
func (p *stubProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return &llm.Response{}, nil
}
func (p *stubProvider) Stream(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
	return nopStream{}, nil
}

type nopStream struct{}

func (nopStream) Close() error                   { return nil }
func (nopStream) Recv() (llm.StreamEvent, error) { return llm.StreamEvent{}, io.EOF }

func defaultLoopCfg(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Session.Dir = t.TempDir()
	cfg.Session.MaxIterations = 100
	cfg.Session.AutoCompactThreshold = 0.85
	cfg.LoopDetection.Enabled = true
	cfg.LoopDetection.Warning = 5
	cfg.LoopDetection.Critical = 10
	cfg.LoopDetection.Global = 30
	return cfg
}

func TestBuildAgentLoop_AppliesAllSubsystems(t *testing.T) {
	cfg := defaultLoopCfg(t)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 200_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		System:   "you are a tester",
		Model:    "claude-test",
		MaxIter:  50,
	})
	if loop == nil {
		t.Fatal("BuildAgentLoop returned nil")
	}
	if loop.Memory == nil {
		t.Error("Memory should be wired when memory dir is creatable")
	}
	if loop.Compactor == nil {
		t.Error("Compactor should always be wired")
	}
	if loop.Detector == nil {
		t.Error("Detector should be wired when LoopDetection.Enabled=true")
	}
	if loop.MaxIters != 50 {
		t.Errorf("MaxIters = %d, want 50 (caller override)", loop.MaxIters)
	}
}

func TestBuildAgentLoop_ExplicitTypedNilMemoryDoesNotFallback(t *testing.T) {
	cfg := defaultLoopCfg(t)
	var concrete *memory.MemoryManager
	var repository memory.Repository = concrete
	if repository == nil {
		t.Fatal("test setup must preserve a typed nil inside the repository interface")
	}

	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider:              &stubProvider{maxCtx: 100_000},
		Registry:              tools.NewRegistry(),
		Gate:                  permission.New(permission.ModeAcceptEdits),
		MemoryManager:         repository,
		MemoryManagerProvided: true,
	})

	if loop.Memory != nil {
		t.Fatalf("explicit typed-nil repository must disable memory without fallback; got %#v", loop.Memory)
	}
	if _, err := os.Stat(filepath.Join(config.Home(), "memory")); !os.IsNotExist(err) {
		t.Fatalf("explicit unavailable repository unexpectedly created fallback memory root: %v", err)
	}
}

func TestBuildAgentLoop_FallsBackToCfgMaxIter(t *testing.T) {
	cfg := defaultLoopCfg(t)
	cfg.Session.MaxIterations = 77
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 100_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
	})
	if loop.MaxIters != 77 {
		t.Errorf("MaxIters = %d, want 77 (cfg fallback)", loop.MaxIters)
	}
}

// TestBuildAgentLoop_DetectorOffWhenDisabled — post-2026-05-08 the
// detector is on by default; the explicit kill switch is now
// `LoopDetection.Disabled = true`. The legacy `Enabled = false` is a
// no-op for backward compat (older configs that never opted in stay
// safe rather than silently losing the safety net).
func TestBuildAgentLoop_DetectorOffWhenDisabled(t *testing.T) {
	cfg := defaultLoopCfg(t)
	cfg.LoopDetection.Disabled = true
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 100_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	if loop.Detector != nil {
		t.Error("Detector should remain nil when LoopDetection.Disabled=true")
	}
}

// TestBuildAgentLoop_DetectorOnByDefault — the safety net runs without
// any opt-in. Pin this so an accidental config refactor doesn't quietly
// turn the detector back into opt-in (which is what burned the user
// in the 2026-05-08 1h 18m hang).
func TestBuildAgentLoop_DetectorOnByDefault(t *testing.T) {
	cfg := defaultLoopCfg(t)
	// Note: cfg.LoopDetection left zero — no Enabled flag, no Disabled flag.
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 100_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	if loop.Detector == nil {
		t.Fatal("Detector should be wired by default — config left at zero values")
	}
	if loop.Detector.SignatureWindowSize != 10 || loop.Detector.SignatureMaxRepeats != 5 {
		t.Errorf("default signature thresholds wrong: window=%d repeats=%d",
			loop.Detector.SignatureWindowSize, loop.Detector.SignatureMaxRepeats)
	}
}

func TestBuildAgentLoop_HonorsDetectorThresholds(t *testing.T) {
	cfg := defaultLoopCfg(t)
	cfg.LoopDetection.Warning = 7
	cfg.LoopDetection.Critical = 14
	cfg.LoopDetection.Global = 42
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 100_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	if loop.Detector == nil {
		t.Fatal("Detector should be wired")
	}
	if loop.Detector.WarningThreshold != 7 {
		t.Errorf("Warning = %d, want 7", loop.Detector.WarningThreshold)
	}
	if loop.Detector.CriticalThreshold != 14 {
		t.Errorf("Critical = %d, want 14", loop.Detector.CriticalThreshold)
	}
	if loop.Detector.GlobalThreshold != 42 {
		t.Errorf("Global = %d, want 42", loop.Detector.GlobalThreshold)
	}
}

func TestBuildAgentLoop_MemoryDirIsUnderCanonicalHome(t *testing.T) {
	cfg := defaultLoopCfg(t)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 100_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	// Just confirm the memory manager exists; we don't introspect its
	// internals (that's memory pkg's responsibility).
	_ = loop
	if _, err := os.Stat(filepath.Join(config.Home(), "memory")); err != nil {
		t.Fatalf("canonical memory root was not created: %v", err)
	}
}

// A cwd-local repository is a migration source, never a second active store.
func TestResolveMemoryRoot_IgnoresProjectDir(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".metis", "memory"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	oldWd, _ := os.Getwd()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	cfg := defaultLoopCfg(t)
	root := resolveMemoryRoot(cfg)
	want := filepath.Join(config.Home(), "memory")
	if root != want {
		t.Errorf("canonical root mismatch:\n  got:  %s\n  want: %s", root, want)
	}
}

// TestResolveMemoryRoot_FallsBackToCanonicalHome — without ./.metis/memory
// in cwd, the root must be <METIS_HOME>/memory. Guards against
// accidentally picking up a stray .metis dir from a parent directory
// (we deliberately don't walk up).
func TestResolveMemoryRoot_FallsBackToCanonicalHome(t *testing.T) {
	cleanCwd := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(cleanCwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	cfg := defaultLoopCfg(t)
	got := resolveMemoryRoot(cfg)
	want := filepath.Join(config.Home(), "memory")
	if got != want {
		t.Errorf("fallback root mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestBuildMemoryManagerMigratesLegacyGlobalRoot(t *testing.T) {
	cleanCwd := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(cleanCwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	cfg := defaultLoopCfg(t)
	legacy := filepath.Join(cfg.Session.Dir, "memory")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "old-topic.md"), []byte("legacy memory"), 0o644); err != nil {
		t.Fatal(err)
	}
	mm := BuildMemoryManager(cfg)
	if mm == nil {
		t.Fatal("BuildMemoryManager returned nil")
	}
	if mm.Root() != filepath.Join(config.Home(), "memory") {
		t.Fatalf("root=%q, want canonical home", mm.Root())
	}
	if got, err := os.ReadFile(filepath.Join(mm.Root(), "old-topic.md")); err != nil || string(got) != "legacy memory" {
		t.Fatalf("legacy migration failed: got=%q err=%v", got, err)
	}
}

func TestBuildMemoryManagerImportsLegacyJSONLDespiteCanonicalNameConflict(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".metis", "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldWd, _ := os.Getwd()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	cfg := defaultLoopCfg(t)
	legacy := filepath.Join(cfg.Session.Dir, "memory")
	canonical := filepath.Join(config.Home(), "memory")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyLine := `{"type":"fact","key":"migration","value":"Legacy Conflict Needle","created_at":"2026-08-27T12:00:00Z"}` + "\n"
	canonicalLine := `{"type":"fact","key":"migration","value":"Canonical Conflict Needle","created_at":"2026-08-27T11:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(legacy, "fact.jsonl"), []byte(legacyLine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "fact.jsonl"), []byte(canonicalLine), 0o600); err != nil {
		t.Fatal(err)
	}

	active := BuildMemoryManager(cfg)
	if active == nil || active.Root() != canonical {
		t.Fatalf("canonical repository did not remain active: %+v", active)
	}
	raw, err := os.ReadFile(filepath.Join(canonical, "archival", "passages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Canonical Conflict Needle") || !strings.Contains(string(raw), "Legacy Conflict Needle") {
		t.Fatalf("canonical repository did not import both conflicting JSONL sources: %s", raw)
	}
}

func TestBuildAgentLoopAutoRetrieveDefaultsOnAndAllowsOptOut(t *testing.T) {
	cfg := defaultLoopCfg(t)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 100_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
	})
	if loop.AutoRetrieveK != 5 {
		t.Fatalf("default AutoRetrieveK=%d, want 5", loop.AutoRetrieveK)
	}

	t.Setenv("METIS_AUTO_RETRIEVE", "off")
	loop = BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 100_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
	})
	if loop.AutoRetrieveK != 0 {
		t.Fatalf("explicit off AutoRetrieveK=%d, want 0", loop.AutoRetrieveK)
	}
}

// TestBuildAgentLoop_WiresMicrocompactDir — the Compactor.Microcompact
// path was dead code before 2026-05-15 because MicrocompactDir was
// empty, which is the kill switch. BuildAgentLoop now defaults the
// dir to <session.dir>/microcompact-cache.
func TestBuildAgentLoop_WiresMicrocompactDir(t *testing.T) {
	// Save and clear the env var in case the user has it set.
	oldEnv := os.Getenv("METIS_MICROCOMPACT")
	_ = os.Unsetenv("METIS_MICROCOMPACT")
	t.Cleanup(func() { _ = os.Setenv("METIS_MICROCOMPACT", oldEnv) })

	cfg := defaultLoopCfg(t)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 200_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	if loop.Compactor == nil {
		t.Fatal("compactor not wired")
	}
	want := filepath.Join(cfg.Session.Dir, "microcompact-cache")
	if loop.Compactor.MicrocompactDir != want {
		t.Errorf("MicrocompactDir = %q, want %q", loop.Compactor.MicrocompactDir, want)
	}
}

// TestBuildAgentLoop_MicrocompactDisabledByEnv — METIS_MICROCOMPACT=0
// must clear the dir back to "" so ShouldMicrocompact short-circuits.
func TestBuildAgentLoop_MicrocompactDisabledByEnv(t *testing.T) {
	oldEnv := os.Getenv("METIS_MICROCOMPACT")
	_ = os.Setenv("METIS_MICROCOMPACT", "0")
	t.Cleanup(func() { _ = os.Setenv("METIS_MICROCOMPACT", oldEnv) })

	cfg := defaultLoopCfg(t)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 200_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	if loop.Compactor.MicrocompactDir != "" {
		t.Errorf("MicrocompactDir = %q, want empty (METIS_MICROCOMPACT=0)", loop.Compactor.MicrocompactDir)
	}
}

// A historical project-local repository is imported, but every new write goes
// to the one canonical repository shared by Desktop, CLI, and Dream.
func TestBuildMemoryManager_ProjectScopeMigratesThenWritesCanonical(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".metis", "memory"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	legacyTopic := filepath.Join(projectDir, ".metis", "memory", "project_legacy.md")
	if err := os.WriteFile(legacyTopic, []byte("legacy project memory\n"), 0o600); err != nil {
		t.Fatalf("seed project memory: %v", err)
	}
	oldWd, _ := os.Getwd()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	cfg := defaultLoopCfg(t)
	mm := BuildMemoryManager(cfg)
	if mm == nil {
		t.Fatal("BuildMemoryManager returned nil")
	}
	canonicalRoot := filepath.Join(config.Home(), "memory")
	if mm.Root() != canonicalRoot {
		t.Fatalf("active root=%q, want canonical %q", mm.Root(), canonicalRoot)
	}
	if err := mm.Core().UpdateBlock("user", "写入统一 canonical memory"); err != nil {
		t.Fatalf("UpdateBlock: %v", err)
	}

	canonicalFile := filepath.Join(canonicalRoot, "core.d", "MEMORY.md")
	if _, err := os.Stat(canonicalFile); err != nil {
		t.Errorf("expected MEMORY.md under canonical root at %s: %v", canonicalFile, err)
	}
	hits := mm.SearchCandidates("legacy project memory", 10)
	foundProjectMemory := false
	for _, hit := range hits {
		if strings.Contains(hit.Content, "legacy project memory") {
			foundProjectMemory = true
			break
		}
	}
	if !foundProjectMemory {
		t.Fatalf("namespaced project migration was not retrievable: %+v", hits)
	}
	projectFile := filepath.Join(projectDir, ".metis", "memory", "core.d", "MEMORY.md")
	if _, err := os.Stat(projectFile); err == nil {
		t.Errorf("new write unexpectedly went to legacy project root: %s", projectFile)
	}
}

func TestMigrationAcrossWorkspacesPreservesIsolation(t *testing.T) {
	metisHome := t.TempDir()
	t.Setenv("METIS_HOME", metisHome)
	cfg := &config.Config{Session: config.Session{Dir: filepath.Join(metisHome, "sessions")}}
	workspaceA := filepath.Join(t.TempDir(), "workspace-a")
	workspaceB := filepath.Join(t.TempDir(), "workspace-b")
	for workspace, body := range map[string]string{
		workspaceA: "alpha workspace core needle",
		workspaceB: "beta workspace core needle",
	} {
		legacy := filepath.Join(workspace, ".metis", "memory")
		if err := os.MkdirAll(filepath.Join(legacy, "core.d"), 0o700); err != nil {
			t.Fatal(err)
		}
		indexNeedle := strings.Replace(body, "core", "index", 1)
		working := "---\nname: working\ndescription: " + indexNeedle + "\ntype: project\n---\n\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(legacy, "core.d", "WORKING.md"), []byte(working), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(legacy, "archival"), 0o700); err != nil {
			t.Fatal(err)
		}
		archiveBody := strings.Replace(body, "core", "archive", 1)
		archive := `{"id":"shared-project-id","content":` + strconv.Quote(archiveBody) + `,"type":"project"}` + "\n"
		if err := os.WriteFile(filepath.Join(legacy, "archival", "passages.jsonl"), []byte(archive), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacy, "preference.jsonl"),
			[]byte(`{"type":"preference","key":"editor","value":"global preference needle","created_at":"2026-08-28T05:00:00Z"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	buildIn := func(workspace string) *memory.MemoryManager {
		t.Helper()
		oldWD, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(workspace); err != nil {
			t.Fatal(err)
		}
		manager := BuildMemoryManager(cfg)
		if err := os.Chdir(oldWD); err != nil {
			t.Fatal(err)
		}
		if manager == nil {
			t.Fatal("BuildMemoryManager returned nil")
		}
		return manager
	}

	managerA := buildIn(workspaceA)
	managerB := buildIn(workspaceB)
	managerA = buildIn(workspaceA)
	assertWorkspaceHits := func(manager *memory.MemoryManager, own, other string) {
		t.Helper()
		ownHits := manager.SearchCandidates(own, 10)
		if len(ownHits) == 0 {
			t.Fatalf("current workspace missed %q", own)
		}
		foundOwnScope := false
		for _, hit := range ownHits {
			if strings.Contains(hit.Content, own) && strings.HasPrefix(hit.Scope, "workspace:") {
				foundOwnScope = true
			}
		}
		if !foundOwnScope {
			t.Fatalf("workspace memory lacked stable scope: %+v", ownHits)
		}
		ownArchive := strings.Replace(own, "core", "archive", 1)
		archiveHits := manager.SearchCandidates(ownArchive, 10)
		if len(archiveHits) == 0 || !strings.Contains(archiveHits[0].Content, ownArchive) || archiveHits[0].Scope == "" {
			t.Fatalf("current workspace archive was not scoped/retrievable: %+v", archiveHits)
		}
		for _, hit := range manager.SearchCandidates(other, 10) {
			if strings.Contains(hit.Content, other) {
				t.Fatalf("cross-workspace memory leaked for %q: %+v", other, hit)
			}
		}
		otherArchive := strings.Replace(other, "core", "archive", 1)
		for _, hit := range manager.SearchCandidates(otherArchive, 10) {
			if strings.Contains(hit.Content, otherArchive) {
				t.Fatalf("cross-workspace archive leaked for %q: %+v", otherArchive, hit)
			}
		}
		ownIndex := strings.Replace(own, "core", "index", 1)
		otherIndex := strings.Replace(other, "core", "index", 1)
		context := manager.BuildContext()
		if !strings.Contains(context, ownIndex) || strings.Contains(context, otherIndex) {
			t.Fatalf("workspace topic index was not isolated: %q", context)
		}
		preferenceHits := manager.SearchCandidates("global preference needle", 10)
		if len(preferenceHits) == 0 {
			t.Fatal("user preference was incorrectly workspace-isolated")
		}
		for _, hit := range preferenceHits {
			if strings.Contains(hit.Content, "global preference needle") && hit.Scope != "user" {
				t.Fatalf("user preference was not kept global: %+v", hit)
			}
		}
	}
	assertWorkspaceHits(managerA, "alpha workspace core needle", "beta workspace core needle")
	assertWorkspaceHits(managerB, "beta workspace core needle", "alpha workspace core needle")
}
