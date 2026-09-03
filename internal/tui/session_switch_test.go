package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// Byte-for-byte fixture of the startup-only Plan section persisted by Metis
// <=0.2.8. Keep this local to the integration test: runtime owns the migration
// constant, while the TUI test should exercise the public cleanup path exactly
// as an old on-disk Header.System supplies it.
const legacyPlanOverlayFixture = "# Plan mode — read-only exploration only\n\n" +
	"You are in plan mode. The permission gate will deny every tool that\n" +
	"mutates state: Edit, Write, NotebookEdit, Bash (any non-read-only\n" +
	"command), MessageTeammate to send PRs, etc. Don't try to call them;\n" +
	"the user will see the denial and you'll have to retry. Allowed tools:\n" +
	"Read, LS, Glob, Grep, WebFetch, and any sub-agent (Agent / Fork) you\n" +
	"spawn in read-only mode.\n\n" +
	"Workflow for plan mode:\n\n" +
	"  1. Explore. Use Read, Grep, Glob freely to understand the codebase.\n" +
	"     Spawn Agent sub-agents for deep dives that would otherwise burn\n" +
	"     your main context.\n" +
	"  2. Synthesize. Decide what would need to change, in what order,\n" +
	"     with what trade-offs.\n" +
	"  3. Produce a plan. Write it out as the final assistant message:\n" +
	"     a numbered list of concrete steps, the files each step touches,\n" +
	"     and the risks / open questions. Use TodoWrite to track the plan\n" +
	"     items so the user sees the checklist in the UI.\n" +
	"  4. STOP and wait. Do NOT start implementing. The user reviews the\n" +
	"     plan, then exits plan mode (`/auto` or `/bypass`) when ready —\n" +
	"     that's the signal to execute. If you implement now, you'll just\n" +
	"     hit denials.\n\n" +
	"Keep the plan compact. Bullet points, not essays. Skip \"next steps\"\n" +
	"and \"future improvements\" sections unless the user asked. The plan\n" +
	"should be a thing the user can scan in 30 seconds and the next-turn\n" +
	"you can execute step-by-step without rereading."

func newSessionSwitchModel(t *testing.T, mode permission.Mode) (*Model, *session.Store) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.WriteHeaderFull(session.Header{ID: "source", Model: "test-model", System: "base-system", Mode: string(mode), Title: "source title"}); err != nil {
		t.Fatalf("source header: %v", err)
	}
	gate := permission.New(mode)
	loop := agent.NewLoop(nil, tools.NewRegistry(), gate, nil, "base-system", 5)
	loop.Model = "test-model"
	m := NewModel(context.Background(), loop, nil, nil, store, "source", gate, "test-model", "", "", &config.Config{})
	m.ext.FreshPermissionMode = permission.ModeAsk
	m.ext.TrustSessionPermissions = true
	return m, store
}

func TestActivateSessionFiltersPermissionsFromUntrustedStore(t *testing.T) {
	tests := []struct {
		name        string
		mode        permission.Mode
		prePlan     string
		wantMode    permission.Mode
		wantPrePlan string
	}{
		{
			name: "direct full access",
			mode: permission.ModeFullAccess, wantMode: permission.ModeAsk,
		},
		{
			name: "plan full access lineage",
			mode: permission.ModePlan, prePlan: string(permission.ModeFullAccess),
			wantMode: permission.ModePlan, wantPrePlan: string(permission.ModeAsk),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			m.ext.TrustSessionPermissions = false
			target := &session.Header{
				ID: "untrusted-target", Model: "test-model", System: "target-system",
				Mode: string(tt.mode), PrePlanMode: tt.prePlan,
				AlwaysAllow: []session.SavedRule{{
					Tool: "Bash", Verb: int(permission.DecisionAllow), Source: "interactive",
				}},
			}
			if err := store.WriteHeaderFull(*target); err != nil {
				t.Fatal(err)
			}

			if err := m.activateSession(target.ID, target, nil, true); err != nil {
				t.Fatal(err)
			}
			if got := m.gate.Mode(); got != tt.wantMode {
				t.Fatalf("restored mode = %q, want %q", got, tt.wantMode)
			}
			if got := m.loop.PrePlanMode(); got != tt.wantPrePlan {
				t.Fatalf("restored pre-plan mode = %q, want %q", got, tt.wantPrePlan)
			}
			if rules := m.gate.Snapshot(); len(rules) != 0 {
				t.Fatalf("untrusted session rules restored: %+v", rules)
			}
		})
	}
}

func TestActiveTurnRejectsStaleSessionScreenResults(t *testing.T) {
	tests := []struct {
		name  string
		build func() screen.Screen
	}{
		{
			name: "sessions picker resume",
			build: func() screen.Screen {
				picker := screen.NewPickerScreen("/sessions", "", []screen.PickerItem{{Key: "target"}})
				picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				return picker
			},
		},
		{
			name: "resume screen resume",
			build: func() screen.Screen {
				resume := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
				resume.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				return resume
			},
		},
		{
			name: "resume screen fork",
			build: func() screen.Screen {
				resume := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
				resume.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
				return resume
			},
		},
		{
			name: "resume screen fresh",
			build: func() screen.Screen {
				resume := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
				resume.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
				return resume
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeFullAccess)
			sourceHistory := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "source prompt"}}}}
			m.loop.Restore(sourceHistory)
			m.loop.SetPrePlanMode(string(permission.ModeDefault))
			m.gate.AppendRules(permission.Rule{Tool: "Read", Verb: permission.DecisionAllow, Source: "interactive"})
			if err := store.WriteHeaderFull(session.Header{ID: "target", Model: "test-model", System: "target-system", Mode: string(permission.ModeAsk)}); err != nil {
				t.Fatal(err)
			}

			beforeID := m.sessionID
			beforeMode := m.gate.Mode()
			beforeRules := m.gate.Snapshot()
			beforeHistory := m.loop.History()
			beforePrePlan := m.loop.PrePlanMode()
			beforeEntries, err := store.List(0)
			if err != nil {
				t.Fatal(err)
			}
			m.turnActive = true // Picker opened while idle; the turn began before selection landed.

			m.applyScreenResult(tt.build())

			if m.sessionID != beforeID {
				t.Fatalf("stale selection changed session: got %q, want %q", m.sessionID, beforeID)
			}
			if m.gate.Mode() != beforeMode || !reflect.DeepEqual(m.gate.Snapshot(), beforeRules) {
				t.Fatalf("stale selection changed gate: mode=%q rules=%+v", m.gate.Mode(), m.gate.Snapshot())
			}
			if !reflect.DeepEqual(m.loop.History(), beforeHistory) || m.loop.PrePlanMode() != beforePrePlan {
				t.Fatalf("stale selection changed loop: history=%+v prePlan=%q", m.loop.History(), m.loop.PrePlanMode())
			}
			afterEntries, err := store.List(0)
			if err != nil {
				t.Fatal(err)
			}
			if len(afterEntries) != len(beforeEntries) {
				t.Fatalf("stale selection created a session: before=%d after=%d", len(beforeEntries), len(afterEntries))
			}
			if len(m.messages) == 0 || !strings.Contains(m.messages[len(m.messages)-1].Content, "stop or cancel") {
				t.Fatalf("stale selection did not explain refusal: %+v", m.messages)
			}
		})
	}
}

