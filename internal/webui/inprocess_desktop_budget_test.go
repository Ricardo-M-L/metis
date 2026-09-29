package webui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type imageBudgetProbeProvider struct {
	activationTestProvider
	entered chan context.Context
}

func (p *imageBudgetProbeProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	select {
	case p.entered <- ctx:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestImageTurnUsesSharedDesktopExecutionPool(t *testing.T) {
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", "") // The Desktop server uses context, not process env.
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "image-budget"
	workDir, slotDir := t.TempDir(), t.TempDir()
	if err := store.WriteHeaderFull(session.Header{
		ID: sessionID, Provider: "wire", Model: "vision-unknown", System: "system",
		WorkDir: workDir, Mode: string(permission.ModeFullAccess), Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	provider := &imageBudgetProbeProvider{
		activationTestProvider: activationTestProvider{name: "wire", model: "vision-unknown"},
		entered:                make(chan context.Context, 1),
	}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeFullAccess), nil, "system", 2)
	loop.Model = provider.ModelID()
	server := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: sessionID, ProviderName: provider.Name(),
		IsolatedRunner: &blockingIsolatedRunner{started: make(chan IsolatedTurnRequest, 1), release: make(chan struct{})},
		IsolatedTurns: &IsolatedTurnOptions{
			Executable: "/ignored-in-test", MaxParallel: 2, MaxConfigurableParallelism: MaxDesktopRootTurnParallelism,
			SubagentSlotDir: slotDir, MaxTotalAgentSlots: 1, MaxSubagentsPerRoot: 1,
		},
	})
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(
			`{"sessionId":"image-budget","input":"describe","images":[{"mediaType":"image/png","data":"aGVsbG8="}]}`)).WithContext(requestCtx)
		server.handler().ServeHTTP(rr, request)
		response <- rr
	}()
	var executionCtx context.Context
	select {
	case executionCtx = <-provider.entered:
	case rr := <-response:
		t.Fatalf("image turn ended before provider execution: %d %s", rr.Code, rr.Body.String())
	case <-time.After(3 * time.Second):
		t.Fatal("image turn did not reach in-process provider")
	}
	config, ok := agent.DesktopExecutionConfigFromContext(executionCtx)
	if !ok || config.SlotDir != slotDir || config.TotalAgentSlots != 1 || config.SubagentsPerRoot != 1 || !strings.HasPrefix(config.Owner, "inproc:image-budget:") {
		t.Fatalf("image turn Desktop context = %+v, found=%v", config, ok)
	}
	if idle, err := agent.DesktopSchedulerIdle(slotDir); err != nil || idle {
		t.Fatalf("image turn did not charge shared execution pool: idle=%v err=%v", idle, err)
	}
	other := agent.WithDesktopExecutionConfig(context.Background(), agent.DesktopExecutionConfig{
		SlotDir: slotDir, TotalAgentSlots: 1, SubagentsPerRoot: 1, Owner: "other-root",
	})
	waitCtx, cancelWait := context.WithTimeout(other, 100*time.Millisecond)
	defer cancelWait()
	if release, err := agent.AcquireDesktopSubagentSlot(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("second execution entered occupied image-turn pool: %v", err)
	}
	cancelRequest()
	select {
	case rr := <-response:
		if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"stopped":true`)) {
			t.Fatalf("cancel image turn = %d %s", rr.Code, rr.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled image turn did not return")
	}
	if idle, err := agent.DesktopSchedulerIdle(slotDir); err != nil || !idle {
		t.Fatalf("image turn did not release execution pool: idle=%v err=%v", idle, err)
	}
}

type continuationBudgetProbeProvider struct {
	*backgroundContinuationProvider
	contexts chan context.Context
}

func (p *continuationBudgetProbeProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	p.contexts <- ctx
	return p.backgroundContinuationProvider.Stream(ctx, req)
}

func TestInProcessBackgroundContinuationKeepsDesktopBudget(t *testing.T) {
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", "")
	fixture := newBackgroundContinuationFixture(t)
	probe := &continuationBudgetProbeProvider{
		backgroundContinuationProvider: fixture.provider,
		contexts:                       make(chan context.Context, 2),
	}
	fixture.server.loop.Provider = probe
	fixture.server.desktopAgentSlotDir = t.TempDir()
	fixture.server.maxTotalAgentSlots = 2
	fixture.server.maxSubagentsPerRoot = 1
	sessionID := fixture.prompt(t)
	first := <-probe.contexts
	firstConfig, ok := agent.DesktopExecutionConfigFromContext(first)
	if !ok || firstConfig.TotalAgentSlots != 2 || firstConfig.SubagentsPerRoot != 1 || firstConfig.Owner == "" {
		t.Fatalf("initial in-process context = %+v, found=%v", firstConfig, ok)
	}
	if err := fixture.release.Close(); err != nil {
		t.Fatal(err)
	}
	var continuation context.Context
	select {
	case continuation = <-probe.contexts:
	case <-time.After(3 * time.Second):
		t.Fatal("background completion did not resume provider")
	}
	continuedConfig, ok := agent.DesktopExecutionConfigFromContext(continuation)
	if !ok || continuedConfig != firstConfig {
		t.Fatalf("continuation budget = %+v, found=%v; initial = %+v", continuedConfig, ok, firstConfig)
	}
	waitBackgroundCondition(t, "completed continuation", func() bool {
		hdr, _, err := fixture.store.Load(sessionID)
		return err == nil && hdr != nil && hdr.Status == "completed"
	})
}
