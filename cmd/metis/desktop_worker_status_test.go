package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type desktopStatusCapabilitiesProvider struct {
	llm.Provider
	capabilities llm.ContextCapabilities
}

func (*desktopStatusCapabilitiesProvider) Name() string            { return "metadata-status" }
func (*desktopStatusCapabilitiesProvider) ModelID() string         { return "brand-new-model" }
func (p *desktopStatusCapabilitiesProvider) MaxContextTokens() int { return p.capabilities.Window }
func (p *desktopStatusCapabilitiesProvider) ContextCapabilities() llm.ContextCapabilities {
	return p.capabilities
}
func (*desktopStatusCapabilitiesProvider) RefreshContextCapabilities(context.Context) error {
	panic("status must not refresh network metadata")
}

func TestDesktopWorkerStatusUpdatesCachedCapabilitiesWithoutModelOrHTTPCall(t *testing.T) {
	p := &desktopStatusCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200000, Source: "fallback"}}
	loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = p.ModelID()
	loop.AppendUser("worker-owned context")
	sampler := desktopWorkerStatusSampler{loop: loop, known: make(map[string]*agent.Teammate)}
	if first := sampler.snapshot(false); first.Context == nil || first.Context.Window != 200000 {
		t.Fatalf("initial context=%+v", first.Context)
	}
	p.capabilities = llm.ContextCapabilities{Window: 272000, DefaultWindow: 272000, MaxWindow: 872000, MaxInputTokens: 144000, MaxOutputTokens: 128000, Source: "endpoint_pi"}
	latest := sampler.snapshot(false)
	if latest.Context == nil || latest.Context.Used <= 0 || latest.Context.Window != 272000 || latest.Context.DefaultWindow != 272000 || latest.Context.MaxWindow != 872000 || latest.Context.MaxInputTokens != 144000 || latest.Context.MaxOutputTokens != 128000 || latest.Context.Source != "endpoint_pi" {
		t.Fatalf("metadata lost or bound to maximum instead of active: %+v", latest.Context)
	}
	p.capabilities = llm.ContextCapabilities{Window: 1050000, DefaultWindow: 272000, ProfileDefaultWindow: 272000, ProfileMaxWindow: 872000, MaxWindow: 1050000, MaxInputTokens: 922000, MaxOutputTokens: 128000, Source: "catalog_profile"}
	latest = sampler.snapshot(false)
	if latest.Context == nil || latest.Context.Window != 1050000 || latest.Context.DefaultWindow != 272000 || latest.Context.ProfileDefaultWindow != 272000 || latest.Context.ProfileMaxWindow != 872000 || latest.Context.MaxWindow != 1050000 || latest.Context.MaxInputTokens != 922000 || latest.Context.Source != "catalog_profile" {
		t.Fatalf("full capacity and client profile conflated in worker sample: %+v", latest.Context)
	}
	sampler.loop = nil
	if final := sampler.snapshot(true); final.Context == nil || *final.Context != *latest.Context {
		t.Fatalf("final capabilities lost: %+v", final.Context)
	}
}