func TestActivateSessionRebindsAllSessionScopedState(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeBypass)
	m.gate.AppendRules(
		permission.Rule{Tool: "Bash", Verb: permission.DecisionDeny, Source: "policy:deny"},
		permission.Rule{Tool: "Edit", Verb: permission.DecisionAllow, Source: "interactive"},
	)
	target := "target"
	hdr := session.Header{
		ID: target, Model: "test-model", System: "target-system",
		Mode: "acceptEdits", Title: "target title",
		AlwaysAllow: []session.SavedRule{{
			Tool: "Read", Verb: int(permission.DecisionAllow), Source: "config:allow",
		}},
	}
	if err := store.WriteHeaderFull(hdr); err != nil {
		t.Fatalf("target header: %v", err)
	}
	msg := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "target prompt"}}}
	if err := store.AppendMessage(target, msg); err != nil {
		t.Fatalf("target message: %v", err)
	}
	if err := store.WriteCost(target, session.CostSnapshot{InputTokens: 41, OutputTokens: 7, CacheReadTokens: 3}); err != nil {
		t.Fatalf("target cost: %v", err)
	}
	m.totalTokens.add(900, 800, 700, 600)
	m.histAll = []string{"source prompt"}
	m.histDirectIdx = 0
	rebound := ""
	boundaryCalls := 0
	m.ext.SessionSwitch = func(id string) { rebound = id }
	m.ext.SessionBoundary = func() { boundaryCalls++ }

	if err := m.activateSession(target, &hdr, []llm.Message{msg}, true); err != nil {
		t.Fatalf("activateSession: %v", err)
	}
	if m.sessionID != target || rebound != target {
		t.Fatalf("session routing not rebound: model=%q hook=%q", m.sessionID, rebound)
	}
	if boundaryCalls != 1 {
		t.Fatalf("successful activation called session boundary %d times, want 1", boundaryCalls)
	}
	if got := m.gate.Mode(); got != permission.ModeAcceptEdits {
		t.Errorf("mode = %q, want acceptEdits", got)
	}
	rules := m.gate.Snapshot()
	if len(rules) != 2 {
		t.Fatalf("rules = %+v, want policy + target approval", rules)
	}
	for _, rule := range rules {
		if rule.Tool == "Edit" {
			t.Errorf("source interactive grant leaked: %+v", rules)
		}
	}
	if rules[1].Tool != "Read" || !strings.HasPrefix(rules[1].Source, "session:") {
		t.Errorf("target approval not restored with session lifetime: %+v", rules)
	}
	if m.loop.System != "target-system" || m.sessionTitle != "target title" {
		t.Errorf("header state not restored: system=%q title=%q", m.loop.System, m.sessionTitle)
	}
	if got := m.loop.History(); len(got) != 1 || got[0].Content[0].Text != "target prompt" {
		t.Errorf("history not restored: %+v", got)
	}
	in, out, create, read := m.totalTokens.Snapshot()
	if in != 41 || out != 7 || create != 0 || read != 3 {
		t.Errorf("cost = (%d,%d,%d,%d), want (41,7,0,3)", in, out, create, read)
	}
	if m.histAll != nil || m.histDirectIdx != -1 {
		t.Errorf("prompt history cache leaked: all=%v idx=%d", m.histAll, m.histDirectIdx)
	}

	// The TimingSink must now append to target.timing.jsonl, not source.
	m.loop.TimingSink("Read", 12*time.Millisecond, false)
	targetTiming, err := store.ReadTiming(target)
	if err != nil || len(targetTiming) != 1 || targetTiming[0].Tool != "Read" {
		t.Fatalf("target timing = %+v, err=%v", targetTiming, err)
	}
	sourceTiming, err := store.ReadTiming("source")
	if err != nil || len(sourceTiming) != 0 {
		t.Fatalf("source timing received destination event: %+v, err=%v", sourceTiming, err)
	}
}

func TestActivateSessionRestoresProviderSpecificContextWindow(t *testing.T) {
	t.Setenv("SESSION_ROUTE_KEY", "sk-test")
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.cfg.Provider.Custom = map[string]config.ProviderRaw{
		"route-small": {
			Transport: "openai_chat", APIKeyEnv: "SESSION_ROUTE_KEY",
			BaseURL: "http://127.0.0.1:1/v1", Model: "shared-model",
			ContextWindow: 100_000, MaxTokens: 4_096,
		},
		"route-large": {
			Transport: "openai_chat", APIKeyEnv: "SESSION_ROUTE_KEY",
			BaseURL: "http://127.0.0.1:1/v1", Model: "shared-model",
			ContextWindow: 240_000, MaxTokens: 4_096,
		},
	}

	sourceBuild, err := rtpkg.BuildProvider(m.cfg, "route-small", "shared-model")
	if err != nil {
		t.Fatal(err)
	}
	m.loop.RebindProviderRuntime(sourceBuild.Provider, sourceBuild.Model, sourceBuild.MaxOutputTokens, m.loop.System, m.loop.SystemSections)
	m.model = sourceBuild.Model
	m.providerName = "route-small"
	if err := store.WriteHeaderFull(session.Header{
		ID: "source", Provider: "route-small", Model: "shared-model",
		System: "base-system", Mode: string(permission.ModeAsk),
	}); err != nil {
		t.Fatal(err)
	}
	target := session.Header{
		ID: "provider-window-target", Provider: "route-large", Model: "shared-model",
		System: "base-system", Mode: string(permission.ModeAsk),
	}
	if err := store.WriteHeaderFull(target); err != nil {
		t.Fatal(err)
	}

	if err := m.activateSession(target.ID, &target, nil, true); err != nil {
		t.Fatalf("activate large-window session: %v", err)
	}
	if _, model, window := m.loop.ProviderModelSnapshot(); m.providerName != "route-large" || model != "shared-model" || window != 240_000 {
		t.Fatalf("large-window binding = provider %q model %q window %d", m.providerName, model, window)
	}

	sourceHeader, _, err := store.LoadHeader("source")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.activateSession("source", sourceHeader, nil, true); err != nil {
		t.Fatalf("reactivate small-window session: %v", err)
	}
	if _, model, window := m.loop.ProviderModelSnapshot(); m.providerName != "route-small" || model != "shared-model" || window != 100_000 {
		t.Fatalf("small-window binding = provider %q model %q window %d", m.providerName, model, window)
	}
}

func TestActivateSessionUsesGateModeAfterListenerDowngrade(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeDefault)
	target := session.Header{
		ID: "target-plan-downgrade", Model: "test-model", System: "target-system",
		Mode: string(permission.ModePlan), PrePlanMode: string(permission.ModeDefault),
	}
	if err := store.WriteHeaderFull(target); err != nil {
		t.Fatal(err)
	}
	m.gate.SetModeChangeListener(func(mode permission.Mode) {
		m.loop.SetPlanMode(mode == permission.ModePlan)
		if mode == permission.ModePlan {
			m.gate.SetMode(permission.ModeDontAsk)
		}
	})

	if err := m.activateSession(target.ID, &target, nil, true); err != nil {
		t.Fatal(err)
	}
	if m.gate.Mode() != permission.ModeDontAsk || m.loop.IsPlanMode() {
		t.Fatalf("restored permission state diverged: gate=%q plan=%v", m.gate.Mode(), m.loop.IsPlanMode())
	}
	if got := m.loop.PrePlanMode(); got != "" {
		t.Fatalf("failed-closed switch retained pre-plan lineage %q", got)
	}
}

func TestActivateSessionRemovesAndPersistsLegacyPlanOverlay(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	const id = "legacy-plan-resume"
	legacySystem := "base instructions\n\n" + legacyPlanOverlayFixture + "\n\n<env>live</env>"
	hdr := session.Header{ID: id, Model: "test-model", System: legacySystem, Mode: "default"}
	if err := store.WriteHeaderFull(hdr); err != nil {
		t.Fatal(err)
	}

	if err := m.activateSession(id, &hdr, nil, true); err != nil {
		t.Fatal(err)
	}
	want := "base instructions\n\n<env>live</env>"
	if m.loop.System != want {
		t.Fatalf("live resumed system = %q, want %q", m.loop.System, want)
	}
	persisted, _, err := store.LoadHeader(id)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.System != want {
		t.Fatalf("persisted resumed system = %q, want %q", persisted.System, want)
	}
}

