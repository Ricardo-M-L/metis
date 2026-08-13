package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func catalogEntryByName(entries []CommandCatalogEntry, name string) (CommandCatalogEntry, bool) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return CommandCatalogEntry{}, false
}

func TestEffectiveCommandCatalogIsSharedSortedMetadata(t *testing.T) {
	visible := true
	repl := NewREPLCommandRegistry()
	repl.Register(REPLCommand{
		Name: "alpha", Aliases: []string{"a"}, Description: "alpha command",
		ArgumentHint: "[path]", Source: "test-repl", Category: "testing",
	})
	repl.Register(REPLCommand{Name: "hidden", Visible: func() bool { return false }})
	repl.Register(REPLCommand{Name: "conditional", Visible: func() bool { return visible }})
	repl.Register(REPLCommand{Name: "disabled", Enabled: func() bool { return false }})

	sl := slash.NewRegistry()
	sl.Register(slash.Cmd{
		Name: "zeta", Aliases: []string{"z"}, Description: "zeta command",
		ArgumentHint: "<topic>", Source: "mcp:test", Category: "mcp",
	})

	m := &Model{cmds: repl, slash: sl}
	got := m.commandCatalog()
	var names []string
	for _, entry := range got {
		names = append(names, entry.Name)
	}
	// Built-ins lead; the synthetic test and MCP sources occupy the final tier
	// and remain alphabetical within it.
	want := []string{"conditional", "alpha", "zeta"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("effective catalog names = %v, want %v", names, want)
	}
	alpha, ok := catalogEntryByName(got, "alpha")
	if !ok || alpha.ArgumentHint != "[path]" || alpha.Source != "test-repl" || alpha.Category != "testing" ||
		!reflect.DeepEqual(alpha.Aliases, []string{"a"}) {
		t.Fatalf("alpha metadata was not preserved: %+v", alpha)
	}
	zeta, ok := catalogEntryByName(got, "zeta")
	if !ok || zeta.ArgumentHint != "<topic>" || zeta.Source != "mcp:test" || zeta.Category != "mcp" {
		t.Fatalf("zeta metadata was not preserved: %+v", zeta)
	}

	// Palette and readline completion are projections of this same catalog.
	m.palFilter = ""
	m.matchCommands()
	var paletteNames []string
	for _, entry := range m.palMatched {
		paletteNames = append(paletteNames, entry.Name)
	}
	if candidates := (&replCandidates{REPL: repl, Slash: sl}).Candidates(); !reflect.DeepEqual(candidates, names) || !reflect.DeepEqual(paletteNames, names) {
		t.Fatalf("catalog projections drifted: catalog=%v palette=%v completion=%v", names, paletteNames, candidates)
	}

	visible = false
	for _, entry := range m.commandCatalog() {
		if entry.Name == "conditional" {
			t.Fatal("Visible predicate was not re-evaluated")
		}
	}
}

func TestEffectiveCommandCatalogOrdersAndProjectsSourceTiers(t *testing.T) {
	repl := NewREPLCommandRegistry()
	repl.Register(REPLCommand{Name: "z-built", Source: "repl", Description: "built"})
	repl.Register(REPLCommand{Name: "a-built", Source: "repl", Description: "built"})
	sl := slash.NewRegistry()
	sl.Register(slash.Cmd{Name: "z-user", Source: "user", Category: "custom", Description: "user"})
	sl.Register(slash.Cmd{Name: "a-project", Source: "project", Category: "custom", Description: "project"})
	sl.Register(slash.Cmd{Name: "mcp-cmd", Source: "mcp:evil\x1b]52;c;x\x07", Category: "mcp", Description: "mcp"})

	m := &Model{cmds: repl, slash: sl, width: 120, height: 40, showPalette: true}
	entries := m.commandCatalog()
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name)
	}
	want := []string{"a-built", "z-built", "z-user", "a-project", "mcp-cmd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source-tier catalog order = %v, want %v", got, want)
	}

	helpRows := m.helpCommandsRows()
	var help strings.Builder
	for _, row := range helpRows {
		help.WriteString(row.Heading)
		help.WriteByte(' ')
		help.WriteString(row.Key)
		help.WriteByte(' ')
		help.WriteString(row.Value)
		help.WriteByte('\n')
	}
	for _, heading := range []string{"Built-in commands", "User commands", "Project commands", "MCP and other commands"} {
		if !strings.Contains(help.String(), heading) {
			t.Fatalf("help missing source heading %q:\n%s", heading, help.String())
		}
	}
	if strings.ContainsRune(help.String(), '\x1b') || strings.ContainsRune(help.String(), '\x07') {
		t.Fatalf("unsafe source badge reached help: %q", help.String())
	}

	m.palFilter = "mcp-cmd"
	m.matchCommands()
	palette := stripANSI(renderPalette(m))
	if !strings.Contains(palette, "[mcp:evil52cx]") || strings.ContainsRune(palette, '\x1b') || strings.ContainsRune(palette, '\x07') {
		t.Fatalf("palette source badge is missing or unsafe: %q", palette)
	}
}

