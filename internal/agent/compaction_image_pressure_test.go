package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func nestedViewImageResult(id string, imageData ...string) llm.Message {
	blocks := []llm.ContentBlock{{Type: "text", Text: "Image loaded from /tmp/screenshot.png"}}
	for _, data := range imageData {
		blocks = append(blocks, llm.ContentBlock{Type: "image", MediaType: "image/png", Data: data})
	}
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Type: "tool_result", ToolUseID: id, ToolResultBlocks: blocks,
	}}}
}

func newImagePressureLoop() (*Loop, *fakeSummarizer) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.Threshold = 0.85
	cfg.MinimumTokens = 0
	cfg.MaxSummarizeInputTokens = 0
	cfg.RetainTokens = 2_048
	cfg.RetainMinMessages = 1
	cfg.RetainMinUserMessages = 1
	messages := unifiedPipelineHistory()
	messages = append(messages,
		toolUseMsg("screenshot", "ViewImage"),
		nestedViewImageResult("screenshot", strings.Repeat("A", 78_308), strings.Repeat("B", 1_268_708)),
		msg(llm.RoleAssistant, "The screenshot is visible; continue with the requested changes."),
	)
	return &Loop{Provider: p, Model: "vision-test", Compactor: NewCompactor(cfg, "vision-test", 200_000, p), Messages: messages}, p
}

func TestCompactNowImagePressureUsesModelVisibleHistory(t *testing.T) {
	for _, name := range []string{"authoritative-low", "authoritative-below-local", "active-fallback", "signed-overhead-fallback", "prefix-overhead-fallback", "authoritative-high"} {
		t.Run(name, func(t *testing.T) {
			loop, p := newImagePressureLoop()
			before := loop.History()
			active := estimateActiveHistoryTokens(before)
			if estimateTokens(before) < loop.Compactor.TriggerTokens() || active >= loop.Compactor.TriggerTokens() {
				t.Fatal("fixture must cross the trigger only when base64 is tokenized")
			}
			opts := CompactOptions{Trigger: "auto"}
			switch name {
			case "authoritative-low":
				opts.EstimatedContextTokens = 32_025
			case "authoritative-below-local":
				loop.Compactor.Threshold = 0.01
				opts.EstimatedContextTokens = 1_000
			case "signed-overhead-fallback":
				loop.Compactor.Threshold = 0.01
				loop.requestOverheadTokens.Store(int64(1_000 - active))
			case "prefix-overhead-fallback":
				loop.requestOverheadTokens.Store(int64(loop.Compactor.TriggerTokens() - active + 10_000))
			case "authoritative-high":
				opts.EstimatedContextTokens = loop.Compactor.TriggerTokens() + 10_000
			}
			result, err := loop.CompactNow(context.Background(), opts)
			if err != nil {
				t.Fatalf("CompactNow: %v", err)
			}
			if result.BeforeTokens != active {
				t.Errorf("history token result=%d want native-image estimate=%d", result.BeforeTokens, active)
			}
			if name == "authoritative-high" || name == "prefix-overhead-fallback" {
				if !result.Applied || p.calls != 1 {
					t.Fatalf("real request pressure did not compact: applied=%v calls=%d", result.Applied, p.calls)
				}
				return
			}
			if result.Applied || p.calls != 0 || !reflect.DeepEqual(loop.History(), before) {
				t.Fatalf("low native-image pressure compacted history: applied=%v calls=%d", result.Applied, p.calls)
			}
		})
	}
}

func TestCompactNowTextStillConsumesContextPressure(t *testing.T) {
	loop, p := newImagePressureLoop()
	loop.Messages = append(unifiedPipelineHistory(), msg(llm.RoleUser, strings.Repeat("A", 800_000)))
	result, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "auto"})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied || p.calls != 1 {
		t.Fatalf("ordinary text pressure was ignored: applied=%v calls=%d", result.Applied, p.calls)
	}
}

