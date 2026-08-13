package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
	pubsession "github.com/Ricardo-M-L/metis/pkg/session"
)

func writeThinkBackFixture(t *testing.T, store *session.Store, header session.Header, messages []llm.Message) {
	t.Helper()
	if err := store.WriteHeaderFull(header); err != nil {
		t.Fatalf("write header %s: %v", header.ID, err)
	}
	for _, message := range messages {
		if err := store.AppendMessage(header.ID, message); err != nil {
			t.Fatalf("append message %s: %v", header.ID, err)
		}
	}
}

func TestCollectThinkBack_CurrentNaturalYearAcrossSessions(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loc := time.FixedZone("fixture", 8*60*60)
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, loc)

	base := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Implement the auth feature and add tests"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "thinking", Text: "private reasoning"},
			{Type: "tool_use", ToolUseID: "read-1", ToolName: "Read"},
			{Type: "tool_use", ToolUseID: "edit-1", ToolName: "Edit"},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "read-1", ToolResult: "ok"},
			{Type: "tool_result", ToolUseID: "edit-1", ToolResult: "failed", IsError: true},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	}
	writeThinkBackFixture(t, store, session.Header{
		ID: "main", CreatedAt: time.Date(2026, time.January, 10, 9, 0, 0, 0, loc),
		Model: "model-a", WorkDir: "/work/app",
	}, base)

	branchMessages := append([]llm.Message(nil), base...)
	branchMessages = append(branchMessages,
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Refactor performance and review the git branch"}}},
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "reasoning", Text: "branch reasoning"},
			{Type: "tool_use", ToolUseID: "git-1", ToolName: "Git"},
		}},
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "git-1", ToolResult: "ok"}}},
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	)
	writeThinkBackFixture(t, store, session.Header{
		ID: "branch", CreatedAt: time.Date(2026, time.March, 3, 10, 0, 0, 0, loc),
		Model: "model-a", WorkDir: "/work/app",
		ForkedFrom: &pubsession.ForkRef{SessionID: "main", MessageCount: len(base)},
	}, branchMessages)

	subHeader := session.Header{
		ID: "subagent", CreatedAt: time.Date(2026, time.March, 4, 11, 0, 0, 0, loc),
		Model: "model-b", WorkDir: "/work/ops", SubAgentOf: "main",
	}
	subTranscript, err := agent.NewSubAgentTranscript(store.Dir, subHeader.ID, subHeader)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Research deployment configuration"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "web-1", ToolName: "WebSearch"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "web-1", ToolResult: "failed", IsError: true}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	} {
		if err := subTranscript.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	if err := subTranscript.Close(); err != nil {
		t.Fatal(err)
	}

	// Both are outside the report window: one belongs to 2025, the other is a
	// future-dated session later in the current natural year.
	writeThinkBackFixture(t, store, session.Header{
		ID: "old", CreatedAt: time.Date(2025, time.December, 31, 23, 0, 0, 0, loc), Model: "old-model",
	}, []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "old"}}}})
	writeThinkBackFixture(t, store, session.Header{
		ID: "future", CreatedAt: time.Date(2026, time.August, 14, 9, 0, 0, 0, loc), Model: "future-model",
	}, []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "future"}}}})

	report, err := collectThinkBack(store, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.year != 2026 || report.sessions != 3 || report.topLevelSessions != 2 || report.subAgentSessions != 1 || report.branches != 1 {
		t.Fatalf("session totals = %+v", report)
	}
	if report.messages != 12 || report.userPrompts != 3 || report.assistantMessages != 6 {
		t.Fatalf("conversation totals = messages:%d prompts:%d assistant:%d", report.messages, report.userPrompts, report.assistantMessages)
	}
	if report.toolCalls != 4 || report.toolErrors != 2 || report.thinkingBlocks != 2 {
		t.Fatalf("work totals = calls:%d errors:%d thinking:%d", report.toolCalls, report.toolErrors, report.thinkingBlocks)
	}
	if len(report.projects) != 2 || len(report.days) != 3 || thinkBackActiveMonths(report.months) != 2 {
		t.Fatalf("breadth = projects:%d days:%d months:%d", len(report.projects), len(report.days), thinkBackActiveMonths(report.months))
	}
	if report.modelMix["model-a"] != 2 || report.modelMix["model-b"] != 1 || report.modelMix["old-model"] != 0 {
		t.Fatalf("model mix = %#v", report.modelMix)
	}
	if report.toolMix["Read"] != 1 || report.toolMix["Edit"] != 1 || report.toolMix["Git"] != 1 || report.toolMix["WebSearch"] != 1 {
		t.Fatalf("tool mix = %#v", report.toolMix)
	}
	if report.themeMix["Building & feature work"] != 1 || report.themeMix["Debugging & reliability"] != 1 || report.themeMix["Release & operations"] != 1 {
		t.Fatalf("theme mix = %#v", report.themeMix)
	}
	if got := thinkBackBusiestMonth(report.months); got != "March · 2 session(s)" {
		t.Fatalf("busiest month = %q", got)
	}
	if got := thinkBackLongestStreak(report.days); got != 2 {
		t.Fatalf("longest streak = %d", got)
	}
	if report.deepestMessages != 4 || report.deepestTools != 2 {
		t.Fatalf("deepest session = %d messages / %d tools", report.deepestMessages, report.deepestTools)
	}

	out := stripANSI(renderThinkBack(report, now))
	for _, want := range []string{
		"Think Back · 2026", "3 total", "1 sub-agent", "12 new messages", "4 calls", "50% successful",
		"2 project(s)", "model-a × 2", "Read × 1", "Building & feature work", "March · 2 session(s)", "2 consecutive active day(s)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("review output missing %q:\n%s", want, out)
		}
	}
}