func TestREPLRegistryRepeatedRegistrationAndAliasCollision(t *testing.T) {
	r := NewREPLCommandRegistry()
	r.Register(REPLCommand{Name: "check", Aliases: []string{"old"}, Description: "old"})
	r.Register(REPLCommand{Name: "check", Aliases: []string{"new"}, Description: "new"})
	r.Register(REPLCommand{Name: "new", Description: "canonical"})

	check := r.Resolve("check")
	if check == nil || check.Description != "new" {
		t.Fatalf("latest canonical registration did not win: %+v", check)
	}
	if r.Resolve("old") != nil {
		t.Fatal("stale alias from replaced REPL registration remained callable")
	}
	if got := r.Resolve("new"); got == nil || got.Name != "new" {
		t.Fatalf("canonical REPL name was shadowed by alias: %+v", got)
	}

	catalog := r.Catalog()
	if len(catalog) != 2 {
		t.Fatalf("effective REPL catalog retained duplicate rows: %+v", catalog)
	}
	entry, ok := func() (REPLCommand, bool) {
		for _, cmd := range catalog {
			if cmd.Name == "check" {
				return cmd, true
			}
		}
		return REPLCommand{}, false
	}()
	if !ok || len(entry.Aliases) != 0 {
		t.Fatalf("colliding alias remained advertised: %+v", entry)
	}
}

func TestEffectiveCatalogUsesCanonicalAliasAndPreferredSlashOwner(t *testing.T) {
	repl := NewREPLCommandRegistry()
	repl.Register(REPLCommand{Name: "diff", Description: "raw git diff", Handler: cmdGitDiff})
	repl.Register(REPLCommand{Name: "diff-view", Aliases: []string{"dv"}, Description: "legacy viewer", Handler: cmdDiffView})
	sl := slash.NewRegistry()
	sl.Register(slash.Cmd{
		Name: "diff", Aliases: []string{"diff-view", "dv"},
		Description: "interactive viewer", Source: "slash", Category: "git",
	})

	entries := effectiveCommandCatalog(repl, sl)
	if len(entries) != 1 {
		t.Fatalf("diff catalog should have one canonical row, got %+v", entries)
	}
	diff := entries[0]
	if diff.Name != "diff" || diff.Description != "interactive viewer" ||
		!reflect.DeepEqual(diff.Aliases, []string{"diff-view", "dv"}) {
		t.Fatalf("preferred slash metadata/aliases = %+v", diff)
	}
	if got, ok := sl.CanonicalName("dv"); !ok || got != "diff" {
		t.Fatalf("slash alias canonical resolve = %q, %v", got, ok)
	}
	if got, ok := repl.CanonicalName("dv"); !ok || got != "diff-view" {
		t.Fatalf("REPL alias canonical resolve = %q, %v", got, ok)
	}
}

func TestPaletteFuzzySearchUsesCanonicalCatalogWithoutDuplicates(t *testing.T) {
	m := newSlashTestModel(t)
	m.palFilter = "ssif"
	m.matchCommands()

	count := 0
	for _, cmd := range m.palMatched {
		if cmd.Name == "session-info" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("fuzzy search for session-info returned %d canonical rows: %+v", count, m.palMatched)
	}
}

