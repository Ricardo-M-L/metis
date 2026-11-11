package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
)

func historicalSubAgentFixture(t *testing.T, dir, id, parent, name, status, output, reason string) string {
	t.Helper()
	transcript, err := agent.NewSubAgentTranscript(dir, id, pubsess.Header{
		ID: id, SubAgentOf: parent, TeammateName: name, CreatedAt: time.Now(), Status: "running", WorkDir: "/fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = transcript.AppendMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: output}}}); err != nil {
		t.Fatal(err)
	}
	if status != "" {
		terminal := agent.SubAgentTerminal{Status: status, Output: output, Result: output, StopHint: reason, EndedAt: time.Now()}
		if status == "failed" {
			terminal.ExitError = "sub-agent incomplete: " + reason
		}
		if err = transcript.AppendTerminal(terminal); err != nil {
			t.Fatal(err)
		}
	}
	path := transcript.Path()
	if err = transcript.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSubAgentHistoryOutputAfterRosterReset(t *testing.T) {
	dir := t.TempDir()
	historicalSubAgentFixture(t, dir, "agt-historical", "parent", "战略路线", "failed", "partial surviving answer", "max_tokens")
	tool := NewSubAgentOutput(permission.New(permission.ModeBypass), agent.NewRoster(0)).WithSessionPersistence(dir, "parent")
	for _, input := range []map[string]any{{"agent_id": "agt-historical"}, {"name": "战略路线"}} {
		result, err := tool.Execute(context.Background(), input)
		if err != nil || result == nil || result.IsError {
			t.Fatalf("cold query failed: %+v %v", result, err)
		}
		for _, expected := range []string{"status=failed", "load_state=not_loaded", "stop_reason=max_tokens", "per-response output limit", "partial surviving answer"} {
			if !strings.Contains(result.Output, expected) {
				t.Errorf("missing %q in %q", expected, result.Output)
			}
		}
	}
	listed, err := NewSubAgentList(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), nil)
	if err != nil || listed == nil || listed.IsError || !strings.Contains(listed.Output, "agt-historical") || !strings.Contains(listed.Output, "max_tokens") {
		t.Fatalf("cold list lost failed transcript: %+v %v", listed, err)
	}
}

func TestSubAgentHistoryRejectsForeignParentEvenWhenLive(t *testing.T) {
	dir := t.TempDir()
	historicalSubAgentFixture(t, dir, "agt-foreign", "other-parent", "foreign", "completed", "FOREIGN_SECRET_MARKER", "end_turn")
	roster := agent.NewRoster(0)
	child := &agent.Teammate{AgentID: "agt-foreign", Name: "foreign"}
	if err := roster.Register(child); err != nil {
		t.Fatal(err)
	}
	child.Finish(agent.StatusCompleted, "FOREIGN_SECRET_MARKER", nil, "end_turn")
	tool := NewSubAgentOutput(permission.New(permission.ModeBypass), roster).WithSessionPersistence(dir, "parent")
	for _, input := range []map[string]any{{"agent_id": "agt-foreign"}, {"name": "foreign"}} {
		result, err := tool.Execute(context.Background(), input)
		if err != nil || result == nil || !result.IsError || strings.Contains(result.Output, "FOREIGN_SECRET_MARKER") || !strings.Contains(result.Output, "not_found") {
			t.Fatalf("foreign parent leaked: %+v %v", result, err)
		}
	}
	listed, err := NewSubAgentList(permission.New(permission.ModeBypass), roster).WithSessionPersistence(dir, "parent").Execute(context.Background(), nil)
	if err != nil || listed == nil || strings.Contains(listed.Output, "agt-foreign") || strings.Contains(listed.Output, "FOREIGN_SECRET_MARKER") {
		t.Fatalf("foreign list leaked: %+v %v", listed, err)
	}
}

func TestSubAgentHistoryNameAmbiguityRequiresStableID(t *testing.T) {
	dir := t.TempDir()
	historicalSubAgentFixture(t, dir, "agt-first", "parent", "配置审查", "completed", "first", "end_turn")
	historicalSubAgentFixture(t, dir, "agt-second", "parent", "配置审查", "completed", "second", "end_turn")
	result, err := NewSubAgentOutput(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"name": "配置审查"})
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "ambiguous") || !strings.Contains(result.Output, "agent_id") {
		t.Fatalf("same-name generations silently selected: %+v %v", result, err)
	}
}