func TestForkSessionDoesNotInheritLegacyPlanOverlay(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	const parent = "legacy-plan-parent"
	legacySystem := "parent base\n\n" + legacyPlanOverlayFixture + "\n\n<env>parent</env>"
	if err := store.WriteHeaderFull(session.Header{ID: parent, Model: "test-model", System: legacySystem}); err != nil {
		t.Fatal(err)
	}

	id, hdr, err := m.forkSession(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "parent base\n\n<env>parent</env>"
	if hdr.System != want {
		t.Fatalf("fork header system = %q, want %q", hdr.System, want)
	}
	persisted, _, err := store.LoadHeader(id)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.System != want {
		t.Fatalf("persisted fork system = %q, want %q", persisted.System, want)
	}
}

func TestActivateSessionClearsOnlySourceEphemeralCronJobs(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	cronSvc, err := agent.NewCronService(filepath.Join(t.TempDir(), "cron"))
	if err != nil {
		t.Fatal(err)
	}
	m.cronSvc = cronSvc
	ephemeral := &agent.CronJob{
		ID: "source-reminder", Prompt: "must not cross sessions", Enabled: true, Ephemeral: true,
		Schedule: agent.CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
	durable := &agent.CronJob{
		ID: "durable-reminder", Prompt: "global daemon job", Enabled: true,
		Schedule: agent.CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
	if err := cronSvc.Create(ephemeral); err != nil {
		t.Fatal(err)
	}
	if err := cronSvc.Create(durable); err != nil {
		t.Fatal(err)
	}

	target := session.Header{ID: "target", Model: "test-model", System: "base-system", Mode: "ask"}
	if err := store.WriteHeaderFull(target); err != nil {
		t.Fatal(err)
	}
	if err := m.activateSession(target.ID, &target, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := cronSvc.Get(ephemeral.ID); ok {
		t.Fatal("source session's ephemeral cron job survived activation")
	}
	if _, ok := cronSvc.Get(durable.ID); !ok {
		t.Fatal("session activation removed a durable cron job")
	}
}

func TestActivateSessionPreflightFailurePreservesEphemeralCronJobs(t *testing.T) {
	m, _ := newSessionSwitchModel(t, permission.ModeAsk)
	cronSvc, err := agent.NewCronService(filepath.Join(t.TempDir(), "cron"))
	if err != nil {
		t.Fatal(err)
	}
	m.cronSvc = cronSvc
	ephemeral := &agent.CronJob{
		ID: "source-reminder", Prompt: "keep on failed switch", Enabled: true, Ephemeral: true,
		Schedule: agent.CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
	if err := cronSvc.Create(ephemeral); err != nil {
		t.Fatal(err)
	}
	target := &session.Header{ID: "bad-target", Provider: "missing-profile", Model: "other-model"}
	if err := m.activateSession(target.ID, target, nil, true); err == nil {
		t.Fatal("activateSession unexpectedly passed provider preflight")
	}
	if _, ok := cronSvc.Get(ephemeral.ID); !ok {
		t.Fatal("failed activation cleared the still-active source session's cron job")
	}
}

func TestActivateSessionProviderFailureLeavesSourceFullyIntact(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeBypass)
	oldProvider := &switchTestProvider{id: "source-wire", maxCtx: 100_000}
	m.loop.Provider = oldProvider
	m.loop.Model = "source-model"
	m.model = "source-model"
	m.providerName = "anthropic"
	m.loop.System = "source-system"
	m.loop.SystemSections = []llm.SystemSection{{Name: "source", Body: "source section", Cache: true}}
	m.loop.Restore([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "source prompt"}}}})
	m.loop.SetPrePlanMode("source-pre-plan")
	m.loop.BypassNextCache = true
	m.sessionTitle = "source title"
	m.messages = []Message{{Role: "assistant", Content: "source visible message"}}
	m.histAll = []string{"source history"}
	m.histDirectIdx = 0
	m.totalTokens.add(11, 12, 13, 14)
	m.gate.AppendRules(permission.Rule{Tool: "Edit", Verb: permission.DecisionAllow, Source: "interactive"})
	sourceTimingCalls := 0
	m.loop.TimingSink = func(string, time.Duration, bool) { sourceTimingCalls++ }
	rebound := ""
	boundaryCalls := 0
	m.ext.SessionSwitch = func(id string) { rebound = id }
	m.ext.SessionBoundary = func() { boundaryCalls++ }

	target := session.Header{
		ID: "cross-provider-target", Provider: "missing-provider-profile", Model: "gpt-4o", System: "target-system",
		Mode: "deny", Title: "target title",
		AlwaysAllow: []session.SavedRule{{Tool: "Read", Verb: int(permission.DecisionAllow), Source: "interactive"}},
	}
	if err := store.WriteHeaderFull(target); err != nil {
		t.Fatal(err)
	}
	targetMessages := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "target prompt"}}}}

	err := m.activateSession(target.ID, &target, targetMessages, true)
	if err == nil || !strings.Contains(err.Error(), "provider/model preflight") {
		t.Fatalf("activateSession error = %v, want provider/model preflight failure", err)
	}
	if m.sessionID != "source" || rebound != "" {
		t.Fatalf("session routing changed on failure: id=%q rebound=%q", m.sessionID, rebound)
	}
	if boundaryCalls != 0 {
		t.Fatalf("failed preflight called session boundary %d times", boundaryCalls)
	}
	if m.model != "source-model" || m.providerName != "anthropic" || m.loop.Model != "source-model" || m.loop.Provider != oldProvider {
		t.Fatalf("model/provider changed on failure: model=%q profile=%q loopModel=%q provider=%T", m.model, m.providerName, m.loop.Model, m.loop.Provider)
	}
	if m.gate.Mode() != permission.ModeBypass {
		t.Fatalf("gate mode changed on failure: %q", m.gate.Mode())
	}
	rules := m.gate.Snapshot()
	if len(rules) != 1 || rules[0].Tool != "Edit" || rules[0].Source != "interactive" {
		t.Fatalf("gate rules changed on failure: %+v", rules)
	}
	if m.loop.System != "source-system" || len(m.loop.SystemSections) != 1 || m.loop.SystemSections[0].Name != "source" {
		t.Fatalf("system state changed on failure: system=%q sections=%+v", m.loop.System, m.loop.SystemSections)
	}
	if got := m.loop.History(); len(got) != 1 || got[0].Content[0].Text != "source prompt" {
		t.Fatalf("source transcript changed on failure: %+v", got)
	}
	if m.loop.PrePlanMode() != "source-pre-plan" || !m.loop.BypassNextCache {
		t.Fatalf("loop session state was reset on failure: prePlan=%q bypass=%v", m.loop.PrePlanMode(), m.loop.BypassNextCache)
	}
	if m.sessionTitle != "source title" || len(m.messages) != 1 || m.messages[0].Content != "source visible message" || len(m.histAll) != 1 || m.histDirectIdx != 0 {
		t.Fatalf("UI state changed on failure: title=%q messages=%+v history=%v idx=%d", m.sessionTitle, m.messages, m.histAll, m.histDirectIdx)
	}
	in, out, create, read := m.totalTokens.Snapshot()
	if in != 11 || out != 12 || create != 13 || read != 14 {
		t.Fatalf("token totals changed on failure: (%d,%d,%d,%d)", in, out, create, read)
	}
	m.loop.TimingSink("Read", time.Millisecond, false)
	if sourceTimingCalls != 1 {
		t.Fatalf("source timing sink was replaced: calls=%d", sourceTimingCalls)
	}

	// Simulate the normal shutdown persistence path. Because the live ID still
	// points at the source, the unavailable target's metadata must remain exact.
	m.persistActiveSessionState()
	gotTarget, _, err := store.LoadHeader(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget.Provider != target.Provider || gotTarget.Model != target.Model || gotTarget.System != target.System ||
		gotTarget.Mode != target.Mode || len(gotTarget.AlwaysAllow) != 1 || gotTarget.AlwaysAllow[0].Tool != "Read" {
		t.Fatalf("failed activation overwrote target header: got=%+v want=%+v", gotTarget, target)
	}
}

func TestSessionPickerProviderFailureWarnsWithoutSuccess(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	oldProvider := &switchTestProvider{id: "source-wire", maxCtx: 100_000}
	m.loop.Provider = oldProvider
	m.loop.Model = "source-model"
	m.model = "source-model"
	m.providerName = "anthropic"
	m.messages = []Message{{Role: "assistant", Content: "source message"}}
	const targetID = "picker-cross-provider"
	if err := store.WriteHeaderFull(session.Header{
		ID: targetID, Provider: "missing-provider-profile", Model: "gpt-4o", System: "target-system", Mode: "deny",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(targetID, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "target"}}}); err != nil {
		t.Fatal(err)
	}

	picker := screen.NewPickerScreen("/sessions", "", []screen.PickerItem{{Key: targetID}})
	picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	before := len(m.messages)
	m.applyScreenResult(picker)

	if m.sessionID != "source" || m.loop.Provider != oldProvider || m.model != "source-model" || m.loop.Model != "source-model" {
		t.Fatalf("picker failure partially activated target: id=%q model=%q loopModel=%q provider=%T", m.sessionID, m.model, m.loop.Model, m.loop.Provider)
	}
	warned := false
	for _, msg := range m.messages[before:] {
		if msg.Role == "success" && strings.Contains(msg.Content, "resumed session") {
			t.Fatalf("failed resume reported success: %+v", m.messages[before:])
		}
		if msg.Role == "warning" && strings.Contains(msg.Content, "session source remains active") && strings.Contains(msg.Content, "provider/model preflight") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("failed resume did not give a clear source-session warning: %+v", m.messages[before:])
	}
}

