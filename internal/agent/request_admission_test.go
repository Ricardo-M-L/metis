package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type admissionRootProvider struct {
	*activeContextTestProvider
	window   int
	requests []llm.Request
}

func (p *admissionRootProvider) MaxContextTokens() int { return p.window }
func (p *admissionRootProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	p.requests = append(p.requests, req)
	return p.activeContextTestProvider.Stream(ctx, req)
}

// The host checkpoint runs after preflight/compaction and before final request
// assembly. A repository edit here reproduces the real unlocked interval;
// the root stream must not be opened before the newer input is admitted.
func TestDispatchRechecksMemoryChangedAfterPreflight(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "header", true: "local_checkpoint"}[local], func(t *testing.T) {
			mm, err := memory.NewMemoryManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := mm.Core().UpdateBlock("user", "old memory"); err != nil {
				t.Fatal(err)
			}
			root := &admissionRootProvider{activeContextTestProvider: &activeContextTestProvider{name: "admission", model: "fixture", responses: [][]llm.StreamEvent{activeContextResponse("done", 1000, 0, 0, 5)}}, window: 128000}
			var provider llm.Provider = root
			if local {
				provider = reviewDynamicCheckpointProvider{Provider: root}
			}
			loop := NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 2)
			loop.Model = "fixture"
			loop.Memory = mm
			loop.Messages = unifiedPipelineHistory()
			loop.AppendUser("continue")
			baseline := loop.EstimateRequestContextTokens(nil)
			root.window = 2 * (baseline + 200)
			cfg := DefaultCompactionConfig()
			cfg.Threshold = 0.5
			cfg.MinimumTokens = 0
			cfg.MaxSummarizeInputTokens = 0
			cfg.ProtectFirst = 1
			cfg.ProtectLast = 2
			summarizer := &fakeSummarizer{}
			loop.Compactor = NewCompactor(cfg, "fixture", 2*(baseline+200), summarizer)
			preflightRan, changed := false, false
			loop.CurrentStateSections = func() []llm.SystemSection { preflightRan = true; return nil }
			loop.HistoryCheckpoint = func([]llm.Message) error {
				if preflightRan && !changed {
					changed = true
					return mm.Core().UpdateBlock("user", strings.Repeat("new dispatch memory ", 150))
				}
				return nil
			}
			loop.BypassNextCache = true
			loop.rescueNoTools = true
			out := make(chan Event, 256)
			if err := loop.Run(context.Background(), out); err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("fixture did not edit memory in preflight/build interval")
			}
			compacted := false
			for len(out) > 0 {
				if ev := <-out; ev.Kind == EventContextCompacted {
					compacted = true
				}
			}
			if !compacted {
				t.Fatal("root provider dispatched the larger memory snapshot without rechecking automatic compaction")
			}
			if root.next != 1 {
				t.Fatalf("root provider calls=%d, want one", root.next)
			}
			if loop.BypassNextCache || loop.rescueNoTools {
				t.Fatal("one-shot posture was not consumed exactly once")
			}
			if len(root.requests[0].Tools) != 0 {
				t.Fatal("admission rebuild lost text-only rescue posture")
			}
			var wire strings.Builder
			if len(root.requests[0].SystemSections) > 0 {
				for _, section := range root.requests[0].SystemSections {
					wire.WriteString(section.Body)
				}
			} else {
				wire.WriteString(root.requests[0].System)
			}
			for _, message := range root.requests[0].Messages {
				for _, block := range message.Content {
					wire.WriteString(block.Text)
				}
			}
			if count := strings.Count(wire.String(), "<cache-refresh nonce="); count != 1 {
				t.Fatalf("cache refresh intent count=%d, want one", count)
			}
		})
	}
}

func TestDispatchAdmissionPreservesOriginalAndPendingOneShotPostures(t *testing.T) {
	root := &activeContextTestProvider{name: "posture", model: "fixture"}
	summarizer := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.Threshold = 0.5
	cfg.MinimumTokens = 0
	cfg.MaxSummarizeInputTokens = 0
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 2
	l := &Loop{Provider: root, Model: "fixture", System: "system", Messages: unifiedPipelineHistory(), Compactor: NewCompactor(cfg, "fixture", 1000, summarizer), BypassNextCache: true, rescueNoTools: true}
	specs := []llm.ToolSpec{{Name: "Read", InputSchema: map[string]any{"type": "object"}}}
	req, anchor := l.buildRequestWithContext(specs)
	if anchor.cacheRefreshNonce == "" || !anchor.rescueWithoutTools {
		t.Fatal("initial posture was not captured")
	}
	// A second user intent arrives after the immutable first request. Rebuild
	// must replay the first posture while leaving these flags for the next turn.
	l.BypassNextCache, l.rescueNoTools = true, true
	final, finalAnchor, err := l.admitBuiltRequest(context.Background(), nil, specs, 0, req, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if summarizer.calls == 0 {
		t.Fatal("fixture did not exercise a compaction/rebuild")
	}
	if len(final.Tools) != 0 || !finalAnchor.rescueWithoutTools || finalAnchor.cacheRefreshNonce != anchor.cacheRefreshNonce {
		t.Fatal("rebuild lost original one-shot posture")
	}
	if !l.BypassNextCache || !l.rescueNoTools {
		t.Fatal("rebuild consumed a later pending one-shot intent")
	}
}

func TestDispatchAdmissionRejectsHardInputOverflowWithoutCompactor(t *testing.T) {
	root := &activeContextTestProvider{name: "limit", model: "fixture"}
	l := &Loop{Provider: root, Model: "fixture", Messages: []llm.Message{msg(llm.RoleUser, strings.Repeat("large input ", 1000))}}
	l.contextCapabilities.MaxInputTokens = 100
	req, anchor := l.buildRequestWithContext(nil)
	if _, _, err := l.admitBuiltRequest(context.Background(), nil, nil, anchor.requestContextTokens, req, anchor); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("hard input overflow not rejected: %v", err)
	}
	if root.next != 0 {
		t.Fatal("overflow reached root provider")
	}
}
