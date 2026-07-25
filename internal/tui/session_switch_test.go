package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
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
	return m, store
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

func TestSessionPickersRejectOversizedResumeBeforeLoading(t *testing.T) {
	t.Setenv("METIS_RESUME_MAX_MB", "1")
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
			if msg.Role == "error" && strings.Contains(msg.Content, "exceeds the 1.0 MiB resume limit") {
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