func TestTurnActiveStaleSessionPickersCannotRebindLoop(t *testing.T) {
	tests := []struct {
		name string
		pick func(*Model)
	}{
		{
			name: "sessions picker",
			pick: func(m *Model) {
				picker := screen.NewPickerScreen("/sessions", "", []screen.PickerItem{{Key: "target"}})
				picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				m.applyScreenResult(picker)
			},
		},
		{
			name: "resume picker",
			pick: func(m *Model) {
				picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
				picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				m.applyScreenResult(picker)
			},
		},
		{
			name: "fresh picker action",
			pick: func(m *Model) {
				picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
				picker.Update(tea.KeyPressMsg{Code: 'n'})
				m.applyScreenResult(picker)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			if err := store.WriteHeaderFull(session.Header{
				ID: "target", Provider: "wire", Model: "target-model", System: "target-system", Mode: string(permission.ModeFullAccess),
			}); err != nil {
				t.Fatal(err)
			}
			m.loop.ResetSession([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "source sentinel"}}}})
			beforeProvider := m.loop.Provider
			beforeHistory := m.loop.History()
			beforeMode := m.gate.Mode()
			beforePrePlan := m.loop.PrePlanMode()
			m.turnActive = true

			tt.pick(m)

			if m.sessionID != "source" || m.loop.Provider != beforeProvider || m.gate.Mode() != beforeMode || m.loop.PrePlanMode() != beforePrePlan {
				t.Fatalf("stale picker rebound live turn: session=%q providerChanged=%v mode=%q prePlan=%q",
					m.sessionID, m.loop.Provider != beforeProvider, m.gate.Mode(), m.loop.PrePlanMode())
			}
			if got := m.loop.History(); !reflect.DeepEqual(got, beforeHistory) {
				t.Fatalf("stale picker replaced live history: got=%+v want=%+v", got, beforeHistory)
			}
			if len(m.messages) == 0 || !strings.Contains(m.messages[len(m.messages)-1].Content, "can't switch sessions mid-turn") {
				t.Fatalf("stale picker did not surface refusal: %+v", m.messages)
			}
		})
	}
}

func TestFreshAndForkStartNewPermissionLifetime(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeBypass)
	m.gate.AppendRules(permission.Rule{Tool: "Bash", Verb: permission.DecisionAllow, Source: "interactive"})
	m.loop.System = "source-session-system"

	m.persistActiveSessionState()
	freshID, freshHdr, err := m.createFreshSession()
	if err != nil {
		t.Fatalf("createFreshSession: %v", err)
	}
	if err := m.activateSession(freshID, freshHdr, nil, false); err != nil {
		t.Fatalf("activate fresh: %v", err)
	}
	if m.gate.Mode() != permission.ModeAsk || len(m.gate.Snapshot()) != 0 {
		t.Fatalf("fresh inherited source permissions: mode=%q rules=%+v", m.gate.Mode(), m.gate.Snapshot())
	}
	loadedFresh, _, err := store.Load(freshID)
	if err != nil {
		t.Fatalf("load fresh: %v", err)
	}
	if loadedFresh.Mode != "default" || loadedFresh.System != "base-system" {
		t.Errorf("fresh header = %+v, want default + invocation system", loadedFresh)
	}

	parent := "parent"
	parentHdr := session.Header{
		ID: parent, Model: "test-model", System: "parent-system", Mode: "bypass",
		AlwaysAllow: []session.SavedRule{{Tool: "Edit", Verb: int(permission.DecisionAllow), Source: "interactive"}},
	}
	if err := store.WriteHeaderFull(parentHdr); err != nil {
		t.Fatalf("parent header: %v", err)
	}
	parentMessages := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "fork me"}}}}
	forkID, forkHdr, err := m.forkSession(parent, parentMessages)
	if err != nil {
		t.Fatalf("forkSession: %v", err)
	}
	if err := m.activateSession(forkID, forkHdr, parentMessages, false); err != nil {
		t.Fatalf("activate fork: %v", err)
	}
	if m.gate.Mode() != permission.ModeAsk || len(m.gate.Snapshot()) != 0 {
		t.Fatalf("fork inherited parent/source permissions: mode=%q rules=%+v", m.gate.Mode(), m.gate.Snapshot())
	}
	loadedFork, _, err := store.Load(forkID)
	if err != nil {
		t.Fatalf("load fork: %v", err)
	}
	if loadedFork.Mode != "default" || len(loadedFork.AlwaysAllow) != 0 || loadedFork.ForkedFrom == nil || loadedFork.ForkedFrom.SessionID != parent {
		t.Errorf("fork header = %+v", loadedFork)
	}
}

func TestPersistActiveSessionStateOnlySavesSessionRules(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAcceptEdits)
	m.providerName = "openai"
	m.model = "gpt-current"
	m.loop.System = "current-system"
	m.gate.AppendRules(
		permission.Rule{Tool: "Bash", Verb: permission.DecisionDeny, Source: "policy:deny"},
		permission.Rule{Tool: "Read", Verb: permission.DecisionAllow, Source: "config:allow"},
		permission.Rule{Tool: "Edit", Verb: permission.DecisionAllow, Source: "interactive"},
	)
	m.persistActiveSessionState()
	hdr, _, err := store.LoadHeader("source")
	if err != nil {
		t.Fatalf("LoadHeader: %v", err)
	}
	if hdr.Provider != "openai" || hdr.Model != "gpt-current" || hdr.System != "current-system" ||
		hdr.Mode != "acceptEdits" || len(hdr.AlwaysAllow) != 1 || hdr.AlwaysAllow[0].Tool != "Edit" {
		t.Errorf("persisted header = %+v, want live provider/model/system + only interactive Edit approval", hdr)
	}
}

func TestPersistSessionStateWaitsForCompletePermissionSnapshot(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeFullAccess)
	entered := make(chan struct{})
	release := make(chan struct{})
	transitionDone := make(chan error, 1)
	go func() {
		transitionDone <- m.gate.RunModeTransition(func() error {
			m.loop.SetPrePlanMode(string(permission.ModeFullAccess))
			close(entered)
			<-release
			m.gate.SetMode(permission.ModePlan)
			m.loop.SetPlanMode(true)
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("permission transition did not reach its in-flight snapshot")
	}
	time.AfterFunc(100*time.Millisecond, func() { close(release) })

	if err := persistSessionState(store, "source", m.gate, m.loop, "wire", "model", "system"); err != nil {
		t.Fatal(err)
	}
	if err := <-transitionDone; err != nil {
		t.Fatal(err)
	}
	hdr, _, err := store.LoadHeader("source")
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Mode != string(permission.ModePlan) || hdr.PrePlanMode != string(permission.ModeFullAccess) {
		t.Fatalf("persisted permission snapshot = mode %q pre-plan %q", hdr.Mode, hdr.PrePlanMode)
	}
}

func TestPersistActiveSessionStateClearsPreviouslySavedRules(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.gate.AppendRules(permission.Rule{Tool: "Edit", Verb: permission.DecisionAllow, Source: "interactive"})
	m.persistActiveSessionState()
	hdr, _, err := store.LoadHeader("source")
	if err != nil {
		t.Fatal(err)
	}
	if len(hdr.AlwaysAllow) != 1 {
		t.Fatalf("precondition: rule was not saved: %+v", hdr.AlwaysAllow)
	}

	m.gate.ResetSessionState(permission.ModeAsk, nil)
	m.persistActiveSessionState()
	for _, load := range []struct {
		name string
		fn   func() (*session.Header, error)
	}{
		{name: "Load", fn: func() (*session.Header, error) { h, _, err := store.Load("source"); return h, err }},
		{name: "LoadHeader", fn: func() (*session.Header, error) { h, _, err := store.LoadHeader("source"); return h, err }},
	} {
		t.Run(load.name, func(t *testing.T) {
			hdr, err := load.fn()
			if err != nil {
				t.Fatal(err)
			}
			if len(hdr.AlwaysAllow) != 0 {
				t.Fatalf("cleared approval resurrected from an older header: %+v", hdr.AlwaysAllow)
			}
		})
	}
}

func TestREPLFreshAndBranchRebindSessionState(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeBypass)
	r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
	if err != nil {
		t.Fatalf("NewREPL: %v", err)
	}
	r.FreshPermissionMode = permission.ModeAsk
	rebound := ""
	boundaryCalls := 0
	r.SessionSwitch = func(id string) { rebound = id }
	r.SessionBoundary = func() { boundaryCalls++ }
	r.Gate.AppendRules(permission.Rule{Tool: "Edit", Verb: permission.DecisionAllow, Source: "interactive"})
	r.Loop.AppendUser("keep in branch")

	branchID, err := r.branchSession()
	if err != nil {
		t.Fatalf("branchSession: %v", err)
	}
	if r.SessionID != branchID || rebound != branchID || r.Gate.Mode() != permission.ModeAsk || len(r.Gate.Snapshot()) != 0 {
		t.Fatalf("branch boundary incomplete: id=%q rebound=%q mode=%q rules=%+v", r.SessionID, rebound, r.Gate.Mode(), r.Gate.Snapshot())
	}
	if boundaryCalls != 1 {
		t.Fatalf("branch session boundary calls = %d, want 1", boundaryCalls)
	}
	if len(r.Loop.History()) != 1 {
		t.Fatalf("branch should retain history, got %+v", r.Loop.History())
	}

	r.Gate.SetMode(permission.ModeBypass)
	r.Gate.AppendRules(permission.Rule{Tool: "Bash", Verb: permission.DecisionAllow, Source: "interactive"})
	freshID, err := r.startFreshSession()
	if err != nil {
		t.Fatalf("startFreshSession: %v", err)
	}
	if r.SessionID != freshID || rebound != freshID || r.Gate.Mode() != permission.ModeAsk || len(r.Gate.Snapshot()) != 0 {
		t.Fatalf("fresh boundary incomplete: id=%q rebound=%q mode=%q rules=%+v", r.SessionID, rebound, r.Gate.Mode(), r.Gate.Snapshot())
	}
	if boundaryCalls != 2 {
		t.Fatalf("fresh session boundary calls = %d, want 2 total", boundaryCalls)
	}
	if len(r.Loop.History()) != 0 {
		t.Fatalf("fresh session retained history: %+v", r.Loop.History())
	}
}