func TestPaletteAliasAnnotationAndMatchTierOrdering(t *testing.T) {
	repl := NewREPLCommandRegistry()
	repl.Register(REPLCommand{Name: "alpha", Aliases: []string{"zz"}})
	repl.Register(REPLCommand{Name: "zz-top"})
	repl.Register(REPLCommand{Name: "fizzzy"})
	repl.Register(REPLCommand{Name: "zany-zebra"})
	m := &Model{cmds: repl, width: 100, height: 40, showPalette: true, palFilter: "zz"}
	m.matchCommands()

	got := make([]string, 0, len(m.palMatched))
	for _, cmd := range m.palMatched {
		got = append(got, cmd.Name)
	}
	want := []string{"alpha", "zz-top", "fizzzy", "zany-zebra"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("palette match tiers = %v, want exact > prefix > contains > fuzzy %v", got, want)
	}
	if alias := m.palMatched[0].MatchedAlias; alias != "zz" {
		t.Fatalf("exact alias match annotation = %q, want zz", alias)
	}
	if out := stripANSI(renderPalette(m)); !strings.Contains(out, "/alpha (zz)") {
		t.Fatalf("palette did not explain canonical alias match:\n%s", out)
	}
	if catalog := m.commandCatalog(); catalog[0].MatchedAlias != "" {
		t.Fatalf("transient alias annotation leaked into canonical help/catalog metadata: %+v", catalog[0])
	}
}

func TestCommandCatalogIncludesSlashOnlyAndCustomCommands(t *testing.T) {
	m := newSlashTestModel(t)
	m.slash.Register(slash.Cmd{
		Name:        "project-check",
		Description: "run the project check",
		Custom:      true,
		Handler: func(string) (string, slash.Signal) {
			return "check", slash.SignalCustomPrompt
		},
	})

	got := map[string]int{}
	for _, cmd := range m.commandCatalog() {
		got[cmd.Name]++
	}
	for _, name := range []string{"help", "clear", "undo", "review", "project-check"} {
		if got[name] != 1 {
			t.Fatalf("catalog count for %q = %d, want 1", name, got[name])
		}
	}
	for _, alias := range []string{"new", "reset"} {
		if got[alias] != 0 {
			t.Fatalf("new-session alias %q must not duplicate canonical /clear in catalog", alias)
		}
	}
	clear, ok := catalogEntryByName(m.commandCatalog(), "clear")
	if !ok || !reflect.DeepEqual(clear.Aliases, []string{"new", "reset"}) {
		t.Fatalf("canonical /clear aliases = %+v", clear)
	}

	m.palFilter = "project-c"
	m.matchCommands()
	if len(m.palMatched) == 0 || m.palMatched[0].Name != "project-check" {
		t.Fatalf("custom slash missing from palette: %+v", m.palMatched)
	}
}

func TestToolsetsAliasesCanonicalToolsPicker(t *testing.T) {
	m := newSlashTestModel(t)
	if cmd := m.cmds.Get("toolsets"); cmd == nil || cmd.Name != "tools" {
		t.Fatalf("REPL /toolsets alias = %+v, want canonical /tools", cmd)
	}
	handled, _, sig, _ := m.slash.Parse("/toolsets")
	if !handled || sig != slash.SignalTools {
		t.Fatalf("slash /toolsets routed to handled=%v signal=%v, want SignalTools", handled, sig)
	}

	m.input.SetValue("/toolsets")
	pressEnter(t, m)
	if _, ok := m.activeScreen.(*screen.PickerScreen); !ok {
		t.Fatalf("/toolsets should open the /tools picker, got %T", m.activeScreen)
	}
}

// Claude Code exposes submit as the Enter-bound `chat:submit` action, not as a
// slash command. Keep it out of both Metis command registries so the palette
// does not advertise a command with no meaningful slash semantics.
func TestSubmitIsAKeyActionNotSlashCommand(t *testing.T) {
	m := newSlashTestModel(t)
	if cmd := m.cmds.Get("submit"); cmd != nil {
		t.Fatalf("REPL unexpectedly exposes /submit: %+v", cmd)
	}
	if cmd, ok := m.slash.Get("submit"); ok || cmd != nil {
		t.Fatalf("slash registry unexpectedly exposes /submit: %+v", cmd)
	}
	for _, cmd := range m.commandCatalog() {
		if cmd.Name == "submit" {
			t.Fatal("palette unexpectedly advertises /submit")
		}
	}
}