func TestSubAgentHistoryWithoutTerminalIsNotInventedCompleted(t *testing.T) {
	dir := t.TempDir()
	historicalSubAgentFixture(t, dir, "agt-interrupted", "parent", "interrupted", "", "durable partial", "")
	result, err := NewSubAgentOutput(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"agent_id": "agt-interrupted"})
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Output, "load_state=not_loaded") || !strings.Contains(result.Output, "status=unknown") || !strings.Contains(result.Output, "durable partial") || strings.Contains(result.Output, "status=completed") {
		t.Fatalf("nonloaded unacknowledged run misreported: %+v %v", result, err)
	}
}

func TestSubAgentHistoryErrorsAndPathGuards(t *testing.T) {
	for _, kind := range []string{"malformed", "malformed_body", "oversize", "symlink", "traversal", "unbound"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			id := "agt-target"
			path := historicalSubAgentFixture(t, dir, id, "parent", "target", "failed", "private partial", "max_tokens")
			parent := "parent"
			switch kind {
			case "malformed":
				if err := os.WriteFile(path, []byte("{broken json}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "malformed_body":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.WriteString("{broken body}\n"); err != nil {
					t.Fatal(err)
				}
				f.Close()
			case "oversize":
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.Truncate(17 << 20); err != nil {
					t.Fatal(err)
				}
				f.Close()
			case "symlink":
				other := historicalSubAgentFixture(t, t.TempDir(), id, "parent", "target", "completed", "SYMLINK_SECRET_MARKER", "end_turn")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "traversal":
				id = filepath.Join("..", "agt-target")
			case "unbound":
				parent = ""
			}
			result, err := NewSubAgentOutput(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, parent).Execute(context.Background(), map[string]any{"agent_id": id})
			if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "read_error") || strings.Contains(result.Output, "SYMLINK_SECRET_MARKER") {
				t.Fatalf("unsafe read was not rejected: %+v %v", result, err)
			}
		})
	}
}

func TestSubAgentHistoryListPaginationAndLiveDeduplication(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"agt-page-a", "agt-page-b", "agt-page-c"} {
		historicalSubAgentFixture(t, dir, id, "parent", id, "completed", "saved-"+id, "end_turn")
	}
	roster := agent.NewRoster(0)
	live := &agent.Teammate{AgentID: "agt-page-b", Name: "agt-page-b"}
	if err := roster.Register(live); err != nil {
		t.Fatal(err)
	}
	list := NewSubAgentList(permission.New(permission.ModeBypass), roster).WithSessionPersistence(dir, "parent")
	result, err := list.Execute(context.Background(), map[string]any{"limit": 2.0, "offset": 0.0})
	if err != nil || result.IsError || !strings.Contains(result.Output, "known_total=3") || !strings.Contains(result.Output, "next_offset=2") || strings.Count(result.Output, "agent_id=") != 2 {
		t.Fatalf("unexpected bounded list: %+v %v", result, err)
	}
	result, err = list.Execute(context.Background(), map[string]any{"limit": 2, "offset": 2})
	if err != nil || result.IsError || strings.Count(result.Output, "agent_id=") != 1 || strings.Contains(result.Output, "more_saved_records=true") {
		t.Fatalf("unexpected final list page: %+v %v", result, err)
	}
	output, err := NewSubAgentOutput(permission.New(permission.ModeBypass), roster).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"agent_id": "agt-page-b"})
	if err != nil || output.IsError || !strings.Contains(output.Output, "status=running") || !strings.Contains(output.Output, "load_state=live") || strings.Contains(output.Output, "saved-agt-page-b") {
		t.Fatalf("live state must override historical terminal: %+v %v", output, err)
	}
}