func TestCollectThinkBack_UsesLogicalHistoryReplacement(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, time.April, 5, 9, 0, 0, 0, time.UTC)
	if err := store.WriteHeaderFull(session.Header{ID: "replaced", CreatedAt: created, Model: "model"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("replaced", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "superseded secret prompt"}}}); err != nil {
		t.Fatal(err)
	}
	logical := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Analyze the current code"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	}
	cursor := session.NewHistoryCursor(nil)
	if err := store.ReplaceHistoryAndMark("replaced", logical, &cursor); err != nil {
		t.Fatal(err)
	}

	report, err := collectThinkBack(store, time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.messages != 2 || report.userPrompts != 1 || report.themeMix["Research & analysis"] != 1 {
		t.Fatalf("logical history not respected: %+v", report)
	}
}

func TestCollectThinkBack_BranchHistoryReplacementKeepsOnlyRealInheritedPrefix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replacement func(base []llm.Message) []llm.Message
	}{
		{
			name: "clear then continue",
			replacement: func([]llm.Message) []llm.Message {
				return []llm.Message{
					{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new branch work"}}},
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "new answer"}}},
				}
			},
		},
		{
			name: "undo preserving inherited prefix",
			replacement: func(base []llm.Message) []llm.Message {
				return append(append([]llm.Message(nil), base...),
					llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "replacement branch work"}}},
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "replacement answer"}}},
				)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			created := time.Date(2026, time.April, 5, 9, 0, 0, 0, time.UTC)
			base := []llm.Message{
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "parent work"}}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "parent answer"}}},
			}
			writeThinkBackFixture(t, store, session.Header{ID: "parent", CreatedAt: created, Model: "model"}, base)
			writeThinkBackFixture(t, store, session.Header{
				ID: "branch", CreatedAt: created.Add(time.Hour), Model: "model",
				ForkedFrom: &pubsession.ForkRef{SessionID: "parent", MessageCount: len(base)},
			}, base)
			logical := tc.replacement(base)
			cursor := session.NewHistoryCursor(base)
			if err := store.ReplaceHistoryAndMark("branch", logical, &cursor); err != nil {
				t.Fatal(err)
			}

			report, err := collectThinkBack(store, created.Add(2*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if report.sessions != 2 || report.messages != 4 || report.userPrompts != 2 || report.assistantMessages != 2 {
				t.Fatalf("branch replacement totals = %+v", report)
			}
		})
	}
}