func TestREPLPlanFreshSessionKeepsLineageAcrossRepeatedNew(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModePlan)
	m.loop.SetPrePlanMode(string(permission.ModeDefault))
	m.loop.SetPlanMode(true)
	r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
	if err != nil {
		t.Fatalf("NewREPL: %v", err)
	}
	r.FreshPermissionMode = permission.ModePlan

	firstID, err := r.startFreshSession()
	if err != nil {
		t.Fatalf("first /new: %v", err)
	}
	if got := r.Loop.PrePlanMode(); got != string(permission.ModeDefault) {
		t.Fatalf("first /new pre-plan mode = %q, want %q", got, permission.ModeDefault)
	}
	secondID, err := r.startFreshSession()
	if err != nil {
		t.Fatalf("second /new after %s: %v", firstID, err)
	}
	if secondID == firstID {
		t.Fatalf("second /new reused session ID %q", secondID)
	}
	if r.Gate.Mode() != permission.ModePlan || !r.Loop.IsPlanMode() {
		t.Fatalf("second /new permission state = gate %q, plan %v", r.Gate.Mode(), r.Loop.IsPlanMode())
	}
	if got := r.Loop.PrePlanMode(); got != string(permission.ModeDefault) {
		t.Fatalf("second /new pre-plan mode = %q, want %q", got, permission.ModeDefault)
	}
}

func attachLifecycleMemory(t *testing.T, loop *agent.Loop) *memory.MemoryManager {
	t.Helper()
	repository, err := memory.NewMemoryManager(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("NewMemoryManager: %v", err)
	}
	loop.Memory = repository
	loop.ResetSession([]llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "remember lifecycle source"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "lifecycle source remembered"}}},
	})
	return repository
}

func assertDailySource(t *testing.T, repository *memory.MemoryManager, sessionID, wantSource string) {
	t.Helper()
	notes, err := repository.ListDailyNotes(20)
	if err != nil {
		t.Fatalf("ListDailyNotes: %v", err)
	}
	for _, note := range notes {
		if note.SessionID == sessionID {
			if note.Source != wantSource {
				t.Fatalf("daily source for %q = %q, want %q", sessionID, note.Source, wantSource)
			}
			return
		}
	}
	t.Fatalf("no daily note persisted for session %q: %+v", sessionID, notes)
}

func TestModelSessionLifecyclePersistsDailySourceMatrix(t *testing.T) {
	tests := []struct {
		name       string
		wantSource string
		activate   func(t *testing.T, m *Model, store *session.Store) error
	}{
		{
			name:       "new",
			wantSource: "cli-new",
			activate: func(t *testing.T, m *Model, _ *session.Store) error {
				id, hdr, err := m.createFreshSession()
				if err != nil {
					return err
				}
				return m.activateSession(id, hdr, nil, false)
			},
		},
		{
			name:       "branch",
			wantSource: "cli-branch",
			activate: func(t *testing.T, m *Model, _ *session.Store) error {
				history := m.loop.History()
				id, hdr, err := m.forkSession(m.sessionID, history)
				if err != nil {
					return err
				}
				return m.activateSession(id, hdr, history, false)
			},
		},
		{
			name:       "resume",
			wantSource: "cli-resume",
			activate: func(t *testing.T, m *Model, store *session.Store) error {
				const id = "resume-target"
				hdr := &session.Header{ID: id, Model: "test-model", System: "base-system", Mode: "ask"}
				if err := store.WriteHeaderFull(*hdr); err != nil {
					return err
				}
				return m.activateSession(id, hdr, nil, true)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			repository := attachLifecycleMemory(t, m.loop)
			if err := tt.activate(t, m, store); err != nil {
				t.Fatalf("activate: %v", err)
			}
			assertDailySource(t, repository, "source", tt.wantSource)
		})
	}
}

func TestModelSessionPickerLifecyclePersistsDailySourceMatrix(t *testing.T) {
	tests := []struct {
		name       string
		wantSource string
		pick       func(*Model)
	}{
		{
			name:       "sessions picker resumes",
			wantSource: "cli-resume",
			pick: func(m *Model) {
				picker := screen.NewPickerScreen("/sessions", "", []screen.PickerItem{{Key: "target"}})
				picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				m.applyScreenResult(picker)
			},
		},
		{
			name:       "resume picker starts fresh",
			wantSource: "cli-new",
			pick: func(m *Model) {
				picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
				picker.Update(tea.KeyPressMsg{Code: 'n'})
				m.applyScreenResult(picker)
			},
		},
		{
			name:       "resume picker resumes",
			wantSource: "cli-resume",
			pick: func(m *Model) {
				picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
				picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				m.applyScreenResult(picker)
			},
		},
		{
			name:       "resume picker forks",
			wantSource: "cli-branch",
			pick: func(m *Model) {
				picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
				picker.Update(tea.KeyPressMsg{Code: 'f'})
				m.applyScreenResult(picker)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			repository := attachLifecycleMemory(t, m.loop)
			if err := store.WriteHeaderFull(session.Header{ID: "target", Model: "test-model", System: "base-system", Mode: "ask"}); err != nil {
				t.Fatal(err)
			}

			tt.pick(m)

			if m.sessionID == "source" {
				t.Fatalf("picker did not switch the source session: messages=%+v", m.messages)
			}
			assertDailySource(t, repository, "source", tt.wantSource)
		})
	}
}

func TestREPLSessionLifecyclePersistsDailySourceMatrix(t *testing.T) {
	tests := []struct {
		name       string
		wantSource string
		run        func(*REPL) error
	}{
		{name: "new", wantSource: "cli-new", run: func(r *REPL) error { _, err := r.startFreshSession(); return err }},
		{name: "branch", wantSource: "cli-branch", run: func(r *REPL) error { _, err := r.branchSession(); return err }},
		{name: "close", wantSource: "cli-close", run: func(r *REPL) error {
			r.stdin = strings.NewReader("/quit\n")
			r.out = &bytes.Buffer{}
			return r.Run(context.Background())
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			repository := attachLifecycleMemory(t, m.loop)
			r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.run(r); err != nil {
				t.Fatalf("lifecycle: %v", err)
			}
			assertDailySource(t, repository, "source", tt.wantSource)
		})
	}
}