func TestHelpUsesRealCustomCommandsNotSkills(t *testing.T) {
	m := newSlashTestModel(t)
	m.slash.Register(slash.Cmd{
		Name:        "release-check",
		Description: "validate a release",
		Custom:      true,
		Handler: func(string) (string, slash.Signal) {
			return "release", slash.SignalCustomPrompt
		},
	})

	var text strings.Builder
	for _, row := range m.helpCustomCommandsRows() {
		text.WriteString(row.Key)
		text.WriteString(" ")
		text.WriteString(row.Value)
		text.WriteString("\n")
	}
	if !strings.Contains(text.String(), "/release-check") {
		t.Fatalf("custom help missing real slash command:\n%s", text.String())
	}
	if strings.Contains(text.String(), "SKILL.md") {
		t.Fatalf("custom-command help still advertises skills as slash commands:\n%s", text.String())
	}
}

func TestResetAliasBelongsToNewSessionNotClear(t *testing.T) {
	m := newSlashTestModel(t)
	if got := m.cmds.Get("reset"); got != nil {
		t.Fatalf("REPL /clear must not shadow slash /reset alias: %+v", got)
	}
	handled, _, sig, _ := m.slash.Parse("/reset")
	if !handled || sig != slash.SignalNew {
		t.Fatalf("/reset routed to handled=%v signal=%v, want SignalNew", handled, sig)
	}
}