func TestSubAgentHistoryScopeRebindPreservesWrapperAndOriginal(t *testing.T) {
	dir := t.TempDir()
	gate := permission.New(permission.ModeBypass)
	historicalSubAgentFixture(t, dir, "agt-root-child", "root", "root-child", "completed", "ROOT_ONLY_MARKER", "end_turn")
	historicalSubAgentFixture(t, dir, "agt-nested-child", "nested", "nested-child", "completed", "NESTED_ONLY_MARKER", "end_turn")
	original := NewSubAgentOutput(gate, nil).WithSessionPersistence(dir, "root")
	registry := tools.NewRegistry()
	registry.Register(agentPermissionBoundTool{inner: original, gate: gate})
	registry.Register(NewSubAgentList(gate, nil).WithSessionPersistence(dir, "root"))
	rebindSubAgentQueryTools(registry, "nested")
	tool, _ := registry.Get("SubAgentOutput")
	if _, ok := tool.(agentPermissionBoundTool); !ok {
		t.Fatal("rebind discarded child permission wrapper")
	}
	for _, fixture := range []struct {
		id     string
		denied bool
	}{{"agt-root-child", true}, {"agt-nested-child", false}} {
		result, err := tool.Execute(context.Background(), map[string]any{"agent_id": fixture.id})
		if err != nil || result == nil || result.IsError != fixture.denied || (fixture.denied && strings.Contains(result.Output, "ROOT_ONLY_MARKER")) {
			t.Fatalf("nested scope not isolated for %s: %+v %v", fixture.id, result, err)
		}
	}
	result, err := original.Execute(context.Background(), map[string]any{"agent_id": "agt-root-child"})
	if err != nil || result.IsError || !strings.Contains(result.Output, "ROOT_ONLY_MARKER") {
		t.Fatalf("copy rebind mutated parent registry generation: %+v %v", result, err)
	}
	listed, _ := registry.Get("SubAgentList")
	result, err = listed.Execute(context.Background(), nil)
	if err != nil || result.IsError || strings.Contains(result.Output, "agt-root-child") || !strings.Contains(result.Output, "agt-nested-child") {
		t.Fatalf("list scope was not rebound with output: %+v %v", result, err)
	}
}

func TestSubAgentHistoryRebindProviderWithoutAgentTool(t *testing.T) {
	dir := t.TempDir()
	gate := permission.New(permission.ModeBypass)
	historicalSubAgentFixture(t, dir, "agt-new-parent", "new-parent", "new-parent", "completed", "new-parent-result", "end_turn")
	reg := tools.NewRegistry()
	AttachSubAgentTools(reg, gate, nil, SubAgentQueryScope{SessionDir: dir, ParentSessionID: "old-parent"})
	RebindProviderTools(reg, rebindProvider{name: "test", model: "test", cap: 100}, "test", "test", "new-parent")
	tool, _ := reg.Get("SubAgentOutput")
	result, err := tool.Execute(context.Background(), map[string]any{"agent_id": "agt-new-parent"})
	if err != nil || result.IsError || !strings.Contains(result.Output, "new-parent-result") {
		t.Fatalf("history-only registry retained old session: %+v %v", result, err)
	}
}