func TestModelSessionBoundaryIsOncePerLiveSessionGeneration(t *testing.T) {
	m, _ := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	boundaryCalls := 0
	m.ext.SessionBoundary = func() { boundaryCalls++ }

	if err := m.leaveActiveSession("cli-close", true); err != nil {
		t.Fatal(err)
	}
	if err := m.leaveActiveSession("cli-close", true); err != nil {
		t.Fatal(err)
	}
	if boundaryCalls != 1 {
		t.Fatalf("boundary calls = %d, want exactly one", boundaryCalls)
	}
	assertDailySource(t, repository, "source", "cli-close")

	id, hdr, err := m.createFreshSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.activateSession(id, hdr, nil, false); err != nil {
		t.Fatal(err)
	}
	m.loop.ResetSession([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new generation"}}}})
	if err := m.leaveActiveSession("cli-close", true); err != nil {
		t.Fatal(err)
	}
	if boundaryCalls != 2 {
		t.Fatalf("boundary calls after rebind = %d, want two generations", boundaryCalls)
	}
	assertDailySource(t, repository, id, "cli-close")
}

type failingDailyRepository struct {
	memory.Repository
}

func (failingDailyRepository) SaveDailyNote(string, string, string) error {
	return errors.New("daily disk unavailable")
}

func listedSessionIDs(t *testing.T, store *session.Store) map[string]bool {
	t.Helper()
	entries, err := store.List(0)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	ids := make(map[string]bool, len(entries))
	for _, entry := range entries {
		ids[entry.ID] = true
	}
	return ids
}

func assertSessionIDsEqual(t *testing.T, got, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("session IDs = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("session IDs = %v, want %v", got, want)
		}
	}
}

func TestModelSessionSwitchKeepsSourceActiveWhenDailyPersistenceFails(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	m.loop.Memory = failingDailyRepository{Repository: repository}
	boundaryCalls := 0
	m.ext.SessionBoundary = func() { boundaryCalls++ }
	target := &session.Header{ID: "daily-failure-target", Model: "test-model", System: "base-system", Mode: "ask"}
	if err := store.WriteHeaderFull(*target); err != nil {
		t.Fatal(err)
	}

	err := m.activateSession(target.ID, target, nil, true)
	if err == nil || !strings.Contains(err.Error(), "daily disk unavailable") {
		t.Fatalf("activateSession error = %v, want Daily persistence failure", err)
	}
	if m.sessionID != "source" || boundaryCalls != 0 || m.sessionBoundaryClosed {
		t.Fatalf("failed boundary changed live session: id=%q releases=%d closed=%v", m.sessionID, boundaryCalls, m.sessionBoundaryClosed)
	}
}

func TestCreatedSessionActivationFailureDoesNotLeaveGhostSession(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Model)
	}{
		{name: "slash new", run: func(m *Model) { m.input.SetValue("/new"); m.handleSubmit() }},
		{name: "slash branch", run: func(m *Model) { m.input.SetValue("/branch"); m.handleSubmit() }},
		{name: "resume picker fresh", run: func(m *Model) {
			picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
			picker.Update(tea.KeyPressMsg{Code: 'n'})
			m.applyScreenResult(picker)
		}},
		{name: "resume picker fork", run: func(m *Model) {
			picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
			picker.Update(tea.KeyPressMsg{Code: 'f'})
			m.applyScreenResult(picker)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			repository := attachLifecycleMemory(t, m.loop)
			m.loop.Memory = failingDailyRepository{Repository: repository}
			if err := store.WriteHeaderFull(session.Header{ID: "target", Model: "test-model", System: "base-system", Mode: "ask"}); err != nil {
				t.Fatal(err)
			}
			reg := slash.NewRegistry()
			slash.RegisterAll(reg, nil)
			m.slash = reg
			beforeIDs := listedSessionIDs(t, store)
			beforeMessages := len(m.messages)

			tt.run(m)

			if m.sessionID != "source" || m.sessionBoundaryClosed {
				t.Fatalf("failed activation changed live source: id=%q closed=%v", m.sessionID, m.sessionBoundaryClosed)
			}
			assertSessionIDsEqual(t, listedSessionIDs(t, store), beforeIDs)
			found := false
			for _, msg := range m.messages[beforeMessages:] {
				if msg.Role == "warning" && strings.Contains(msg.Content, "daily disk unavailable") {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("activation failure was not surfaced: %+v", m.messages[beforeMessages:])
			}
		})
	}
}

func TestREPLCreatedSessionFailureDoesNotLeaveGhostSession(t *testing.T) {
	tests := []struct {
		name string
		run  func(*REPL) (string, error)
	}{
		{name: "new", run: (*REPL).startFreshSession},
		{name: "branch", run: (*REPL).branchSession},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			repository := attachLifecycleMemory(t, m.loop)
			m.loop.Memory = failingDailyRepository{Repository: repository}
			r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
			if err != nil {
				t.Fatal(err)
			}
			beforeIDs := listedSessionIDs(t, store)

			if _, err := tt.run(r); err == nil || !strings.Contains(err.Error(), "daily disk unavailable") {
				t.Fatalf("switch error = %v, want Daily persistence failure", err)
			}
			if r.SessionID != "source" || r.sessionBoundaryClosed {
				t.Fatalf("failed switch changed live source: id=%q closed=%v", r.SessionID, r.sessionBoundaryClosed)
			}
			assertSessionIDsEqual(t, listedSessionIDs(t, store), beforeIDs)
		})
	}
}

func TestRunTUIClosePersistsDailyMemory(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A canceled program context gives the test a deterministic, non-interactive
	// Bubble Tea exit while still exercising RunTUI's real shutdown tail.
	_ = RunTUI(
		ctx,
		m.loop,
		nil,
		slash.NewRegistry(),
		store,
		"source",
		m.gate,
		"test-model",
		"",
		"",
		&config.Config{},
		false,
	)
	assertDailySource(t, repository, "source", "cli-close")
}

func TestStopForegroundTurnForCloseWaitsForDoneBeforePersisting(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.loop.AppendUser("foreground tail must survive close")
	m.turnActive = true
	m.spinnerActive = true
	m.doneCh = make(chan error, 1)
	cancelled := make(chan struct{})
	m.turnCancel = func() { close(cancelled) }
	done := make(chan error, 1)
	go func() { done <- m.stopForegroundTurnForClose() }()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("foreground cancel was not requested")
	}
	select {
	case err := <-done:
		t.Fatalf("close returned before foreground turn completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	_, beforeMessages, err := store.Load("source")
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeMessages) != 0 {
		t.Fatalf("foreground tail persisted before the turn completed: %+v", beforeMessages)
	}

	m.doneCh <- context.Canceled
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopForegroundTurnForClose: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not continue after foreground turn completed")
	}
	if m.turnActive || m.spinnerActive {
		t.Fatalf("foreground state still active: turn=%v spinner=%v", m.turnActive, m.spinnerActive)
	}
	_, messages, err := store.Load("source")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != llm.RoleUser || messages[0].Content[0].Text != "foreground tail must survive close" {
		t.Fatalf("persisted foreground tail = %+v", messages)
	}
}

func TestStopForegroundTurnForCloseFailsClosedWhenTailCannotPersist(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.loop.AppendUser("foreground tail cannot be dropped")
	m.turnActive = true
	m.spinnerActive = true
	m.doneCh = make(chan error, 1)
	m.doneCh <- context.Canceled
	blockActiveSessionWrites(t, store, "source")

	err := m.stopForegroundTurnForClose()
	if err == nil || !strings.Contains(err.Error(), "persist foreground turn") {
		t.Fatalf("stop error = %v, want foreground persistence failure", err)
	}
	if m.turnActive || m.spinnerActive {
		t.Fatalf("completed foreground state remained active: turn=%v spinner=%v", m.turnActive, m.spinnerActive)
	}
}

func TestRunTUICloseSkipsDailyWhenSourceStateCannotPersist(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	blockActiveSessionWrites(t, store, "source")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunTUI(
		ctx,
		m.loop,
		nil,
		slash.NewRegistry(),
		store,
		"source",
		m.gate,
		"test-model",
		"",
		"",
		&config.Config{},
		false,
	); err == nil {
		t.Fatal("RunTUI succeeded despite source persistence failure")
	}
	notes, err := repository.ListDailyNotes(20)
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range notes {
		if note.SessionID == "source" {
			t.Fatalf("Daily note was written after source persistence failed: %+v", note)
		}
	}
}

type lifecycleBlockingProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (*lifecycleBlockingProvider) Name() string          { return "lifecycle-blocking" }
func (*lifecycleBlockingProvider) ModelID() string       { return "test-model" }
func (*lifecycleBlockingProvider) MaxContextTokens() int { return 200_000 }
func (p *lifecycleBlockingProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (*lifecycleBlockingProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("stream not used")
}

func TestModelSessionSwitchDoesNotWaitForFrozenAutoMemory(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	provider := &lifecycleBlockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.loop.RebindProviderRuntime(provider, "test-model", 1024, m.loop.System, m.loop.SystemSections)
	m.loop.AutoMemory = true
	extractor, err := agent.NewAutoMemoryExtractor(m.loop, repository.Root(), filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatalf("NewAutoMemoryExtractor: %v", err)
	}
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three", "four"} {
		if err := os.WriteFile(filepath.Join(sessionsDir, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	extractor.SetSessionsDir(sessionsDir)
	extractor.OnLoopEnd(context.Background(), "end_turn")
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Auto Memory provider did not start")
	}

	target := &session.Header{ID: "target-after-memory", Model: "test-model", System: "base-system", Mode: "ask"}
	if err := store.WriteHeaderFull(*target); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.activateSession(target.ID, target, nil, true) }()
	release := func() {
		select {
		case <-provider.release:
		default:
			close(provider.release)
		}
	}
	t.Cleanup(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("activateSession: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		release()
		<-done
		t.Fatal("session switch blocked on an immutable Auto Memory snapshot")
	}
	if m.sessionID != target.ID {
		t.Fatalf("active session = %q, want %q", m.sessionID, target.ID)
	}
	busyCtx, cancelBusy := context.WithTimeout(context.Background(), 30*time.Millisecond)
	busyErr := m.loop.WaitAutoMemoryIdle(busyCtx)
	cancelBusy()
	if !errors.Is(busyErr, context.DeadlineExceeded) {
		t.Fatalf("Auto Memory unexpectedly stopped at session switch: %v", busyErr)
	}
	assertDailySource(t, repository, "source", "cli-resume")

	release()
	idleCtx, cancelIdle := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelIdle()
	if err := m.loop.WaitAutoMemoryIdle(idleCtx); err != nil {
		t.Fatalf("wait for frozen Auto Memory snapshot: %v", err)
	}
}

type lifecycleDistillProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (*lifecycleDistillProvider) Name() string          { return "lifecycle-distill" }
func (*lifecycleDistillProvider) ModelID() string       { return "test-model" }
func (*lifecycleDistillProvider) MaxContextTokens() int { return 200_000 }
func (p *lifecycleDistillProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return &llm.Response{Content: []llm.ContentBlock{{
			Type: "text",
			Text: `[{"type":"user","content":"The user's TUI boundary codename is Citrine Lark.","tags":["tui","boundary","codename"]}]`,
		}}, StopReason: "end_turn"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (*lifecycleDistillProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return &lifecycleTextStream{}, nil
}

type lifecycleTextStream struct{ step int }

func (*lifecycleTextStream) Close() error { return nil }
func (s *lifecycleTextStream) Recv() (llm.StreamEvent, error) {
	s.step++
	switch s.step {
	case 1:
		return llm.StreamEvent{Type: "message_start", InputTokens: 1}, nil
	case 2:
		return llm.StreamEvent{Type: "text_delta", TextDelta: "I will remember this sufficiently long lifecycle answer for later."}, nil
	case 3:
		return llm.StreamEvent{Type: "message_delta", StopReason: "end_turn", OutputTokens: 1}, nil
	case 4:
		return llm.StreamEvent{Type: "message_stop", StopReason: "end_turn"}, nil
	default:
		return llm.StreamEvent{}, io.EOF
	}
}

func TestModelSessionSwitchWaitsForDistillationBeforeDaily(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	provider := &lifecycleDistillProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.loop.RebindProviderRuntime(provider, "test-model", 1024, m.loop.System, m.loop.SystemSections)
	m.loop.DistillEvery = 1
	m.loop.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot {
		return agent.RuntimeStateSnapshot{SessionID: "source"}
	}
	m.loop.ResetSession([]llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Type: "text", Text: "Please remember this sufficiently long lifecycle fact for future sessions.",
		}},
	}})
	if err := m.loop.Run(context.Background(), make(chan agent.Event, 32)); err != nil {
		t.Fatalf("Loop.Run: %v", err)
	}
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("distillation provider did not start")
	}

	target := &session.Header{ID: "target-after-distill", Model: "test-model", System: "base-system", Mode: "ask"}
	if err := store.WriteHeaderFull(*target); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.activateSession(target.ID, target, nil, true) }()
	release := func() {
		select {
		case <-provider.release:
		default:
			close(provider.release)
		}
	}
	t.Cleanup(release)
	select {
	case err := <-done:
		t.Fatalf("session switch completed before source distillation was durable: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	if m.sessionID != "source" || m.sessionBoundaryClosed {
		t.Fatalf("blocked switch changed source state: id=%q closed=%v", m.sessionID, m.sessionBoundaryClosed)
	}
	notes, err := repository.ListDailyNotes(20)
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range notes {
		if note.SessionID == "source" {
			t.Fatalf("Daily was written before distillation completed: %+v", note)
		}
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("activateSession after distillation release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session switch did not finish after distillation became durable")
	}
	if m.sessionID != target.ID {
		t.Fatalf("active session = %q, want %q", m.sessionID, target.ID)
	}
	assertDailySource(t, repository, "source", "cli-resume")
}

func runLifecycleOneTurn(t *testing.T, m *Model, provider llm.Provider, prompt string) {
	t.Helper()
	m.loop.RebindProviderRuntime(provider, "test-model", 1024, m.loop.System, m.loop.SystemSections)
	m.loop.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot {
		return agent.RuntimeStateSnapshot{SessionID: "source"}
	}
	m.loop.ResetSession([]llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Type: "text", Text: prompt,
		}},
	}})
	if err := m.loop.Run(context.Background(), make(chan agent.Event, 32)); err != nil {
		t.Fatalf("Loop.Run: %v", err)
	}
}

func TestModelOneTurnSessionSwitchFlushesResidualDistillation(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	release := make(chan struct{})
	close(release)
	provider := &lifecycleDistillProvider{entered: make(chan struct{}, 1), release: release}
	runLifecycleOneTurn(
		t,
		m,
		provider,
		"Please remember that my TUI boundary codename is Citrine Lark in future sessions.",
	)
	if got := m.loop.DistillEvery; got != agent.DefaultDistillEvery {
		t.Fatalf("DistillEvery=%d, want default %d", got, agent.DefaultDistillEvery)
	}
	select {
	case <-provider.entered:
		t.Fatal("one-turn session distilled before a durability boundary")
	default:
	}

	target := &session.Header{ID: "target-after-residual", Model: "test-model", System: "base-system", Mode: "ask"}
	if err := store.WriteHeaderFull(*target); err != nil {
		t.Fatal(err)
	}
	if err := m.activateSession(target.ID, target, nil, true); err != nil {
		t.Fatalf("activateSession: %v", err)
	}
	select {
	case <-provider.entered:
	default:
		t.Fatal("session switch did not flush the one-turn residual distillation")
	}
	hits, err := repository.Archival().Search(memory.SearchOptions{Query: "Citrine Lark"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SourceSessionID != "source" ||
		!strings.HasPrefix(hits[0].SourceMessageID, "source/message/") {
		t.Fatalf("boundary-distilled memory/provenance = %+v", hits)
	}
	assertDailySource(t, repository, "source", "cli-resume")
}

func TestModelSessionSwitchDistillEveryZeroIsNoOp(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	release := make(chan struct{})
	close(release)
	provider := &lifecycleDistillProvider{entered: make(chan struct{}, 1), release: release}
	m.loop.DistillEvery = 0
	runLifecycleOneTurn(
		t,
		m,
		provider,
		"This successful exchange must not be distilled because the feature is explicitly disabled.",
	)
	target := &session.Header{ID: "target-distillation-disabled", Model: "test-model", System: "base-system", Mode: "ask"}
	if err := store.WriteHeaderFull(*target); err != nil {
		t.Fatal(err)
	}
	if err := m.activateSession(target.ID, target, nil, true); err != nil {
		t.Fatalf("activateSession: %v", err)
	}
	select {
	case <-provider.entered:
		t.Fatal("DistillEvery=0 invoked the distillation provider at a session boundary")
	default:
	}
	hits, err := repository.Archival().Search(memory.SearchOptions{Query: "explicitly disabled"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("DistillEvery=0 persisted archival memory: %+v", hits)
	}
	assertDailySource(t, repository, "source", "cli-resume")
}

type blockingResidualRepository struct {
	memory.Repository
	entered chan struct{}
	release chan struct{}
}

func (r *blockingResidualRepository) DistillTurnWithMetadata(
	_ context.Context,
	_ llm.Provider,
	_, _, _, _ string,
) error {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	<-r.release // Deliberately model a provider/repository that ignores context.
	return nil
}

func TestMemoryBoundaryWaitFailureIsFailClosedButCloseStillWritesDaily(t *testing.T) {
	m, _ := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	blocking := &blockingResidualRepository{
		Repository: repository,
		entered:    make(chan struct{}, 1),
		release:    make(chan struct{}),
	}
	m.loop.Memory = blocking
	providerRelease := make(chan struct{})
	close(providerRelease)
	provider := &lifecycleDistillProvider{entered: make(chan struct{}, 1), release: providerRelease}
	runLifecycleOneTurn(
		t,
		m,
		provider,
		"Remember this one-turn fact even when the boundary join initially times out.",
	)
	release := func() {
		select {
		case <-blocking.release:
		default:
			close(blocking.release)
		}
	}
	t.Cleanup(release)

	err := persistMemoryBoundaryWithGrace(
		m.loop, "source", "cli-resume", "source summary", false, 15*time.Millisecond, time.Second,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("switch boundary error = %v, want deadline exceeded", err)
	}
	notes, err := repository.ListDailyNotes(20)
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range notes {
		if note.SessionID == "source" {
			t.Fatalf("failed switch wrote a Daily note: %+v", note)
		}
	}

	err = persistMemoryBoundaryWithGrace(
		m.loop, "source", "cli-close", "final source summary", true, 15*time.Millisecond, time.Second,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close boundary error = %v, want aggregated deadline exceeded", err)
	}
	assertDailySource(t, repository, "source", "cli-close")
	stillRunning, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	err = m.loop.WaitForDistillation(stillRunning, "source")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close boundary canceled freshly flushed distillation: %v", err)
	}
	release()
	joined, cancelJoined := context.WithTimeout(context.Background(), time.Second)
	defer cancelJoined()
	if err := m.loop.WaitForDistillation(joined, "source"); err != nil {
		t.Fatalf("join residual after release: %v", err)
	}
}

func blockActiveSessionWrites(t *testing.T, store *session.Store, id string) {
	t.Helper()
	path := filepath.Join(store.Dir, id+".jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove active session file: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("replace active session file with directory: %v", err)
	}
}

func TestREPLSessionBoundaryRequiresSuccessfulPersistence(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
	if err != nil {
		t.Fatal(err)
	}
	r.Loop.AppendUser("source history")
	boundaryCalls := 0
	r.SessionBoundary = func() { boundaryCalls++ }
	blockActiveSessionWrites(t, store, "source")

	for _, switchSession := range []struct {
		name string
		fn   func() (string, error)
	}{
		{name: "fresh", fn: r.startFreshSession},
		{name: "branch", fn: r.branchSession},
	} {
		t.Run(switchSession.name, func(t *testing.T) {
			if _, err := switchSession.fn(); err == nil || !strings.Contains(err.Error(), "persist session source state") {
				t.Fatalf("switch error = %v, want source persistence failure", err)
			}
			if r.SessionID != "source" || len(r.Loop.History()) != 1 || boundaryCalls != 0 {
				t.Fatalf("failed switch changed live state: id=%q history=%d boundary=%d", r.SessionID, len(r.Loop.History()), boundaryCalls)
			}
		})
	}
}

func TestSessionSwitchUISurfacesPersistenceFailure(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Model)
	}{
		{name: "slash new", run: func(m *Model) { m.input.SetValue("/new"); m.handleSubmit() }},
		{name: "slash branch", run: func(m *Model) { m.input.SetValue("/branch"); m.handleSubmit() }},
		{name: "sessions picker", run: func(m *Model) {
			picker := screen.NewPickerScreen("/sessions", "", []screen.PickerItem{{Key: "target"}})
			picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m.applyScreenResult(picker)
		}},
		{name: "resume picker fresh", run: func(m *Model) {
			picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
			picker.Update(tea.KeyPressMsg{Code: 'n'})
			m.applyScreenResult(picker)
		}},
		{name: "resume picker resume", run: func(m *Model) {
			picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
			picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m.applyScreenResult(picker)
		}},
		{name: "resume picker fork", run: func(m *Model) {
			picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: "target"}})
			picker.Update(tea.KeyPressMsg{Code: 'f'})
			m.applyScreenResult(picker)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, store := newSessionSwitchModel(t, permission.ModeAsk)
			if err := store.WriteHeaderFull(session.Header{ID: "target", Model: "test-model", Mode: "ask"}); err != nil {
				t.Fatal(err)
			}
			reg := slash.NewRegistry()
			slash.RegisterAll(reg, nil)
			m.slash = reg
			boundaryCalls := 0
			m.ext.SessionBoundary = func() { boundaryCalls++ }
			blockActiveSessionWrites(t, store, "source")
			before := len(m.messages)

			tt.run(m)

			if m.sessionID != "source" || boundaryCalls != 0 {
				t.Fatalf("persistence failure changed session: id=%q boundary=%d", m.sessionID, boundaryCalls)
			}
			found := false
			for _, msg := range m.messages[before:] {
				if msg.Role == "error" && strings.Contains(msg.Content, "persist session source state") {
					found = true
				}
			}
			if !found {
				t.Fatalf("persistence failure was not surfaced: %+v", m.messages[before:])
			}
		})
	}
}