func TestDesktopWorkerStatusPreservesModelBudgetProvenanceAndInputOnlyUnknownTotal(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capabilities llm.ContextCapabilities
		want         desktopipc.ContextPressure
	}{
		{
			name: "manual budget constrained by route while model remains full",
			capabilities: llm.ContextCapabilities{Window: 128000, WindowKind: llm.ContextWindowTotal,
				DefaultWindow: 128000, MaxWindow: 128000, MaxInputTokens: 96000, MaxOutputTokens: 32000,
				ProfileDefaultWindow: 500000, ProfileMaxWindow: 872000, Source: "configured_clamped",
				ModelWindow: 1000000, ModelMaxInputTokens: 900000, ModelMaxOutputTokens: 100000, ModelSource: "catalog_model",
				RequestedWindow: 800000, LimitSource: "endpoint_pi", BudgetSource: llm.ContextBudgetPerModel,
				ProviderID: "route-provider", Model: "brand-new-model", RouteKey: "route-account-model-key"},
			want: desktopipc.ContextPressure{Window: 128000, WindowKind: "total",
				DefaultWindow: 128000, MaxWindow: 128000, MaxInputTokens: 96000, MaxOutputTokens: 32000,
				ProfileDefaultWindow: 500000, ProfileMaxWindow: 872000, Source: "configured_clamped",
				ModelWindow: 1000000, ModelMaxInputTokens: 900000, ModelMaxOutputTokens: 100000, ModelSource: "catalog_model",
				RequestedWindow: 800000, LimitSource: "endpoint_pi", BudgetSource: "per_model",
				ProviderID: "route-provider", Model: "brand-new-model", RouteKey: "route-account-model-key"},
		},
		{
			name: "input only does not claim total model capacity",
			capabilities: llm.ContextCapabilities{Window: 160000, WindowKind: llm.ContextWindowInput,
				MaxInputTokens: 160000, MaxOutputTokens: 64000, Source: "endpoint_input_limit", BudgetSource: llm.ContextBudgetAutomatic,
				ProviderID: "input-route", Model: "brand-new-model", RouteKey: "input-account-model-key"},
			want: desktopipc.ContextPressure{Window: 160000, WindowKind: "input",
				MaxInputTokens: 160000, MaxOutputTokens: 64000, Source: "endpoint_input_limit", BudgetSource: "automatic",
				ProviderID: "input-route", Model: "brand-new-model", RouteKey: "input-account-model-key"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &desktopStatusCapabilitiesProvider{capabilities: tc.capabilities}
			loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
			loop.Model = p.ModelID()
			loop.AppendUser("owned worker transcript")
			sampler := desktopWorkerStatusSampler{loop: loop, known: make(map[string]*agent.Teammate)}
			got := sampler.snapshot(false)
			if got.Context == nil || got.Context.Used <= 0 {
				t.Fatalf("worker context unavailable: %+v", got.Context)
			}
			want := tc.want
			want.Used = got.Context.Used
			if *got.Context != want {
				t.Fatalf("worker lost budget provenance: got=%+v want=%+v", got.Context, want)
			}
			sampler.loop = nil
			final := sampler.snapshot(true)
			if final.Context == nil || *final.Context != want {
				t.Fatalf("cleanup lost exact last capability tuple: %+v", final.Context)
			}
		})
	}
}

func TestDesktopWorkerStatusEmitsCapabilityOnlyChanges(t *testing.T) {
	a := desktopipc.ContextPressure{Used: 30488, Window: 800000, ModelWindow: 1050000,
		ProfileDefaultWindow: 500000, ProfileMaxWindow: 872000, ModelSource: "catalog_model", BudgetSource: "per_model",
		ProviderID: "bound-provider", Model: "exact-model", RouteKey: "account-a-route"}
	for _, tc := range []struct {
		name   string
		change func(*desktopipc.ContextPressure)
	}{
		{"model evidence", func(p *desktopipc.ContextPressure) { p.ModelSource = "endpoint_models" }},
		{"full model capacity", func(p *desktopipc.ContextPressure) { p.ModelWindow = 2000000 }},
		{"requested budget", func(p *desktopipc.ContextPressure) { p.RequestedWindow = 1200000 }},
		{"account route owner", func(p *desktopipc.ContextPressure) { p.RouteKey = "account-b-route" }},
		{"input interpretation", func(p *desktopipc.ContextPressure) { p.WindowKind = "input" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := a
			tc.change(&b)
			if equivalentDesktopWorkerStatus(desktopipc.Status{Context: &a}, desktopipc.Status{Context: &b}) {
				t.Fatal("new capacity metadata was suppressed because used/window were unchanged")
			}
		})
	}
}

