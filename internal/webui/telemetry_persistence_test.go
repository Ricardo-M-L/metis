package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type telemetryPersistenceProvider struct {
	calls int
}

type terminalUsageRelease struct {
	reached chan struct{}
	release chan struct{}
}

type cancelledTerminalUsageProvider struct {
	streams chan *terminalUsageRelease
}

func (*cancelledTerminalUsageProvider) Name() string          { return "cancelled-terminal-usage" }
func (*cancelledTerminalUsageProvider) ModelID() string       { return "cancelled-terminal-usage-model" }
func (*cancelledTerminalUsageProvider) MaxContextTokens() int { return 128_000 }
func (*cancelledTerminalUsageProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("cancelled terminal usage test expects streaming")
}
func (p *cancelledTerminalUsageProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	state := &terminalUsageRelease{reached: make(chan struct{}), release: make(chan struct{})}
	p.streams <- state
	return &cancelledTerminalUsageStream{state: state}, nil
}

type cancelledTerminalUsageStream struct {
	state *terminalUsageRelease
	index int
}

func (*cancelledTerminalUsageStream) Close() error { return nil }
func (s *cancelledTerminalUsageStream) Recv() (llm.StreamEvent, error) {
	switch s.index {
	case 0:
		s.index++
		return llm.StreamEvent{Type: "text_delta", TextDelta: "terminal usage"}, nil
	case 1:
		s.index++
		close(s.state.reached)
		<-s.state.release
		return llm.StreamEvent{
			Type: "message_stop", StopReason: "end_turn",
			InputTokens: 11, OutputTokens: 7, CacheCreationInputTokens: 3, CacheReadInputTokens: 5,
		}, nil
	default:
		return llm.StreamEvent{}, errors.New("unexpected read after terminal message_stop")
	}
}

func (*telemetryPersistenceProvider) Name() string          { return "telemetry-persistence" }
func (*telemetryPersistenceProvider) ModelID() string       { return "telemetry-persistence-model" }
func (*telemetryPersistenceProvider) MaxContextTokens() int { return 128_000 }
func (*telemetryPersistenceProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("telemetry persistence test expects streaming")
}
func (p *telemetryPersistenceProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls++
	if p.calls == 1 {
		return &composerSummaryStream{events: []llm.StreamEvent{
			{Type: "message_start", InputTokens: 100, CacheCreationInputTokens: 20, CacheReadInputTokens: 30},
			{Type: "tool_use_start", ToolUseID: "persist-telemetry", ToolName: "PersistMarker"},
			{Type: "tool_input_delta", ToolUseID: "persist-telemetry", InputDelta: `{}`},
			{Type: "tool_use_stop", ToolUseID: "persist-telemetry", InputDelta: `{}`},
			{Type: "message_delta", StopReason: "tool_use", OutputTokens: 10},
			{Type: "message_stop"},
		}}, nil
	}
	return &composerSummaryStream{events: []llm.StreamEvent{
		{Type: "message_start", InputTokens: 200, CacheCreationInputTokens: 40, CacheReadInputTokens: 50},
		{Type: "text_delta", TextDelta: "telemetry persisted"},
		{Type: "message_delta", StopReason: "end_turn", OutputTokens: 20},
		{Type: "message_stop"},
	}}, nil
}

func TestDesktopTurnPersistsCumulativeCostAndPerTurnUsage(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "desktop-telemetry-persistence"
	provider := &telemetryPersistenceProvider{}
	registry := tools.NewRegistry()
	registry.Register(turnTailMarkerTool{})
	loop := agent.NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 4)
	loop.Model = provider.ModelID()
	if err := store.WriteHeaderFull(session.Header{
		ID: sessionID, Provider: provider.Name(), Model: provider.ModelID(), System: "system", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: sessionID,
		ProviderName:     provider.Name(),
	})

	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(
		`{"sessionId":"`+sessionID+`","input":"persist usage"}`,
	)))
	if rr.Code != http.StatusOK {
		t.Fatalf("turn = %d: %s", rr.Code, rr.Body.String())
	}
	wantCost := session.CostSnapshot{
		InputTokens: 300, OutputTokens: 30, CacheCreateTokens: 60, CacheReadTokens: 80,
	}
	if got, ok, err := store.ReadCost(sessionID); err != nil || !ok || got != wantCost {
		t.Fatalf("persisted cost = %+v ok=%v err=%v, want %+v", got, ok, err, wantCost)
	}
	metrics, err := store.ReadMessageMetrics(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 {
		t.Fatalf("message metrics = %+v, want one turn", metrics)
	}
	got := metrics[0]
	if got.InputTokens != 300 || got.OutputTokens != 30 || got.CacheCreateTokens != 60 || got.CacheReadTokens != 80 {
		t.Fatalf("per-turn usage = %+v", got)
	}
}