func TestResetDoesNotExecuteBeforeEnter(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.AppendUser("keep this turn")
	before := m.loop.History()

	typeRunes(t, m, "/reset")

	if got := m.input.Value(); got != "/reset" {
		t.Fatalf("typing /reset changed the editor before Enter: %q", got)
	}
	after := m.loop.History()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("typing /reset executed /clear before Enter:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestNewSessionAliasesRequireEnterAndShareRealDispatch(t *testing.T) {
	for _, input := range []string{"/new", "/clear", "/reset"} {
		t.Run(input, func(t *testing.T) {
			m, _ := newSessionSwitchModel(t, permission.ModeAsk)
			reg := slash.NewRegistry()
			slash.RegisterAll(reg, nil)
			m.slash = reg
			m.loop.AppendUser("preserve this in the previous session")
			oldID := m.sessionID

			typeRunes(t, m, input)
			if m.sessionID != oldID || len(m.loop.History()) != 1 {
				t.Fatalf("typing %s before Enter changed session state: id=%q history=%d", input, m.sessionID, len(m.loop.History()))
			}

			pressEnter(t, m)
			if m.sessionID == oldID || m.sessionID == "" {
				t.Fatalf("%s did not activate a fresh session: old=%q new=%q", input, oldID, m.sessionID)
			}
			if got := len(m.loop.History()); got != 0 {
				t.Fatalf("%s carried old history into fresh session: %d message(s)", input, got)
			}
		})
	}
}

func TestREPLSemanticCommandMappingsUseCanonicalHandlers(t *testing.T) {
	r := BuildREPLCommands()
	loop := &agent.Loop{Jobs: jobs.NewRegistry(t.TempDir())}
	repl := &REPL{Loop: loop}

	quick := r.Resolve("quick")
	if quick == nil || !strings.Contains(quick.Handler(repl, "on"), "effort=low") || !loop.FastEnabled() {
		t.Fatalf("/quick is not mapped to quick-output behavior: %+v", quick)
	}
	if r.Resolve("fast") != nil {
		t.Fatal("legacy /fast is still registered")
	}

	todos := r.Resolve("todos")
	if todos == nil || !strings.HasPrefix(todos.Handler(repl, ""), "todos:") {
		t.Fatalf("/todos is not mapped to the TodoRead handler: %+v", todos)
	}
	tasks := r.Resolve("tasks")
	if tasks == nil || !strings.Contains(tasks.Handler(repl, "list"), "background jobs") {
		t.Fatalf("/tasks is not mapped to background jobs: %+v", tasks)
	}

	sessionCmd := r.Resolve("session")
	if sessionCmd == nil || !strings.Contains(sessionCmd.Handler(repl, "status"), "local only") {
		t.Fatalf("/session is not mapped to sharing status: %+v", sessionCmd)
	}
	if info := r.Resolve("sid"); info == nil || info.Name != "session-info" {
		t.Fatalf("/sid does not resolve to /session-info: %+v", info)
	}

	if memory := r.Resolve("memory"); memory == nil || !strings.Contains(memory.Description, "memory ops") {
		t.Fatalf("original REPL /memory registration changed: %+v", memory)
	}
	if agents := r.Resolve("av"); agents == nil || agents.Name != "agents" || !strings.Contains(agents.Description, "tree") {
		t.Fatalf("original /agents live-tree registration changed: %+v", agents)
	}
}

func TestRichSlashHandlersOwnConflictingTUICommands(t *testing.T) {
	m := newSlashTestModel(t)
	for _, name := range []string{"memory", "doctor"} {
		cmd := m.cmds.Get(name)
		if cmd == nil {
			t.Fatalf("test fixture missing legacy REPL command %q", name)
		}
		if !preferSlashInTUI(cmd.Name) {
			t.Fatalf("%q should be owned by the richer slash path", name)
		}
	}

	m.input.SetValue("/memory path")
	pressEnter(t, m)
	last := m.messages[len(m.messages)-1].Content
	if !strings.Contains(last, "memory") || strings.Contains(last, "Core Memory") {
		t.Fatalf("/memory args were swallowed by legacy REPL handler: %q", last)
	}

	if preferSlashInTUI("agents") {
		t.Fatal("/agents must remain owned by the live multi-agent tree command")
	}
	if legacy := m.cmds.Get("agents-view"); legacy != nil {
		t.Fatalf("legacy /agents-view command is still registered: %+v", legacy)
	}
	if canonical := m.cmds.Get("agents"); canonical == nil || !strings.Contains(canonical.Description, "tree") {
		t.Fatalf("canonical /agents tree command missing: %+v", canonical)
	}
	if alias := m.cmds.Get("av"); alias == nil || alias.Name != "agents" {
		t.Fatalf("/av should resolve to canonical /agents: %+v", alias)
	}
	for command, want := range map[string]bool{
		"/agents":      true,
		"/av":          true,
		"/agents-view": false,
	} {
		if got := isAgentsViewCommand(command); got != want {
			t.Errorf("isAgentsViewCommand(%q) = %v, want %v", command, got, want)
		}
	}
	// /memory path opens a modal. Use a fresh model so this assertion tests
	// /agents routing instead of sending Enter to the still-open memory screen.
	agentsModel := newSlashTestModel(t)
	agentsModel.input.SetValue("/agents")
	_, _ = agentsModel.handleSubmit()
	if _, ok := agentsModel.activeScreen.(*screen.MultiAgentScreen); !ok {
		t.Fatalf("/agents did not open the tree screen: %T", agentsModel.activeScreen)
	}
	agentsView := agentsModel.activeScreen.View()
	if !strings.Contains(agentsView, "/agents") || !strings.Contains(agentsView, "0 agents") ||
		strings.Contains(agentsView, "single agent mode") {
		t.Fatalf("/agents did not use the live roster backend: %q", agentsView)
	}
}

func TestRecapReturnsLatestStructuralRecap(t *testing.T) {
	m := newSlashTestModel(t)
	m.messages = append(m.messages,
		Message{Role: "recap", Content: "read 2 files"},
		Message{Role: "info", Content: "later status"},
		Message{Role: "recap", Content: "edited 3 files · tests passed"},
	)
	m.input.SetValue("/recap")
	pressEnter(t, m)
	last := m.messages[len(m.messages)-1].Content
	if !strings.Contains(last, "edited 3 files") || strings.Contains(last, "scroll up") {
		t.Fatalf("/recap did not return latest recap: %q", last)
	}
}

func TestRetryReplacesPriorTurnAndImmediatelyResubmits(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.Restore([]llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "try this again"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "old response"}}},
	})
	m.messages = append(m.messages,
		Message{Role: "user", Content: "try this again"},
		Message{Role: "assistant", Content: "old response"},
	)
	m.input.SetValue("/retry")
	cmd := pressEnter(t, m)

	if cmd == nil || !m.turnActive {
		t.Fatal("/retry did not immediately start a replacement turn")
	}
	hist := m.loop.History()
	if len(hist) != 1 || hist[0].Role != llm.RoleUser || hist[0].Content[0].Text != "try this again" {
		t.Fatalf("retry history=%+v, want only replacement user prompt before response", hist)
	}
	for _, msg := range m.messages {
		if msg.Role == "assistant" && msg.Content == "old response" {
			t.Fatal("retry retained the old visible assistant response")
		}
	}
}