func TestREPLRunReturnsFinalPersistenceFailure(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	repository := attachLifecycleMemory(t, m.loop)
	r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
	if err != nil {
		t.Fatal(err)
	}
	r.stdin = strings.NewReader("/quit\n")
	r.out = &bytes.Buffer{}
	blockActiveSessionWrites(t, store, "source")
	if err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "persist session source state") {
		t.Fatalf("Run error = %v, want final persistence failure", err)
	}
	notes, err := repository.ListDailyNotes(20)
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range notes {
		if note.SessionID == "source" {
			t.Fatalf("Daily note was written after source persistence failed: %+v", note)
		}
	}
}

func TestSessionBoundaryClearsSystemStateWhenBaseSystemIsEmpty(t *testing.T) {
	m, _ := newSessionSwitchModel(t, permission.ModeAsk)
	m.baseSystem = ""
	m.baseSystemSections = nil
	m.loop.System = "source-session-system"
	m.loop.SystemSections = []llm.SystemSection{{Name: "source", Body: "must not leak", Cache: true}}

	legacy := &session.Header{ID: "legacy-empty-system", Model: "test-model"}
	if err := m.activateSession(legacy.ID, legacy, nil, true); err != nil {
		t.Fatal(err)
	}
	if m.loop.System != "" || len(m.loop.SystemSections) != 0 {
		t.Fatalf("legacy destination inherited source system state: system=%q sections=%+v", m.loop.System, m.loop.SystemSections)
	}

	// Fresh creation must use the invocation base verbatim. Falling back to
	// Loop.System here would resurrect whichever resumed session was active.
	m.loop.System = "another-source-system"
	m.loop.SystemSections = []llm.SystemSection{{Name: "source-2", Body: "also stale"}}
	freshID, freshHdr, err := m.createFreshSession()
	if err != nil {
		t.Fatal(err)
	}
	if freshHdr.System != "" {
		t.Fatalf("fresh header inherited source system: %+v", freshHdr)
	}
	if err := m.activateSession(freshID, freshHdr, nil, false); err != nil {
		t.Fatal(err)
	}
	if m.loop.System != "" || len(m.loop.SystemSections) != 0 {
		t.Fatalf("fresh destination inherited source system state: system=%q sections=%+v", m.loop.System, m.loop.SystemSections)
	}
}

func TestREPLFreshSessionUsesEmptyInvocationBase(t *testing.T) {
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	m.loop.System = ""
	m.loop.SystemSections = nil
	r, err := NewREPL(m.loop, nil, store, "source", false, false, m.gate, "test-model", "")
	if err != nil {
		t.Fatal(err)
	}
	r.FreshPermissionMode = permission.ModeAsk
	// Simulate having resumed a session with a custom prompt after the REPL
	// captured an intentionally empty invocation base.
	r.Loop.System = "resumed-session-system"
	r.Loop.SystemSections = []llm.SystemSection{{Name: "resumed", Body: "stale"}}
	id, err := r.startFreshSession()
	if err != nil {
		t.Fatal(err)
	}
	hdr, _, err := store.LoadHeader(id)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.System != "" || r.Loop.System != "" || len(r.Loop.SystemSections) != 0 {
		t.Fatalf("REPL fresh session inherited resumed system state: header=%q loop=%q sections=%+v", hdr.System, r.Loop.System, r.Loop.SystemSections)
	}
}

func TestSessionPickersRejectOversizedPhysicalLedgerBeforeLoading(t *testing.T) {
	t.Setenv("METIS_RESUME_PHYSICAL_MAX_MB", "1")
	m, store := newSessionSwitchModel(t, permission.ModeAsk)
	const oversizedID = "oversized-target"
	if err := os.WriteFile(filepath.Join(store.Dir, oversizedID+".jsonl"), make([]byte, 2*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	assertRejected := func(t *testing.T, result screen.Screen) {
		t.Helper()
		beforeID := m.sessionID
		beforeMessages := len(m.messages)
		m.applyScreenResult(result)
		if m.sessionID != beforeID {
			t.Fatalf("oversized session was activated: before=%q after=%q", beforeID, m.sessionID)
		}
		found := false
		for _, msg := range m.messages[beforeMessages:] {
			if msg.Role == "error" && strings.Contains(msg.Content, "exceeds the 1.0 MiB physical resume limit") {
				found = true
			}
		}
		if !found {
			t.Fatalf("oversized resume did not surface size error: %+v", m.messages[beforeMessages:])
		}
	}

	t.Run("sessions picker", func(t *testing.T) {
		picker := screen.NewPickerScreen("/sessions", "", []screen.PickerItem{{Key: oversizedID}})
		picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		assertRejected(t, picker)
	})
	t.Run("desktop resume picker", func(t *testing.T) {
		picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: oversizedID}})
		picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		assertRejected(t, picker)
	})
	t.Run("desktop fork picker", func(t *testing.T) {
		picker := screen.NewResumeScreen([]screen.SessionEntry{{ID: oversizedID}})
		picker.Update(tea.KeyPressMsg{Code: 'f'})
		assertRejected(t, picker)
	})
}