func TestMaybeCompactRepeatedNestedImagesDoNotManufactureContextPressure(t *testing.T) {
	loop, p := newImagePressureLoop()
	for round := 0; round < 18; round++ {
		id := "repeated-screenshot-" + istr(round)
		loop.Messages = append(loop.Messages,
			toolUseMsg(id, "ViewImage"),
			nestedViewImageResult(id, strings.Repeat("C", 1_268_708)),
			msg(llm.RoleAssistant, "I can see the image."),
		)
		before := loop.History()
		pressure := loop.EstimateRequestContextTokens(nil)
		if pressure >= loop.Compactor.TriggerTokens() {
			t.Fatalf("round %d fixture unexpectedly crossed native-image pressure: %d", round, pressure)
		}
		loop.maybeCompactWithPressure(context.Background(), nil, pressure)
		if p.calls != 0 || !reflect.DeepEqual(loop.History(), before) {
			t.Fatalf("round %d compacted a low-pressure image history: summary calls=%d pressure=%d", round, p.calls, pressure)
		}
	}
}

func TestCompactNowProjectedCheckpointCannotManufactureContextPressure(t *testing.T) {
	for _, usePreflight := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "preflight"}[usePreflight], func(t *testing.T) {
			loop, p := newImagePressureLoop()
			loop.Messages = unifiedPipelineHistory()
			checkpoint, _ := projectCheckpointSnapshot(nil, []llm.SystemSection{{
				Name: "runtime_state", Body: strings.Repeat("obsolete runtime checkpoint ", 50_000),
			}})
			loop.Messages = append(checkpoint, loop.Messages...)
			before := loop.History()
			opts := CompactOptions{Trigger: "auto"}
			if usePreflight {
				opts.EstimatedContextTokens = loop.EstimateRequestContextTokens(nil)
			}
			result, err := loop.CompactNow(context.Background(), opts)
			if err != nil {
				t.Fatalf("CompactNow: %v", err)
			}
			if result.Applied || p.calls != 0 || !reflect.DeepEqual(loop.History(), before) {
				t.Fatalf("projected-out checkpoint triggered compaction: applied=%v calls=%d", result.Applied, p.calls)
			}
			if want := estimateActiveHistoryTokens(loop.projectDynamicCheckpoints(before)); result.BeforeTokens != want {
				t.Fatalf("result counts projected-out checkpoint: got=%d want=%d", result.BeforeTokens, want)
			}
		})
	}
}

func TestCompactNowProjectedCheckpointKeepsRawPersistenceSource(t *testing.T) {
	loop, p := newImagePressureLoop()
	checkpoint, _ := projectCheckpointSnapshot(nil, []llm.SystemSection{{
		Name: "runtime_state", Body: strings.Repeat("OLD_OMITTED_CHECKPOINT ", 50_000),
	}})
	loop.Messages = append(checkpoint, unifiedPipelineHistory()...)
	before := loop.History()
	var persistedBefore []llm.Message
	loop.CompactionCheckpoint = func(rawBefore, replacement []llm.Message) error {
		persistedBefore = cloneMessages(rawBefore)
		return nil
	}
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "auto", EstimatedContextTokens: loop.Compactor.TriggerTokens() + 10_000,
	})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied || p.calls != 1 {
		t.Fatalf("real pressure did not summarize projected history: applied=%v calls=%d", result.Applied, p.calls)
	}
	if !reflect.DeepEqual(persistedBefore, before) {
		t.Fatal("transport projection changed the raw checkpoint persistence source")
	}
	if historyHasText(p.lastReq.Messages, "OLD_OMITTED_CHECKPOINT") {
		t.Fatal("summarizer received a checkpoint omitted from the model's request")
	}
	if !historyHasText(p.lastReq.Messages, "ancient greeting") || !historyHasText(result.History, "continue with the current task") {
		t.Fatal("projection discarded replayed source history or the active user request")
	}
}

