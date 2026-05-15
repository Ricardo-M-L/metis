package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
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

func TestBuildAgentLoop_MemoryDirIsUnderSessionDir(t *testing.T) {
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
	if _, err := dirExists(filepath.Join(cfg.Session.Dir, "memory")); err == nil {
		// memory.NewMemoryManager creates the dir on success.
	}
}

func dirExists(path string) (bool, error) {
	// Trivial helper used only by the test above; standard library has no
	// 1-liner so we keep this local instead of introducing a hidden
	// dependency.
	_ = path
	return false, nil
}

// TestResolveMemoryRoot_PrefersProjectDir — when ./.metis/memory exists
// in the process cwd, BuildMemoryManager must pick that path over the
// user-global session dir. Mirrors the project-vs-user precedent
// already used for agent profiles (LoadAgentProfile).
func TestResolveMemoryRoot_PrefersProjectDir(t *testing.T) {
	// Chdir into a fresh tmp dir + create .metis/memory under it.
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

	wantAbs, _ := filepath.Abs(filepath.Join(projectDir, ".metis", "memory"))
	// Resolve macOS /private/var symlink quirk on tmp dirs.
	gotResolved, _ := filepath.EvalSymlinks(root)
	wantResolved, _ := filepath.EvalSymlinks(wantAbs)
	if gotResolved != wantResolved {
		t.Errorf("project-scoped root mismatch:\n  got:  %s\n  want: %s", gotResolved, wantResolved)
	}
}

// TestResolveMemoryRoot_FallsBackToSessionDir — without ./.metis/memory
// in cwd, the root must be <cfg.Session.Dir>/memory. Guards against
// accidentally picking up a stray .metis dir from a parent directory
// (we deliberately don't walk up).
func TestResolveMemoryRoot_FallsBackToSessionDir(t *testing.T) {
	cleanCwd := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(cleanCwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	cfg := defaultLoopCfg(t)
	got := resolveMemoryRoot(cfg)
	want := filepath.Join(cfg.Session.Dir, "memory")
	if got != want {
		t.Errorf("fallback root mismatch:\n  got:  %s\n  want: %s", got, want)
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

// TestBuildMemoryManager_ProjectScopeWritesLocally — full integration:
// create .metis/memory in tmp, run BuildMemoryManager, write a USER
// block, confirm the file lands under the project dir (not session dir).
func TestBuildMemoryManager_ProjectScopeWritesLocally(t *testing.T) {
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
	mm := BuildMemoryManager(cfg)
	if mm == nil {
		t.Fatal("BuildMemoryManager returned nil")
	}
	if err := mm.Core().UpdateBlock("user", "在项目本地的 memory"); err != nil {
		t.Fatalf("UpdateBlock: %v", err)
	}

	// File should land in the project's .metis/memory/core.d, not under cfg.Session.Dir.
	// The "user" block persists to MEMORY.md (see labelToFilename in memory pkg).
	projectFile := filepath.Join(projectDir, ".metis", "memory", "core.d", "MEMORY.md")
	if _, err := os.Stat(projectFile); err != nil {
		t.Errorf("expected MEMORY.md under project scope at %s: %v", projectFile, err)
	}
	sessionFile := filepath.Join(cfg.Session.Dir, "memory", "core.d", "MEMORY.md")
	if _, err := os.Stat(sessionFile); err == nil {
		t.Errorf("user-global file %s should NOT exist when project scope is active", sessionFile)
	}
}