func TestDesktopTurnAdvancesPastDurableMetricsWhenTraceWasLost(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	adapter := rtpkg.InstallTrace(t.TempDir())
	if adapter == nil {
		t.Fatal("InstallTrace returned nil adapter")
	}
	traceStore := rtpkg.CurrentTraceStore()
	t.Cleanup(func() {
		adapter.SetResolvedEventObserver(nil)
		_ = traceStore.Close()
		rtpkg.SetTraceAdapter(oldAdapter)
		if oldAdapter != nil {
			agent.SetTraceHook(oldAdapter.OnEvent)
		} else {
			agent.SetTraceHook(nil)
		}
	})

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "desktop-lost-trace-turn-floor"
	provider := &telemetryPersistenceProvider{}
	registry := tools.NewRegistry()
	registry.Register(turnTailMarkerTool{})
	loop := agent.NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 4)
	loop.Model = provider.ModelID()
	if err := store.WriteHeaderFull(session.Header{
		ID: sessionID, Provider: provider.Name(), Model: provider.ModelID(), System: "system", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.August, 27, 15, 28, 0, 0, time.UTC)
	for _, metric := range []session.MessageMetric{
		{Turn: 1, StartedAt: base, CompletedAt: base.Add(time.Second), DurationMS: 1_000, InputTokens: 100, OutputTokens: 10, CacheCreateTokens: 20, CacheReadTokens: 80},
		{Turn: 2, StartedAt: base.Add(time.Minute), CompletedAt: base.Add(time.Minute + time.Second), DurationMS: 1_000, InputTokens: 200, OutputTokens: 20, CacheCreateTokens: 30, CacheReadTokens: 150},
	} {
		if _, err := store.ReconcileMessageMetric(sessionID, metric); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(store.Dir, sessionID+".message-metrics.jsonl")); err != nil {
		t.Fatal(err)
	}

	// Model a restart where only cost.json/accounted_turns survived: both the
	// presentation metrics and trace directory were lost. Recovery must rebuild
	// the metrics and make the next trace turn follow the ledger's max turn,
	// instead of reopening turn 1 and merging unrelated usage into it.
	adapter.SetSession(sessionID)
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: sessionID,
		ProviderName:     provider.Name(),
		TraceAdapter:     adapter,
		TraceStore:       traceStore,
	})
	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(
		`{"sessionId":"`+sessionID+`","input":"continue after trace loss"}`,
	)))
	if rr.Code != http.StatusOK {
		t.Fatalf("turn = %d: %s", rr.Code, rr.Body.String())
	}

	metrics, err := store.ReadMessageMetrics(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 3 {
		t.Fatalf("message metrics = %+v, want three distinct turns", metrics)
	}
	last := metrics[2]
	if last.Turn != 3 || last.InputTokens != 300 || last.OutputTokens != 30 ||
		last.CacheCreateTokens != 60 || last.CacheReadTokens != 80 {
		t.Fatalf("recovered turn metric = %+v, want new usage on turn 3", last)
	}
	wantCost := session.CostSnapshot{
		InputTokens: 600, OutputTokens: 60, CacheCreateTokens: 110, CacheReadTokens: 310,
	}
	if got, ok, err := store.ReadCost(sessionID); err != nil || !ok || got != wantCost {
		t.Fatalf("recovered cumulative cost = %+v ok=%v err=%v, want %+v", got, ok, err, wantCost)
	}
	if got := traceStore.CurrentTurn(sessionID); got != 3 {
		t.Fatalf("trace current turn = %d, want ledger-aligned turn 3", got)
	}
	for _, ev := range traceStore.Events(sessionID) {
		if ev.Turn != 3 {
			t.Fatalf("new trace event reused old turn: %+v", ev)
		}
	}
}