func TestDesktopWorkerStatusClearsPreviousBudgetWhenActiveAutoCapacityUnknown(t *testing.T) {
	p := &desktopStatusCapabilitiesProvider{capabilities: llm.ContextCapabilities{
		Window: 100000, WindowKind: "total", RequestedWindow: 100000, Source: "configured",
		BudgetSource: "per_model", ProviderID: "unknown-cloud-route", Model: "brand-new-model", RouteKey: "unknown-account-route",
	}}
	loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = p.ModelID()
	loop.AppendUser("still active owned transcript")
	sampler := desktopWorkerStatusSampler{loop: loop, known: make(map[string]*agent.Teammate)}
	before := sampler.snapshot(false)
	if before.Context == nil || before.Context.Window != 100000 || before.Context.BudgetSource != "per_model" {
		t.Fatalf("initial manual budget unavailable: %+v", before.Context)
	}
	// Explicit Auto can have no discovered total. That is a current reading,
	// not evidence that the previous manual budget remains active.
	p.capabilities = llm.ContextCapabilities{WindowKind: "total", Source: "unknown",
		BudgetSource: "automatic", ProviderID: "unknown-cloud-route", Model: "brand-new-model", RouteKey: "unknown-account-route"}
	after := sampler.snapshot(false)
	if after.Context == nil || after.Context.Window != 0 || after.Context.ModelWindow != 0 || after.Context.RequestedWindow != 0 || after.Context.Source != "unknown" || after.Context.BudgetSource != "automatic" || after.Context.RouteKey != "unknown-account-route" {
		t.Fatalf("active unknown Auto retained obsolete manual capacity: %+v", after.Context)
	}
	sampler.loop = nil
	final := sampler.snapshot(true)
	if final.Context == nil || *final.Context != *after.Context {
		t.Fatalf("cleanup failed to preserve latest unknown Auto tuple: %+v", final.Context)
	}
}

