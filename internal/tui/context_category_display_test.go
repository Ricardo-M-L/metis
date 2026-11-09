package tui

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

type contextRegistryReadProbe struct {
	mcpLoginTestTool
	reads atomic.Int32
}

func (p *contextRegistryReadProbe) Description() string {
	p.reads.Add(1)
	return "live registry description"
}

func (p *contextRegistryReadProbe) InputSchema() map[string]any {
	p.reads.Add(1)
	return map[string]any{"type": "object"}
}

func assertContextSnapshotCategories(t *testing.T, out string, snapshot agent.ContextUsage) {
	t.Helper()
	if snapshot.Breakdown == nil {
		t.Fatal("fixture has no request breakdown")
	}
	for _, category := range []struct {
		label  string
		tokens int
	}{
		{"System prompt", snapshot.Breakdown.SystemTokens},
		{"System tools", snapshot.Breakdown.ToolTokens},
		{"Messages", snapshot.Breakdown.MessageTokens},
	} {
		want := fmt.Sprintf("%s: %s tokens", category.label, fmtThousands(category.tokens))
		if !strings.Contains(out, want) {
			t.Errorf("context category disagrees with display snapshot: missing %q", want)
		}
	}
	if !strings.Contains(out, "by category (estimated)") {
		t.Error("context categories do not identify local estimates")
	}
}

func TestContextCategoriesStayCommittedDuringCheckingAndSaving(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	for _, stage := range []string{"checking", "saving"} {
		t.Run(stage, func(t *testing.T) {
			m := minimalModel(200000)
			configureRewindSummary(t, m)
			for range 15 {
				m.loop.AppendUser(strings.Repeat("old context ", 100))
			}
			m.loop.SystemSections = []llm.SystemSection{{Name: "base", Body: strings.Repeat("canonical prefix ", 20)}}
			m.loop.EstimateRequestContextTokens([]llm.ToolSpec{{Name: "PreparedTool", Description: strings.Repeat("prepared tool schema ", 20)}})
			before := m.loop.ContextDisplaySnapshot()
			probe := &contextRegistryReadProbe{}
			m.loop.Registry.Register(probe)
			started, release := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			block := func() {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			opts := agent.CompactOptions{Force: true}
			if stage == "checking" {
				hooks := agent.NewHookRegistry()
				hooks.Register(agent.PostCompactHandler(func(context.Context, agent.HookContext, *agent.PostCompact) *agent.ModifiedPostCompact {
					block()
					return nil
				}))
				m.loop.Hooks = hooks
			} else {
				opts.Persist = func([]llm.Message) error { block(); return ctx.Err() }
			}
			done := make(chan error, 1)
			go func() { _, err := m.loop.CompactNow(ctx, opts); done <- err }()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("compaction ended before %s: %v", stage, err)
			case <-time.After(3 * time.Second):
				t.Fatalf("compaction never reached %s", stage)
			}
			if candidate := m.loop.ContextUsageSnapshot(); candidate.Used >= before.Used {
				t.Error("fixture did not install a smaller candidate")
			}
			if got := m.loop.ContextDisplaySnapshot(); got.CompactionStage != stage || got.Used != before.Used {
				t.Errorf("fixture did not freeze committed context: %+v", got)
			}
			out := stripANSI(renderContext(m))
			assertContextSnapshotCategories(t, out, before)
			if probe.reads.Load() != 0 {
				t.Error("active context details read live tool schemas")
			}
			if !strings.Contains(out, "Resource details available after compaction is saved.") || strings.Contains(out, "MCP tools") {
				t.Error("active context details mixed live resource inventory with the committed snapshot")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			out = stripANSI(renderContext(m))
			if !strings.Contains(out, "MCP tools") || probe.reads.Load() == 0 {
				t.Error("committed context did not restore its resource inventory")
			}
		})
	}
}

func TestContextCategoriesUseCanonicalImageAwareBreakdown(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := minimalModel(200000)
	m.loop.SystemSections = []llm.SystemSection{{Name: "base", Body: strings.Repeat("canonical prefix ", 20)}}
	m.loop.AppendUserBlocks([]llm.ContentBlock{{Type: "text", Text: "look at this image"}, {Type: "image", Data: strings.Repeat("A", 1024)}})
	m.loop.EstimateRequestContextTokens([]llm.ToolSpec{{Name: "PreparedTool", Description: strings.Repeat("prepared tool schema ", 20)}})
	assertContextSnapshotCategories(t, stripANSI(renderContext(m)), m.loop.ContextDisplaySnapshot())
}

func TestContextCategoriesDistinguishMissingBreakdownFromKnownZero(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := minimalModel(200000)
	m.loop.AppendUser("unprepared history")
	out := stripANSI(renderContext(m))
	for _, label := range []string{"System prompt", "System tools", "Messages"} {
		if !strings.Contains(out, label+": unknown") {
			t.Errorf("missing breakdown invented %s estimate", label)
		}
	}
	m.loop = &agent.Loop{}
	m.loop.EstimateRequestContextTokens(nil)
	out = stripANSI(renderContext(m))
	assertContextSnapshotCategories(t, out, m.loop.ContextDisplaySnapshot())
	if strings.Contains(out, ": unknown") {
		t.Error("known zero categories were treated as unavailable")
	}
}