func TestSubAgentHistoryDiagnosticsRedactedAndInputLimits(t *testing.T) {
	dir := t.TempDir()
	gate := permission.New(permission.ModeBypass)
	transcript, err := agent.NewSubAgentTranscript(dir, "agt-redacted", pubsess.Header{ID: "agt-redacted", SubAgentOf: "parent", TeammateName: "redacted", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err = transcript.AppendTerminal(agent.SubAgentTerminal{Status: "failed", StopHint: "max_tokens", Output: "answer", ExitError: "upstream Authorization: Bearer fixture-secret-123456"}); err != nil {
		t.Fatal(err)
	}
	transcript.Close()
	tool := NewSubAgentOutput(gate, nil).WithSessionPersistence(dir, "parent")
	result, err := tool.Execute(context.Background(), map[string]any{"agent_id": "agt-redacted"})
	if err != nil || result.IsError || strings.Contains(result.Output, "fixture-secret-123456") || strings.Contains(result.Output, "upstream Authorization") || !strings.Contains(result.Output, "---diagnostic---") || !strings.Contains(result.Output, "per-response output limit") {
		t.Fatalf("unsafe or missing diagnostic: %+v %v", result, err)
	}
	list := NewSubAgentList(gate, nil).WithSessionPersistence(dir, "parent")
	for _, input := range []map[string]any{{"limit": 0}, {"limit": 101}, {"limit": 1.5}, {"offset": -1}, {"offset": 5000}} {
		result, err = list.Execute(context.Background(), input)
		if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "read_error") {
			t.Fatalf("invalid page admitted: %+v %v", result, err)
		}
	}
}

func TestSubAgentHistoryCatalogWarnsOnUnreadableRecord(t *testing.T) {
	dir := t.TempDir()
	historicalSubAgentFixture(t, dir, "agt-good", "parent", "name", "completed", "good", "end_turn")
	if err := os.WriteFile(filepath.Join(dir, agent.SubAgentTranscriptDirname, "agt-broken.jsonl"), []byte("{broken}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := NewSubAgentList(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), nil)
	if err != nil || list.IsError || !strings.Contains(list.Output, "history_incomplete=true") || !strings.Contains(list.Output, "read_errors=1") || !strings.Contains(list.Output, "agt-good") {
		t.Fatalf("list silently hid read failure: %+v %v", list, err)
	}
	output, err := NewSubAgentOutput(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"name": "name"})
	if err != nil || !output.IsError || !strings.Contains(output.Output, "read_error") {
		t.Fatalf("incomplete name scan invented uniqueness: %+v %v", output, err)
	}
}

func TestSubAgentHistoryAggregateReadBudgetIsExplicit(t *testing.T) {
	dir := t.TempDir()
	historicalSubAgentFixture(t, dir, "agt-budget", "parent", "budget", "completed", "body", "end_turn")
	scope := subAgentQueryScope{sessionDir: dir, parentID: "parent"}
	root, err := scope.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	budget := int64(1)
	_, err = scope.readRecord(root, "agt-budget", nil, &budget)
	if err != errSubAgentQueryReadBudget {
		t.Fatalf("expected explicit bounded read budget failure, got %v", err)
	}
}

func TestSubAgentHistoryOwnedQueuedWithoutTranscript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created-yet")
	roster := agent.NewRoster(0)
	for _, fixture := range []struct{ id, parent string }{{"agt-own-queued", "parent"}, {"agt-other-queued", "foreign"}, {"agt-unbound-queued", ""}} {
		child := &agent.Teammate{AgentID: fixture.id, Name: fixture.id, ParentSessionID: fixture.parent, Status: agent.StatusQueued}
		if err := roster.Register(child); err != nil {
			t.Fatal(err)
		}
	}
	list, err := NewSubAgentList(permission.New(permission.ModeBypass), roster).WithSessionPersistence(dir, "parent").Execute(context.Background(), nil)
	if err != nil || list.IsError || !strings.Contains(list.Output, "agt-own-queued") || !strings.Contains(list.Output, "status=queued") || strings.Contains(list.Output, "agt-other-queued") || strings.Contains(list.Output, "agt-unbound-queued") {
		t.Fatalf("queued owner scope leaked or lost: %+v %v", list, err)
	}
	reader := NewSubAgentOutput(permission.New(permission.ModeBypass), roster).WithSessionPersistence(dir, "parent")
	for _, fixture := range []struct {
		id     string
		denied bool
	}{{"agt-own-queued", false}, {"agt-other-queued", true}, {"agt-unbound-queued", true}} {
		result, err := reader.Execute(context.Background(), map[string]any{"agent_id": fixture.id})
		if err != nil || result.IsError != fixture.denied || (!fixture.denied && (!strings.Contains(result.Output, "load_state=live") || !strings.Contains(result.Output, "status=queued"))) {
			t.Fatalf("queued ID ownership incorrect for %s: %+v %v", fixture.id, result, err)
		}
	}
}

func TestSubAgentHistoryNoLinkStorageDirectories(t *testing.T) {
	for _, location := range []string{"session-root", "subagents"} {
		t.Run(location, func(t *testing.T) {
			actual := t.TempDir()
			historicalSubAgentFixture(t, actual, "agt-target", "parent", "target", "completed", "DIRECTORY_LINK_SECRET", "end_turn")
			dir := filepath.Join(t.TempDir(), "linked")
			if location == "session-root" {
				if err := os.Symlink(actual, dir); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(actual, agent.SubAgentTranscriptDirname), filepath.Join(dir, agent.SubAgentTranscriptDirname)); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			result, err := NewSubAgentOutput(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"agent_id": "agt-target"})
			if err != nil || !result.IsError || !strings.Contains(result.Output, "read_error") || strings.Contains(result.Output, "DIRECTORY_LINK_SECRET") {
				t.Fatalf("directory link bypassed storage boundary: %+v %v", result, err)
			}
		})
	}
}