func TestDesktopWorkerStatusPreservesFinalSubagentAfterCleanup(t *testing.T) {
	input, parent := io.Pipe()
	defer parent.Close()
	var output bytes.Buffer
	bridge := newDesktopWorkerBridge(context.Background(), input, &output)
	defer bridge.Close()
	roster := agent.NewRoster(4)
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.ContextWindow = 128_000
	loop.AppendUser(strings.Repeat("live worker prompt ", 80))
	stop := bridge.startStatus(roster, nil, loop)
	teammate := &agent.Teammate{Name: "review", AgentID: "agt-status", Started: time.Now(), Status: agent.StatusRunning}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	teammate.AppendText("first streamed output")
	teammate.Finish(agent.StatusCompleted, "final reply", nil, "end_turn")
	roster.UnregisterTeammate(teammate)
	stop(func() {
		if err := roster.CancelAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	decoder := desktopipc.NewDecoder(&output)
	var latest *desktopipc.Status
	frames := 0
	for {
		message, err := decoder.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		latest = message.Status
		frames++
		if frames == 1 && (latest == nil || latest.Context == nil || latest.Context.Used <= 0) {
			t.Fatalf("running status lacked worker pressure: %+v", latest)
		}
	}
	if frames < 2 || latest == nil || latest.SubAgents != 0 || len(latest.Agents) != 1 {
		t.Fatalf("final status lost child: %+v", latest)
	}
	if latest.Context == nil || latest.Context.Used <= 0 || latest.Context.Window != 128_000 {
		t.Fatalf("final status lost worker context pressure: %+v", latest.Context)
	}
	child := latest.Agents[0]
	if child.Status != "completed" || child.Output != "first streamed output" || child.Result != "final reply" {
		t.Fatalf("bad final child: %+v", child)
	}
}

func TestDesktopWorkerStatusSamplesLoopPressureAndKeepsFinalReading(t *testing.T) {
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.ContextWindow = 128_000
	loop.AppendUser(strings.Repeat("worker context ", 100))
	sampler := desktopWorkerStatusSampler{loop: loop, known: make(map[string]*agent.Teammate)}
	first := sampler.snapshot(false)
	if first.Context == nil || first.Context.Used <= 0 || first.Context.Window != 128_000 {
		t.Fatalf("running worker has no real pressure: %+v", first.Context)
	}
	loop.AppendUser(strings.Repeat("new turn ", 200))
	latest := sampler.snapshot(false)
	if latest.Context == nil || latest.Context.Used <= first.Context.Used {
		t.Fatalf("worker pressure did not advance: first=%+v latest=%+v", first.Context, latest.Context)
	}
	// Cleanup can release the runtime before the final IPC frame. That frame
	// must preserve the last sample instead of publishing a fabricated zero.
	sampler.loop = nil
	final := sampler.snapshot(true)
	if final.Context == nil || *final.Context != *latest.Context {
		t.Fatalf("final worker pressure was lost: latest=%+v final=%+v", latest.Context, final.Context)
	}
}

func TestDesktopWorkerStatusTruncatesLongChildText(t *testing.T) {
	text := strings.Repeat("中", desktopWorkerStatusTextLimit) + "TAIL"
	got, truncated := desktopWorkerStatusText(text)
	if !truncated || len([]rune(got)) > desktopWorkerStatusTextLimit || !strings.HasSuffix(got, "TAIL") {
		t.Fatal("long output was not safely head/tail bounded")
	}
}

func TestDesktopWorkerStatusSeparatesLifecycleFromHeldExecution(t *testing.T) {
	config := agent.DesktopExecutionConfig{SlotDir: t.TempDir(), TotalAgentSlots: 1, SubagentsPerRoot: 1, Owner: "status-test"}
	ctx := agent.WithDesktopExecutionConfig(context.Background(), config)
	roster := agent.NewRoster(1, 1)
	child := &agent.Teammate{Name: "worker"}
	if err := agent.RegisterDesktopTeammate(ctx, roster, child, false); err != nil {
		t.Fatal(err)
	}
	defer roster.UnregisterTeammate(child)
	sampler := desktopWorkerStatusSampler{roster: roster, known: make(map[string]*agent.Teammate)}
	queued := sampler.snapshot(false).Agents[0]
	if queued.Status != "queued" || queued.ExecutionPhase != "queued" || queued.HoldsExecutionSlot {
		t.Fatalf("queued status=%+v", queued)
	}
	if _, err := agent.AcquireDesktopTeammateExecution(ctx, roster, child); err != nil {
		t.Fatal(err)
	}
	running := sampler.snapshot(false).Agents[0]
	if running.Status != "running" || running.ExecutionPhase != "executing" || !running.HoldsExecutionSlot {
		t.Fatalf("running status=%+v", running)
	}
	child.Finish(agent.StatusCompleted, "done", nil, "end_turn")
	roster.UnregisterTeammate(child)
	finished := sampler.snapshot(true).Agents[0]
	if finished.Status != "completed" || finished.ExecutionPhase != "finished" || finished.HoldsExecutionSlot {
		t.Fatalf("finished status=%+v", finished)
	}
}

func TestDesktopWorkerStatusCarriesPreparedContextBreakdown(t *testing.T) {
	p := &desktopStatusCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 1000000, Model: "brand-new-model"}}
	loop := agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system prompt", 2)
	loop.Model = p.ModelID()
	loop.AppendUser("worker owned messages")
	loop.SyncContextCapabilities()
	loop.EstimateRequestContextTokens([]llm.ToolSpec{{Name: "Read", Description: "read a file"}})
	want := loop.ContextUsageSnapshot()
	sampler := desktopWorkerStatusSampler{loop: loop, known: make(map[string]*agent.Teammate)}
	got := sampler.snapshot(false)
	if got.Context == nil || got.Context.Breakdown == nil || !got.Context.Breakdown.Valid() || got.Context.Used != want.Used || got.Context.Model != want.Capabilities.Model || got.Context.Window != want.Capabilities.Window {
		t.Fatalf("missing coherent context: %+v", got.Context)
	}
	parts := got.Context.Breakdown
	if parts.SystemTokens != want.Breakdown.SystemTokens || parts.ToolTokens != want.Breakdown.ToolTokens || parts.MessageTokens != want.Breakdown.MessageTokens {
		t.Fatalf("parts changed across worker IPC: got=%+v want=%+v", parts, want.Breakdown)
	}
	final := sampler.snapshot(true)
	if final.Context != got.Context {
		t.Fatal("cleanup did not retain final complete status")
	}
	loop.ResetSession(nil)
	reset := sampler.snapshot(false)
	if reset.Context == nil || reset.Context.Breakdown != nil {
		t.Fatalf("worker retained prior session categories: %+v", reset.Context)
	}
}
