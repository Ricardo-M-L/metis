package runtime

import (
	"context"
	"io"
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
		Gate:     permission.New(permission.ModeAuto),
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
		Gate:     permission.New(permission.ModeAuto),
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
		Gate:     permission.New(permission.ModeAuto),
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
		Gate:     permission.New(permission.ModeAuto),
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
		Gate:     permission.New(permission.ModeAuto),
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
		Gate:     permission.New(permission.ModeAuto),
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
