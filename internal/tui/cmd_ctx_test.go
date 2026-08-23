package tui

// cmd_ctx_test.go — pins the `/ctx` diagnostic command's output format
// added 2026-05-17 (user thread after screenshot 35/36 confusion).
// /ctx is the user-facing tool for "why hasn't auto-compact fired?";
// the lines below verify each diagnostic field renders so future
// refactors can't silently drop the bit a confused user needs.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// ctxTestProvider is the bare-minimum provider for /ctx output assertions
// — Just exposes ModelID + MaxContextTokens.
type ctxTestProvider struct {
	id     string
	maxCtx int
}

func (p *ctxTestProvider) Name() string          { return "ctx-test" }
func (p *ctxTestProvider) ModelID() string       { return p.id }
func (p *ctxTestProvider) MaxContextTokens() int { return p.maxCtx }
func (p *ctxTestProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (p *ctxTestProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	return nil, nil
}

// TestCmdCtx_PrintsAllDiagnosticFields — happy path: loop has a
// Provider + Compactor + nonzero ContextWindow. The output must
// include the provider model id, the context window number, the
// threshold (both ratio and %), the trigger token count, the current
// token estimate, a status line, and the iter index header.
func TestCmdCtx_PrintsAllDiagnosticFields(t *testing.T) {
	prov := &ctxTestProvider{id: "minimax-m2.7", maxCtx: 192_000}
	cfg := agent.DefaultCompactionConfig()
	cfg.Threshold = 0.8
	cfg.MinimumTokens = 50_000

	loop := agent.NewLoop(prov, tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.Model = "minimax-m2.7"
	loop.ContextWindow = prov.MaxContextTokens()
	loop.Compactor = agent.NewCompactor(cfg, "minimax-m2.7",
		prov.MaxContextTokens(), prov)
	loop.Compactor.MaxOutputTokens = 20_000

	r := &REPL{Loop: loop}

	out := cmdCtx(r, "")

	wantSubstrings := []string{
		"Context state",
		"provider model:",
		"minimax-m2.7",
		"context window:",
		"192,000 tokens",
		"threshold:",
		"0.80 (80%)",
		"trigger at:",
		"current tokens:",
		"status:",
		"iter index:",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(out, w) {
			t.Errorf("/ctx output missing %q; full output:\n%s", w, out)
		}
	}
}

// TestCmdCtx_UsesAuthoritativeTriggerTokens verifies that /ctx reports the
// same boundary as Compactor.ShouldCompact even when the full output reserve
// compatibility switch is enabled. A locally duplicated cap formula used to
// ignore this switch and display a different trigger from the one in use.
func TestCmdCtx_UsesAuthoritativeTriggerTokens(t *testing.T) {
	t.Setenv("METIS_COMPACT_RESERVE_FULL_MAX_TOKENS", "1")

	prov := &ctxTestProvider{id: "large-output-model", maxCtx: 192_000}
	cfg := agent.DefaultCompactionConfig()
	cfg.Threshold = 0.8

	loop := agent.NewLoop(prov, tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.Compactor = agent.NewCompactor(cfg, prov.id, prov.maxCtx, prov)
	loop.Compactor.MaxOutputTokens = 64_000

	out := cmdCtx(&REPL{Loop: loop}, "")
	want := fmt.Sprintf("trigger at:      %s tokens", fmtThousands(loop.Compactor.TriggerTokens()))
	if !strings.Contains(out, want) {
		t.Fatalf("/ctx trigger must match Compactor.TriggerTokens(); want %q in:\n%s", want, out)
	}
}

// TestCmdCtx_ClampsMinimumToTrigger verifies the diagnostic applies the same
// floor clamp as Compactor.ShouldCompact. A copied 200K minimum must not make
// /ctx claim compaction is gated past the real boundary of a smaller model.
func TestCmdCtx_ClampsMinimumToTrigger(t *testing.T) {
	prov := &ctxTestProvider{id: "small-window-model", maxCtx: 128_000}
	cfg := agent.DefaultCompactionConfig()
	cfg.Threshold = 0.85
	cfg.MinimumTokens = 200_000

	loop := agent.NewLoop(prov, tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.Compactor = agent.NewCompactor(cfg, prov.id, prov.maxCtx, prov)
	loop.Compactor.MaxOutputTokens = 20_000

	out := cmdCtx(&REPL{Loop: loop}, "")
	trigger := loop.Compactor.TriggerTokens()
	wantFloor := fmt.Sprintf("minimum tokens:  %s (configured %s; clamped to trigger)",
		fmtThousands(trigger), fmtThousands(cfg.MinimumTokens))
	if !strings.Contains(out, wantFloor) {
		t.Fatalf("/ctx must display the effective clamped floor; want %q in:\n%s", wantFloor, out)
	}
	wantStatus := fmt.Sprintf("BELOW MINIMUM FLOOR (%s < %s)",
		fmtThousands(loop.EstimateContextTokens()), fmtThousands(trigger))
	if !strings.Contains(out, wantStatus) {
		t.Fatalf("/ctx status must use the effective clamped floor; want %q in:\n%s", wantStatus, out)
	}
}

// TestCmdCtx_NoLoopGracefulMessage — REPL with no loop should not
// crash; it should surface a one-line explanation so the user knows
// the command isn't applicable (e.g. invoked from a surface that
// hasn't wired the agent yet).
func TestCmdCtx_NoLoopGracefulMessage(t *testing.T) {
	r := &REPL{Loop: nil}
	out := cmdCtx(r, "")
	if !strings.Contains(out, "not running") {
		t.Errorf("/ctx with no loop should explain; got %q", out)
	}
}

// TestCmdCtx_NoCompactorPrintsDisabled — if the Compactor is nil
// (compaction disabled for this build / session), /ctx must still
// print the provider lines and then declare compaction disabled
// rather than crash on a nil pointer.
func TestCmdCtx_NoCompactorPrintsDisabled(t *testing.T) {
	prov := &ctxTestProvider{id: "test-model", maxCtx: 100_000}
	loop := agent.NewLoop(prov, tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.Compactor = nil

	r := &REPL{Loop: loop}
	out := cmdCtx(r, "")
	if !strings.Contains(out, "compactor:") || !strings.Contains(out, "disabled") {
		t.Errorf("/ctx with no compactor should print 'compactor: ... disabled'; got:\n%s", out)
	}
}