func TestNextMessageMetricTurnUsesLedgerWhenMetricsAndTraceWereLost(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	rtpkg.SetTraceAdapter(nil)
	t.Cleanup(func() { rtpkg.SetTraceAdapter(oldAdapter) })

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "desktop-ledger-only-turn-floor"
	if _, err := store.ReconcileMessageMetric(sessionID, session.MessageMetric{
		Turn: 4, InputTokens: 400, OutputTokens: 40, CacheReadTokens: 300,
	}); err != nil {
		t.Fatal(err)
	}
	// Model the second half of the crash window: cost.json/accounted_turns was
	// committed, but message-metrics was lost and tracing is disabled/lost.
	if err := os.Remove(filepath.Join(store.Dir, sessionID+".message-metrics.jsonl")); err != nil {
		t.Fatal(err)
	}
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{})
	if got := server.nextMessageMetricTurn(sessionID, nil); got != 5 {
		t.Fatalf("next metric turn = %d, want ledger max turn + 1", got)
	}
}

func TestTraceHistoryRecoversOnlyPersistedAggregateTelemetry(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(oldAdapter)
	adapter := rtpkg.InstallTrace(t.TempDir())
	if adapter == nil {
		t.Fatal("InstallTrace returned nil adapter")
	}
	defer func() {
		if traceStore := rtpkg.CurrentTraceStore(); traceStore != nil {
			_ = traceStore.Close()
		}
	}()

	server, store := testServer(t)
	const sessionID = "history-sidecar-telemetry"
	if err := store.WriteHeader(sessionID, "model", "system"); err != nil {
		t.Fatal(err)
	}
	for _, message := range []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "first"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "one"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "second"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "two"}}},
	} {
		if err := store.AppendMessage(sessionID, message); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, time.August, 27, 15, 28, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	for _, metric := range []session.MessageMetric{
		{Turn: 1, StartedAt: base, CompletedAt: base.Add(8 * time.Second), DurationMS: 8_000, TTFTMS: 1_000, InputTokens: 100, OutputTokens: 40, CacheCreateTokens: 20, CacheReadTokens: 80, TokPerSec: 8},
		{Turn: 2, StartedAt: base.Add(time.Minute), CompletedAt: base.Add(time.Minute + 12*time.Second), DurationMS: 12_000, TTFTMS: 2_000, InputTokens: 200, OutputTokens: 60, CacheCreateTokens: 10, CacheReadTokens: 190, TokPerSec: 6},
	} {
		if err := store.AppendMessageMetric(sessionID, metric); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.WriteCost(sessionID, session.CostSnapshot{
		InputTokens: 300, OutputTokens: 100, CacheCreateTokens: 30, CacheReadTokens: 270,
	}); err != nil {
		t.Fatal(err)
	}
	store.NewTimingRecorder(sessionID).Record("Bash", 2500*time.Millisecond, false)

	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/trace?sessionId="+sessionID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("trace = %d: %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Source string `json:"source"`
		Stats  struct {
			DurationMs    int64   `json:"durationMs"`
			ToolCalls     int     `json:"toolCalls"`
			ToolMs        int64   `json:"toolMs"`
			LlmMs         int64   `json:"llmMs"`
			InputTokens   int64   `json:"inputTokens"`
			OutputTokens  int64   `json:"outputTokens"`
			CacheRead     int64   `json:"cacheReadTokens"`
			CacheWrite    int64   `json:"cacheWriteTokens"`
			CacheHitRate  float64 `json:"cacheHitRate"`
			TokPerSec     float64 `json:"tokPerSec"`
			TtftAverageMs int64   `json:"ttftAverageMs"`
		} `json:"stats"`
		TurnMetrics []map[string]any `json:"turnMetrics"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Source != "history" || len(payload.TurnMetrics) != 2 {
		t.Fatalf("history fallback identity = source %q metrics %+v", payload.Source, payload.TurnMetrics)
	}
	stats := payload.Stats
	if stats.DurationMs != 20_000 || stats.ToolCalls != 1 || stats.ToolMs != 2_500 || stats.LlmMs != 17_500 {
		t.Fatalf("recovered timing stats = %+v", stats)
	}
	if stats.InputTokens != 600 || stats.OutputTokens != 100 || stats.CacheRead != 270 || stats.CacheWrite != 30 {
		t.Fatalf("recovered token stats = %+v", stats)
	}
	// Per-turn rates are aggregated over their matching generation seconds:
	// 40/8 + 60/6 = 15s, therefore 100/15 = 6.666... tok/s.
	if stats.CacheHitRate != 45 || stats.TtftAverageMs != 1_500 || stats.TokPerSec < 6.66 || stats.TokPerSec > 6.67 {
		t.Fatalf("recovered derived stats = %+v", stats)
	}
}

func TestLegacyPartialMetricsNeverReportToolTimeBeyondDuration(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(oldAdapter)
	if rtpkg.InstallTrace(t.TempDir()) == nil {
		t.Fatal("InstallTrace returned nil adapter")
	}
	defer func() {
		if traceStore := rtpkg.CurrentTraceStore(); traceStore != nil {
			_ = traceStore.Close()
		}
	}()

	server, store := testServer(t)
	const sessionID = "legacy-partial-telemetry"
	if err := store.WriteHeader(sessionID, "model", "system"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(sessionID, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "legacy"}}}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.August, 27, 15, 28, 0, 0, time.UTC)
	// v0.4.33 persisted only output and its directly measured per-turn rate.
	if err := store.AppendMessageMetric(sessionID, session.MessageMetric{
		Turn: 1, StartedAt: base, CompletedAt: base.Add(time.Second), DurationMS: 1_000,
		OutputTokens: 100, TokPerSec: 20,
	}); err != nil {
		t.Fatal(err)
	}
	// A partially migrated session may have a broader cumulative cost file and
	// timing rows than its single surviving message metric.
	if err := store.WriteCost(sessionID, session.CostSnapshot{InputTokens: 500, OutputTokens: 100_000}); err != nil {
		t.Fatal(err)
	}
	store.NewTimingRecorder(sessionID).Record("Bash", 5*time.Second, false)

	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/trace?sessionId="+sessionID, nil))
	var payload struct {
		Stats traceStats `json:"stats"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Stats.ToolMs > payload.Stats.DurationMs {
		t.Fatalf("inconsistent partial timing: tool=%dms duration=%dms", payload.Stats.ToolMs, payload.Stats.DurationMs)
	}
	if payload.Stats.TokPerSec != 20 {
		t.Fatalf("tok/s = %.2f, want persisted per-turn evidence 20 (not a rate derived across mismatched coverage)", payload.Stats.TokPerSec)
	}
}

func TestCancelledTurnPersistsDeliveredTerminalUsage(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(oldAdapter)
	adapter := rtpkg.InstallTrace(t.TempDir())
	if adapter == nil {
		t.Fatal("InstallTrace returned nil adapter")
	}
	defer func() {
		if traceStore := rtpkg.CurrentTraceStore(); traceStore != nil {
			_ = traceStore.Close()
		}
	}()

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "cancelled-terminal-usage"
	provider := &cancelledTerminalUsageProvider{streams: make(chan *terminalUsageRelease)}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypassPermissions), nil, "system", 2)
	loop.Model = provider.ModelID()
	if err := store.WriteHeaderFull(session.Header{
		ID: sessionID, Provider: provider.Name(), Model: provider.ModelID(), System: "system", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: sessionID,
		ProviderName:     provider.Name(),
	})
	adapter.SetSession(sessionID)

	// emit() races a ready channel send against ctx.Done(). Repeating the
	// deterministic cancellation boundary makes the old lossy WebUI accounting
	// fail reliably while keeping every provider response terminal and valid.
	const turns = 32
	for turn := 0; turn < turns; turn++ {
		requestCtx, cancel := context.WithCancel(context.Background())
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(
				`{"sessionId":"`+sessionID+`","input":"terminal usage"}`,
			)).WithContext(requestCtx)
			server.handler().ServeHTTP(rr, req)
			done <- rr
		}()
		state := <-provider.streams
		<-state.reached
		cancel()
		close(state.release)
		rr := <-done
		if rr.Code != http.StatusOK {
			t.Fatalf("turn %d = %d: %s", turn+1, rr.Code, rr.Body.String())
		}
	}
	want := session.CostSnapshot{
		InputTokens: 11 * turns, OutputTokens: 7 * turns,
		CacheCreateTokens: 3 * turns, CacheReadTokens: 5 * turns,
	}
	if got, ok, err := store.ReadCost(sessionID); err != nil || !ok || got != want {
		t.Fatalf("cancelled terminal usage = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

func TestTraceHistoryWithoutSidecarsDoesNotFabricateTelemetry(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(oldAdapter)
	if rtpkg.InstallTrace(t.TempDir()) == nil {
		t.Fatal("InstallTrace returned nil adapter")
	}
	defer func() {
		if traceStore := rtpkg.CurrentTraceStore(); traceStore != nil {
			_ = traceStore.Close()
		}
	}()
	server, store := testServer(t)
	const sessionID = "history-no-sidecars"
	if err := store.WriteHeader(sessionID, "model", "system"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(sessionID, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/trace?sessionId="+sessionID, nil))
	var payload struct {
		Stats traceStats `json:"stats"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Stats.DurationMs != 0 || payload.Stats.ToolMs != 0 || payload.Stats.InputTokens != 0 || payload.Stats.OutputTokens != 0 || payload.Stats.TtftAverageMs != 0 {
		t.Fatalf("history fallback fabricated unavailable telemetry: %+v", payload.Stats)
	}
}

func TestStatusIncludesActiveSessionIdentity(t *testing.T) {
	server, _ := testServer(t)
	server.stateMu.Lock()
	server.activeSessionID = "active-status-session"
	server.stateMu.Unlock()
	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var payload struct {
		ActiveSessionID string `json:"activeSessionId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ActiveSessionID != "active-status-session" {
		t.Fatalf("activeSessionId = %q", payload.ActiveSessionID)
	}
}

func TestResolvedTraceUsagePersistsAfterFinalizationAndSessionSwitch(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(oldAdapter)
	traceStore, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer traceStore.Close()
	adapter := rtpkg.NewTraceAdapter(traceStore)
	rtpkg.SetTraceAdapter(adapter)

	store, _ := session.NewStore(t.TempDir())
	const sessionA = "late-child-origin-a"
	const sessionB = "late-child-origin-b"
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{
		InitialSessionID: sessionA,
		TraceAdapter:     adapter,
		TraceStore:       traceStore,
	})
	// Building handlers repeatedly must not install duplicate observers.
	_ = server.handler()
	_ = server.handler()

	adapter.SetSession(sessionA)
	rtpkg.RecordUserMessage(sessionA, "start background work")
	_, originA := rtpkg.BindTraceTurn(context.Background(), sessionA)
	adapter.OnEvent(agent.Event{
		Kind: agent.EventTokens, TraceInvocationID: originA.InvocationID,
		InputTokens: 100, OutputTokens: 10, CacheCreationInputTokens: 20, CacheReadInputTokens: 80,
	})
	started := time.Date(2026, time.August, 27, 16, 0, 0, 0, time.UTC)
	if _, err := store.ReconcileMessageMetric(sessionA, session.MessageMetric{
		Turn: originA.Turn, StartedAt: started, CompletedAt: started.Add(2 * time.Second), DurationMS: 2_000,
		InputTokens: 100, OutputTokens: 10, CacheCreateTokens: 20, CacheReadTokens: 80,
	}); err != nil {
		t.Fatal(err)
	}

	adapter.SetSession(sessionB)
	// The old root invocation remains immutable even though B is now active.
	adapter.OnEvent(agent.Event{
		Kind: agent.EventTokens, TraceInvocationID: originA.InvocationID,
		InputTokens: 200, OutputTokens: 20, CacheCreationInputTokens: 20, CacheReadInputTokens: 160,
	})
	wantA := session.CostSnapshot{InputTokens: 300, OutputTokens: 30, CacheCreateTokens: 40, CacheReadTokens: 240}
	if got, ok, err := store.ReadCost(sessionA); err != nil || !ok || got != wantA {
		t.Fatalf("late A cost = %+v ok=%v err=%v, want %+v", got, ok, err, wantA)
	}
	if got, ok, err := store.ReadCost(sessionB); err != nil || ok || got != (session.CostSnapshot{}) {
		t.Fatalf("late A usage leaked into B: %+v ok=%v err=%v", got, ok, err)
	}
	metrics, err := store.ReadMessageMetrics(sessionA)
	if err != nil || len(metrics) != 1 {
		t.Fatalf("A metrics = %+v err=%v", metrics, err)
	}
	if got := metrics[0]; got.Turn != originA.Turn || got.OutputTokens != 30 || got.InputTokens != 300 ||
		!got.StartedAt.Equal(started) || got.DurationMS != 2_000 {
		t.Fatalf("late reconciled metric = %+v", got)
	}
}

func TestNewServerBootstrapsLegacyTraceWithoutDoubleCountAndRepairsMetric(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(oldAdapter)
	traceStore, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer traceStore.Close()
	const sessionID = "restart-trace-bootstrap"
	for _, ev := range []session.TraceEvent{
		{SessionID: sessionID, Turn: 1, Kind: "user", Text: "legacy"},
		{SessionID: sessionID, Turn: 1, Kind: "tokens", Text: "input=100 output=10 cache_write=20 cache_read=80"},
	} {
		if err := traceStore.Append(&ev); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := session.NewStore(t.TempDir())
	want := session.CostSnapshot{InputTokens: 100, OutputTokens: 10, CacheCreateTokens: 20, CacheReadTokens: 80}
	if err := store.WriteCost(sessionID, want); err != nil {
		t.Fatal(err)
	}
	adapter := rtpkg.NewTraceAdapter(traceStore)
	rtpkg.SetTraceAdapter(adapter)
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{
		InitialSessionID: sessionID, TraceAdapter: adapter, TraceStore: traceStore,
	})
	if got, ok, err := store.ReadCost(sessionID); err != nil || !ok || got != want {
		t.Fatalf("restart bootstrap duplicated cost: %+v ok=%v err=%v", got, ok, err)
	}

	// Model a process stop after cost-ledger commit but before metric rename.
	metricPath := filepath.Join(store.Dir, sessionID+".message-metrics.jsonl")
	if err := os.Remove(metricPath); err != nil {
		t.Fatal(err)
	}
	restartedAdapter := rtpkg.NewTraceAdapter(traceStore)
	rtpkg.SetTraceAdapter(restartedAdapter)
	server = NewServer("127.0.0.1:0", nil, store, RuntimeBindings{
		InitialSessionID: sessionID, TraceAdapter: restartedAdapter, TraceStore: traceStore,
	})
	if got, ok, err := store.ReadCost(sessionID); err != nil || !ok || got != want {
		t.Fatalf("restart repair double-counted cost: %+v ok=%v err=%v", got, ok, err)
	}
	metrics, err := store.ReadMessageMetrics(sessionID)
	if err != nil || len(metrics) != 1 || metrics[0].Turn != 1 || metrics[0].OutputTokens != 10 {
		t.Fatalf("restart did not repair partial metric: %+v err=%v", metrics, err)
	}

	// Partial usage is durable but not a completed message footer.
	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/trace?sessionId="+sessionID, nil))
	var payload struct {
		TurnMetrics []traceTurnMetricView `json:"turnMetrics"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.TurnMetrics) != 0 {
		t.Fatalf("partial metric exposed a fake footer: %+v", payload.TurnMetrics)
	}
}