func TestSubAgentHistoryBoundedHeaderAndUnacknowledgedTail(t *testing.T) {
	dir := t.TempDir()
	path := historicalSubAgentFixture(t, dir, "agt-tail", "parent", "tail", "completed", "saved complete", "end_turn")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"checkpoint","checkpoint":`)
	f.Close()
	reader := NewSubAgentOutput(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, "parent")
	result, err := reader.Execute(context.Background(), map[string]any{"agent_id": "agt-tail"})
	if err != nil || result.IsError || !strings.Contains(result.Output, "incomplete_tail=true") || !strings.Contains(result.Output, "saved complete") {
		t.Fatalf("partial trailing bytes destroyed acknowledged status: %+v %v", result, err)
	}
	transcript, err := agent.NewSubAgentTranscript(dir, "agt-large-header", pubsess.Header{ID: "agt-large-header", SubAgentOf: "parent", System: strings.Repeat("x", 70<<10)})
	if err != nil {
		t.Fatal(err)
	}
	transcript.Close()
	result, err = reader.Execute(context.Background(), map[string]any{"agent_id": "agt-large-header"})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "read_error") {
		t.Fatalf("unbounded first header admitted: %+v %v", result, err)
	}
}

func TestSubAgentHistoryScanLimitWarnsAndDoesNotAssertUniqueName(t *testing.T) {
	dir := t.TempDir()
	historicalSubAgentFixture(t, dir, "agt-deep-name", "parent", "maybe-duplicate", "completed", "result", "end_turn")
	for i := 0; i < subAgentHistoryMaxScan+1; i++ {
		if err := os.WriteFile(filepath.Join(dir, agent.SubAgentTranscriptDirname, fmt.Sprintf("ignored-%04d.txt", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gate := permission.New(permission.ModeBypass)
	result, err := NewSubAgentList(gate, nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), nil)
	if err != nil || result.IsError || !strings.Contains(result.Output, "history_incomplete=true") || !strings.Contains(result.Output, "scan_limit_reached=true") {
		t.Fatalf("scan ceiling was silently treated as complete: %+v %v", result, err)
	}
	result, err = NewSubAgentOutput(gate, nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"name": "maybe-duplicate"})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "read_error") {
		t.Fatalf("incomplete scan invented name uniqueness: %+v %v", result, err)
	}
	result, err = NewSubAgentOutput(gate, nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"agent_id": "agt-deep-name"})
	if err != nil || result.IsError || !strings.Contains(result.Output, "status=completed") {
		t.Fatalf("direct stable-ID query must bypass catalog ceiling safely: %+v %v", result, err)
	}
}

func TestSubAgentHistoryRetainsContinuationAttemptsAndExecutionPhase(t *testing.T) {
	dir := t.TempDir()
	transcript, err := agent.NewSubAgentTranscript(dir, "agt-recovered", pubsess.Header{ID: "agt-recovered", SubAgentOf: "parent", TeammateName: "恢复验证", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err = transcript.AppendTerminal(agent.SubAgentTerminal{Status: "failed", Result: "partial final answer", StopHint: "max_tokens", ExitError: "sub-agent incomplete: max_tokens", ContinuationAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	transcript.Close()
	result, err := NewSubAgentOutput(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"agent_id": "agt-recovered"})
	if err != nil || result.IsError || !strings.Contains(result.Output, "continuation_attempts=3") {
		t.Fatalf("historical continuation receipt lost: %+v %v", result, err)
	}
	presentation := result.Presentation["subagent"].(map[string]any)
	if presentation["continuationAttempts"] != 3 || presentation["partialOutput"] != true || presentation["canResume"] != true {
		t.Fatalf("structured continuation metadata wrong: %+v", presentation)
	}
	roster := agent.NewRoster(0)
	queued := &agent.Teammate{AgentID: "agt-queued-phase", Name: "queued", ParentSessionID: "parent", Status: agent.StatusQueued}
	if err := roster.Register(queued); err != nil {
		t.Fatal(err)
	}
	result, err = NewSubAgentOutput(permission.New(permission.ModeBypass), roster).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"agent_id": "agt-queued-phase"})
	if err != nil || result.IsError || !strings.Contains(result.Output, "execution_phase=queued") {
		t.Fatalf("machine execution_phase field missing from live first line: %+v %v", result, err)
	}
}

func TestSubAgentHistoryDiagnosticNeverCopiesProviderBody(t *testing.T) {
	const raw = `HTTP 429 {"error":{"message":"UPSTREAM_RAW_BODY_MARKER account_private_data=keep-out Authorization: Bearer fixture-secret-123456"}}`
	for _, mode := range []string{"cold", "live", "unknown_hint"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			stop := "api_error"
			if mode == "unknown_hint" {
				stop = raw
			}
			transcript, err := agent.NewSubAgentTranscript(dir, "agt-http-failure", pubsess.Header{ID: "agt-http-failure", SubAgentOf: "parent", TeammateName: "api-test", CreatedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if err = transcript.AppendTerminal(agent.SubAgentTerminal{Status: "failed", Output: "safe partial answer", StopHint: stop, ExitError: raw}); err != nil {
				t.Fatal(err)
			}
			transcript.Close()
			var roster *agent.Roster
			if mode == "live" {
				roster = agent.NewRoster(0)
				child := &agent.Teammate{AgentID: "agt-http-failure", Name: "api-test", ParentSessionID: "parent"}
				if err = roster.Register(child); err != nil {
					t.Fatal(err)
				}
				child.Finish(agent.StatusFailed, "safe partial answer", &transport.HTTPStatusError{StatusCode: 429, Err: errors.New(raw)}, stop)
			}
			result, err := NewSubAgentOutput(permission.New(permission.ModeBypass), roster).WithSessionPersistence(dir, "parent").Execute(context.Background(), map[string]any{"agent_id": "agt-http-failure"})
			if err != nil || result.IsError || !strings.Contains(result.Output, "safe partial answer") {
				t.Fatalf("failed status not readable: %+v %v", result, err)
			}
			expectedReason, expectedDiagnostic := "api_error", "provider request failed"
			if mode == "unknown_hint" {
				expectedReason, expectedDiagnostic = "execution_error", "failed during execution"
			}
			if !strings.Contains(result.Output, "stop_reason="+expectedReason) || !strings.Contains(result.Output, expectedDiagnostic) {
				t.Errorf("host failure classification was lost: %s", result.Output)
			}
			encoded, _ := json.Marshal(result.Presentation)
			for _, text := range []string{result.Output, string(encoded)} {
				for _, forbidden := range []string{"UPSTREAM_RAW_BODY_MARKER", "account_private_data", "fixture-secret-123456", "HTTP 429"} {
					if strings.Contains(text, forbidden) {
						t.Errorf("raw provider payload leaked in %s: %s", mode, text)
					}
				}
			}
			list, err := NewSubAgentList(permission.New(permission.ModeBypass), roster).WithSessionPersistence(dir, "parent").Execute(context.Background(), nil)
			if err != nil || list.IsError || strings.Contains(list.Output, "UPSTREAM_RAW_BODY_MARKER") || strings.Contains(list.Output, "account_private_data") {
				t.Fatalf("list diagnostic copied raw provider body: %+v %v", list, err)
			}
		})
	}
}

func TestSubAgentHistoryHeaderNamePatchesRemainQueryable(t *testing.T) {
	dir := t.TempDir()
	path := historicalSubAgentFixture(t, dir, "agt-renamed", "parent", "original-name", "completed", "rename answer", "end_turn")
	patch := func(path, name, parent string) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"type": "header", "header": pubsess.Header{ID: filepath.Base(strings.TrimSuffix(path, ".jsonl")), TeammateName: name, SubAgentOf: parent}})
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err = f.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	patch(path, "current-name", "")
	tool := NewSubAgentOutput(permission.New(permission.ModeBypass), nil).WithSessionPersistence(dir, "parent")
	for _, input := range []map[string]any{{"name": "current-name"}, {"agent_id": "agt-renamed"}} {
		result, err := tool.Execute(context.Background(), input)
		if err != nil || result.IsError || !strings.Contains(result.Output, `name="current-name"`) || !strings.Contains(result.Output, "rename answer") {
			t.Fatalf("mutable persisted name was ignored: %+v %v", result, err)
		}
	}
	result, err := tool.Execute(context.Background(), map[string]any{"name": "original-name"})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "not_found") {
		t.Fatalf("obsolete name still resolves: %+v %v", result, err)
	}
	second := historicalSubAgentFixture(t, dir, "agt-also-renamed", "parent", "another-name", "completed", "second answer", "end_turn")
	patch(second, "current-name", "")
	result, err = tool.Execute(context.Background(), map[string]any{"name": "current-name"})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "ambiguous") {
		t.Fatalf("renames bypassed name ambiguity guard: %+v %v", result, err)
	}
	patch(path, "bad-owner", "foreign-parent")
	result, err = tool.Execute(context.Background(), map[string]any{"agent_id": "agt-renamed"})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "read_error") || strings.Contains(result.Output, "rename answer") {
		t.Fatalf("mutable header changed immutable ownership: %+v %v", result, err)
	}
}