func TestCollectThinkBack_MissingStoreIsAnEmptyYear(t *testing.T) {
	report, err := collectThinkBack(&session.Store{Dir: filepath.Join(t.TempDir(), "not-created")}, time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.year != 2026 || report.sessions != 0 {
		t.Fatalf("empty report = %+v", report)
	}
}

func TestCollectThinkBack_IgnoresTimingSidecars(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, time.April, 5, 9, 0, 0, 0, time.UTC)
	writeThinkBackFixture(t, store, session.Header{ID: "timed", CreatedAt: created, Model: "model"}, nil)
	store.NewTimingRecorder("timed").Record("Read", time.Millisecond, false)

	report, err := collectThinkBack(store, created.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.sessions != 1 || report.skippedSessions != 0 {
		t.Fatalf("timing sidecar was treated as a session: %+v", report)
	}
}

func TestThinkBackOutput_DoesNotExposeSessionSecrets(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Build the credential-shaped fixture at runtime so repository secret
	// scanners do not mistake this redaction test for a committed credential.
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz" + "ABCDEFGHIJ"
	if len(secret) != len("ghp_")+36 {
		t.Fatalf("fixture token length = %d", len(secret))
	}
	writeThinkBackFixture(t, store, session.Header{
		ID: "private", CreatedAt: time.Date(2026, time.June, 1, 9, 0, 0, 0, time.UTC),
		Model: secret, WorkDir: "/private/" + secret, Title: "title " + secret,
	}, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "remember " + secret}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "private-tool", ToolName: secret,
			ToolInput: map[string]any{"token": secret},
		}}},
	})

	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	report, err := collectThinkBack(store, now)
	if err != nil {
		t.Fatal(err)
	}
	out := stripANSI(renderThinkBack(report, now))
	if strings.Contains(out, secret) || strings.Contains(out, "remember") || strings.Contains(out, "/private/") || strings.Contains(out, "private-tool") {
		t.Fatalf("think-back leaked session-derived private content:\n%s", out)
	}
	if !strings.Contains(out, "[private] × 1") {
		t.Fatalf("redacted aggregate label missing:\n%s", out)
	}
}

func TestCmdThoughts_PreservesLatestThinkingTrace(t *testing.T) {
	loop := &agent.Loop{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "question"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "thinking", Text: "latest trace"}, {Type: "text", Text: "answer"}}},
	}}
	out := stripANSI(cmdThoughts(&REPL{Loop: loop}, ""))
	if !strings.Contains(out, "Last Thinking") || !strings.Contains(out, "latest trace") {
		t.Fatalf("/thoughts did not preserve trace behavior:\n%s", out)
	}
	if got := cmdThoughts(&REPL{}, "extra"); got != "usage: /thoughts" {
		t.Fatalf("unexpected /thoughts arg handling: %q", got)
	}
}

func TestThinkBackCommands_AppearInCatalogWithCanonicalAlias(t *testing.T) {
	registry := BuildREPLCommands()
	canonical := registry.Get("think-back")
	if canonical == nil || canonical.Name != "think-back" {
		t.Fatalf("canonical /think-back registration = %+v", canonical)
	}
	alias := registry.Get("thinkback")
	if alias == nil || alias.Name != "think-back" {
		t.Fatalf("/thinkback alias = %+v, want canonical /think-back", alias)
	}
	thoughts := registry.Get("thoughts")
	if thoughts == nil || thoughts.Name != "thoughts" {
		t.Fatalf("/thoughts registration = %+v", thoughts)
	}

	m := newSlashTestModel(t)
	counts := make(map[string]int)
	for _, command := range m.commandCatalog() {
		counts[command.Name]++
	}
	if counts["think-back"] != 1 || counts["thoughts"] != 1 {
		t.Fatalf("think command catalog counts = %#v", counts)
	}
	if counts["thinkback"] != 0 {
		t.Fatalf("alias /thinkback should not duplicate the canonical palette row: %#v", counts)
	}
}

func TestThinkBackCommands_DispatchThroughRealSubmitPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	writeThinkBackFixture(t, store, session.Header{
		ID: "dispatch", CreatedAt: now.Add(-time.Minute), Model: "dispatch-model",
	}, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Implement a feature"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	})

	for _, input := range []string{"/think-back", "/thinkback"} {
		t.Run(input, func(t *testing.T) {
			m := newSlashTestModel(t)
			m.session = store
			before := len(m.messages)
			m.input.SetValue(input)
			pressEnter(t, m)
			if len(m.messages) <= before {
				t.Fatalf("%s did not append a result", input)
			}
			out := stripANSI(m.messages[len(m.messages)-1].Content)
			if !strings.Contains(out, "Think Back · "+now.Format("2006")) ||
				!strings.Contains(out, "dispatch-model × 1") {
				t.Fatalf("%s dispatched unexpected output:\n%s", input, out)
			}
		})
	}

	t.Run("/thoughts", func(t *testing.T) {
		m := newSlashTestModel(t)
		m.loop.Restore([]llm.Message{{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				{Type: "thinking", Text: "dispatch trace"},
				{Type: "text", Text: "answer"},
			},
		}})
		before := len(m.messages)
		m.input.SetValue("/thoughts")
		pressEnter(t, m)
		if len(m.messages) <= before {
			t.Fatal("/thoughts did not append a result")
		}
		out := stripANSI(m.messages[len(m.messages)-1].Content)
		if !strings.Contains(out, "Last Thinking") || !strings.Contains(out, "dispatch trace") {
			t.Fatalf("/thoughts dispatched unexpected output:\n%s", out)
		}
	})
}