func TestCompactNowProjectedCheckpointIsNotCompactionProgress(t *testing.T) {
	for _, trigger := range []string{"auto", "manual", "overflow", "iteration-budget"} {
		t.Run(trigger, func(t *testing.T) {
			loop, p := newImagePressureLoop()
			checkpoint, _ := projectCheckpointSnapshot(nil, []llm.SystemSection{{Name: "runtime_state", Body: "old state"}})
			loop.Messages = append(checkpoint, msg(llm.RoleUser, "hello"), msg(llm.RoleAssistant, "world"))
			before := loop.History()
			checkpointCalls := 0
			loop.CompactionCheckpoint = func([]llm.Message, []llm.Message) error { checkpointCalls++; return nil }
			result, err := loop.CompactNow(context.Background(), CompactOptions{
				Trigger: trigger, Force: trigger != "auto", Emergency: trigger == "overflow",
				EstimatedContextTokens: loop.Compactor.TriggerTokens() + 10_000,
			})
			if err != nil {
				t.Fatalf("CompactNow: %v", err)
			}
			if result.Applied || p.calls != 0 || checkpointCalls != 0 || !reflect.DeepEqual(loop.History(), before) {
				t.Fatalf("deleting a projected-out checkpoint counted as model-context progress: applied=%v summaries=%d checkpoints=%d", result.Applied, p.calls, checkpointCalls)
			}
		})
	}
}

func TestCompactNowImageCheapPreparationUsesActivePressureDelta(t *testing.T) {
	loop, c, p, _ := newCheapPreparationLoop(t)
	loop.Messages = append(loop.Messages,
		toolUseMsg("latest-image", "ViewImage"),
		nestedViewImageResult("latest-image", strings.Repeat("A", 1_268_708)),
	)
	before := loop.History()
	prepared := c.Microcompact(before)
	reduction := estimateActiveHistoryTokens(before) - estimateActiveHistoryTokens(prepared)
	if reduction <= 0 || estimateTokens(prepared) < c.TriggerTokens() {
		t.Fatal("fixture must retain large image transport data after useful text preparation")
	}
	result, err := loop.CompactNow(context.Background(), CompactOptions{
		Trigger: "auto", EstimatedContextTokens: c.TriggerTokens() + reduction - 1,
	})
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !result.Applied || p.calls != 0 {
		t.Fatalf("native image bytes overrode successful cheap preparation: applied=%v calls=%d", result.Applied, p.calls)
	}
	if !reflect.DeepEqual(result.History, prepared) {
		t.Fatal("cheap preparation did not preserve the native image payload")
	}
}

func TestAutoCompactWatermarkIgnoresBase64GrowthAndRearmsForNewMaterial(t *testing.T) {
	loop, _ := newImagePressureLoop()
	active := estimateActiveHistoryTokens(loop.Messages)
	overhead := loop.Compactor.TriggerTokens() + 1_000
	loop.noteAutoCompactPressure(CompactResult{
		Applied: true, BeforeTokens: active, AfterTokens: active, History: loop.History(),
	}, active+overhead, loop.autoCompactGenerationSnapshot())
	if !loop.autoCompactPressurePinned {
		t.Fatal("fixture did not arm pressure watermark")
	}
	// Replacing transport bytes for an existing native image adds no model-visible
	// context and must not permit another paid summary of the same checkpoint.
	image := &loop.Messages[len(loop.Messages)-2].Content[0].ToolResultBlocks[1]
	image.Data += strings.Repeat("D", 1_000_000)
	if !loop.deferRepeatedAutoCompact(active + overhead) {
		t.Fatal("base64 transport growth re-armed automatic compaction")
	}
	loop.AppendUser(strings.Repeat("new material ", autoCompactRearmGrowthTokens(loop.Compactor.TriggerTokens())))
	current := estimateActiveHistoryTokens(loop.Messages)
	if loop.deferRepeatedAutoCompact(current + overhead) {
		t.Fatal("new model-visible material did not re-arm automatic compaction")
	}
}

func TestAutoCompactWatermarkDoesNotRaiseAuthoritativePressureToLocalFloor(t *testing.T) {
	loop, _ := newImagePressureLoop()
	loop.Compactor.Threshold = 0.01 // 2K trigger
	after := estimateActiveHistoryTokens(loop.Messages)
	pressure := loop.Compactor.TriggerTokens() + 100
	before := after + pressure
	loop.noteAutoCompactPressure(CompactResult{
		Applied: true, BeforeTokens: before, AfterTokens: after, History: loop.History(),
	}, pressure, loop.autoCompactGenerationSnapshot())
	if loop.autoCompactPressurePinned {
		t.Fatalf("local estimate overrode lower provider pressure after compaction: before=%d after=%d pressure=%d", before, after, pressure)
	}
}
