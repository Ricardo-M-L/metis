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
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type rewindFixtureProvider struct {
	activationTestProvider
	paths []string
	calls int
}

func (p *rewindFixtureProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls++
	if p.calls > 1 {
		return &composerSummaryStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "done"}, {Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"}}}, nil
	}
	var events []llm.StreamEvent
	for index, path := range p.paths {
		id := string(rune('a' + index))
		data, _ := json.Marshal(map[string]any{"path": path, "content": "changed"})
		events = append(events, llm.StreamEvent{Type: "tool_use_start", ToolUseID: id, ToolName: "Write"}, llm.StreamEvent{Type: "tool_input_delta", ToolUseID: id, InputDelta: string(data)}, llm.StreamEvent{Type: "tool_use_stop", ToolUseID: id, InputDelta: string(data)})
	}
	events = append(events, llm.StreamEvent{Type: "message_delta", StopReason: "tool_use"}, llm.StreamEvent{Type: "message_stop"})
	return &composerSummaryStream{events: events}, nil
}

type rewindFixtureWrite struct{ tools.BaseTool }

func (rewindFixtureWrite) Name() string                { return "Write" }
func (rewindFixtureWrite) Description() string         { return "write a disposable fixture file" }
func (rewindFixtureWrite) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (rewindFixtureWrite) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (rewindFixtureWrite) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (rewindFixtureWrite) Execute(_ context.Context, input map[string]any) (*tools.Result, error) {
	path, _ := input["path"].(string)
	content, _ := input["content"].(string)
	if path == "" {
		return nil, errors.New("missing fixture path")
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return nil, err
	}
	return &tools.Result{Output: "written"}, nil
}

type rewindFixture struct {
	server                           *Server
	store                            *session.Store
	id, workspace, existing, created string
	history                          []llm.Message
}

func newRewindFixture(t *testing.T) rewindFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	workspace := filepath.Join(home, "project")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	existing, created := filepath.Join(workspace, "before.txt"), filepath.Join(workspace, "created.txt")
	if err := os.WriteFile(existing, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := "rewind-api-fixture"
	provider := &rewindFixtureProvider{activationTestProvider: activationTestProvider{name: "wire", model: "fixture"}, paths: []string{existing, created}}
	registry := tools.NewRegistry()
	registry.Register(rewindFixtureWrite{})
	loop := agent.NewLoop(provider, registry, permission.New(permission.ModeDefault), nil, "system", 5)
	loop.Model = "fixture"
	loop.SetCheckpointer(checkpoint.NewManager(id, workspace, ""))
	text := func(role llm.Role, text string) llm.Message {
		return llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
	}
	loop.Restore([]llm.Message{text(llm.RoleUser, "first conversation"), text(llm.RoleAssistant, "first answer")})
	loop.AppendUser("edit the two fixture files")
	if err := loop.Run(t.Context(), make(chan agent.Event, 512)); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(existing); string(data) != "changed" {
		t.Fatalf("fixture Write did not execute: %q", data)
	}
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: workspace, Provider: "wire", Model: "fixture", Mode: "default", System: "system"}); err != nil {
		t.Fatal(err)
	}
	for _, message := range loop.History() {
		if err := store.AppendMessage(id, message); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer("127.0.0.1:0", nil, store)
	server.loop = agent.NewLoop(nil, nil, nil, nil, "other system", 1)
	server.loop.AppendUser("other active conversation")
	server.activeSessionID = "other-active"
	return rewindFixture{server: server, store: store, id: id, workspace: workspace, existing: existing, created: created, history: loop.History()}
}

func rewindGet(t *testing.T, f rewindFixture) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	f.server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/rewind?sessionId="+f.id, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET rewind = %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}
func rewindPost(t *testing.T, f rewindFixture, mode, revision string, turn int) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"sessionId": f.id, "mode": mode, "turn": turn, "revision": revision})
	rr := httptest.NewRecorder()
	f.server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/rewind", bytes.NewReader(data)))
	return rr
}

func TestDesktopRewindDurableThreeModesWithoutActivation(t *testing.T) {
	for _, mode := range []string{"conversation", "code", "both"} {
		t.Run(mode, func(t *testing.T) {
			f := newRewindFixture(t)
			active := f.server.loop.History()
			cancelled := false
			f.server.backgroundSession = "other-active"
			f.server.backgroundCancel = func() { cancelled = true }
			view := rewindGet(t, f)
			points := view["points"].([]any)
			if len(points) != 2 || points[0].(map[string]any)["codeAvailable"] != true {
				t.Fatalf("durable points = %#v", points)
			}
			if f.server.activeSessionID != "other-active" || !reflect.DeepEqual(active, f.server.loop.History()) {
				t.Fatal("GET activated an unrelated session")
			}
			rr := rewindPost(t, f, mode, view["revision"].(string), 2)
			if rr.Code != http.StatusOK {
				t.Fatalf("POST %s = %d: %s", mode, rr.Code, rr.Body.String())
			}
			var body struct {
				SessionID, Prefill string
				Changed            bool
				Messages           []llm.Message
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			_, durable, err := f.store.Load(f.id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "code" {
				if body.Prefill != "" || !reflect.DeepEqual(f.history, durable) {
					t.Fatal("code-only changed conversation or prefill")
				}
			} else if len(durable) != 2 || body.Prefill != "edit the two fixture files" {
				t.Fatalf("conversation boundary = %#v %q", durable, body.Prefill)
			}
			data, _ := os.ReadFile(f.existing)
			_, newErr := os.Stat(f.created)
			if mode == "conversation" {
				if string(data) != "changed" || newErr != nil {
					t.Fatal("conversation-only changed files")
				}
			} else if string(data) != "original" || !os.IsNotExist(newErr) {
				t.Fatalf("files not restored: %q new=%v", data, newErr)
			}
			if cancelled || f.server.activeSessionID != "other-active" || !reflect.DeepEqual(active, f.server.loop.History()) {
				t.Fatal("rewind changed another active runtime/background watcher")
			}
			if !body.Changed || !reflect.DeepEqual(body.Messages, durable) {
				t.Fatal("response not bound to durable resulting history")
			}
		})
	}
}

func TestDesktopRewindRejectsStaleBusyMissingAndExternalChanges(t *testing.T) {
	for _, scenario := range []string{"stale", "session busy", "workspace busy", "ancestor busy", "external file change", "unwritable history"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRewindFixture(t)
			view := rewindGet(t, f)
			switch scenario {
			case "stale":
				if err := f.store.AppendMessage(f.id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new request"}}}); err != nil {
					t.Fatal(err)
				}
			case "session busy":
				lock, ok, err := f.store.TryAcquireTurnLock(f.id)
				if err != nil || !ok {
					t.Fatal(err)
				}
				defer lock.Release()
			case "workspace busy", "ancestor busy":
				path := f.workspace
				if scenario == "ancestor busy" {
					path = filepath.Dir(path)
				}
				lease, ok := f.server.turnCoordinator.TryAcquire(path)
				if !ok {
					t.Fatal("failed to reserve workspace")
				}
				defer lease.Release()
			case "external file change":
				if err := os.WriteFile(f.existing, []byte("user external change"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unwritable history":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses ordinary file write permissions")
				}
				path := filepath.Join(f.store.Dir, f.id+".jsonl")
				if err := os.Chmod(path, 0o400); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(path, 0o600)
			}
			_, before, _ := f.store.Load(f.id)
			fileBefore, _ := os.ReadFile(f.existing)
			rr := rewindPost(t, f, "both", view["revision"].(string), 2)
			if rr.Code != http.StatusConflict {
				t.Fatalf("%s accepted: %d %s", scenario, rr.Code, rr.Body.String())
			}
			_, after, err := f.store.Load(f.id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed restore changed durable history: %v", err)
			}
			fileAfter, _ := os.ReadFile(f.existing)
			if !bytes.Equal(fileBefore, fileAfter) {
				t.Fatalf("failed restore changed file: before=%q after=%q", fileBefore, fileAfter)
			}
		})
	}
}

func TestDesktopRewindNoSnapshotConversationStillWorks(t *testing.T) {
	f := newRewindFixture(t)
	id := "conversation-only"
	f.id = id
	if err := f.store.WriteHeaderFull(session.Header{ID: id, WorkDir: f.workspace}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "only chatting"}}}); err != nil {
		t.Fatal(err)
	}
	view := rewindGet(t, f)
	metadata := checkpoint.NewManager(f.id, f.workspace, "").RewindMetadataPath()
	if _, err := os.Stat(filepath.Dir(metadata)); !os.IsNotExist(err) {
		t.Fatalf("GET created a shadow repository for a chat-only session: %v", err)
	}
	point := view["points"].([]any)[0].(map[string]any)
	if point["codeAvailable"] != false || point["codeUnavailableReason"] == "" {
		t.Fatalf("missing snapshot reason = %#v", point)
	}
	if rr := rewindPost(t, f, "code", view["revision"].(string), 1); rr.Code != http.StatusConflict {
		t.Fatalf("no snapshot accepted: %d %s", rr.Code, rr.Body.String())
	}
	if rr := rewindPost(t, f, "conversation", view["revision"].(string), 1); rr.Code != http.StatusOK {
		t.Fatalf("conversation unavailable: %d %s", rr.Code, rr.Body.String())
	}
}

func TestDesktopRewindBlocksParentOwnedAndInvalidMode(t *testing.T) {
	f := newRewindFixture(t)
	view := rewindGet(t, f)
	if rr := rewindPost(t, f, "shell", view["revision"].(string), 2); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode=%d", rr.Code)
	}
	if err := f.store.WriteHeaderFull(session.Header{ID: f.id, WorkDir: f.workspace, SubAgentOf: "parent-session"}); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	f.server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/rewind?sessionId="+f.id, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("child GET=%d %s", rr.Code, rr.Body.String())
	}
	if rr := rewindPost(t, f, "both", view["revision"].(string), 2); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "parent") {
		t.Fatalf("child POST=%d %s", rr.Code, rr.Body.String())
	}
}

func TestDesktopRewindBlocksDurableChildAndUnreadableChildRecord(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid child", true: "damaged child"}[broken], func(t *testing.T) {
			f := newRewindFixture(t)
			activeBefore := f.server.loop.History()
			view := rewindGet(t, f)
			child, err := agent.NewSubAgentTranscript(f.store.Dir, f.id, session.Header{ID: f.id, SubAgentOf: "durable-parent", WorkDir: f.workspace})
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Close(); err != nil {
				t.Fatal(err)
			}
			if broken {
				path := filepath.Join(f.store.Dir, agent.SubAgentTranscriptDirname, f.id+".jsonl")
				if err := os.WriteFile(path, []byte("broken child record\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rr := httptest.NewRecorder()
			f.server.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/rewind?sessionId="+f.id, nil))
			if rr.Code != http.StatusForbidden {
				t.Fatalf("child GET=%d %s", rr.Code, rr.Body.String())
			}
			if rr := rewindPost(t, f, "both", view["revision"].(string), 2); rr.Code != http.StatusForbidden {
				t.Fatalf("child POST=%d %s", rr.Code, rr.Body.String())
			}
			if current := f.server.loop.History(); !reflect.DeepEqual(current, activeBefore) {
				t.Fatal("blocked child rewind changed active history")
			}
			_, durable, err := f.store.Load(f.id)
			if err != nil || !reflect.DeepEqual(durable, f.history) {
				t.Fatal("blocked child rewind changed the target's durable history")
			}
		})
	}
}

func TestDesktopRewindCodeOnlyInvalidatesStoppedToken(t *testing.T) {
	f := newRewindFixture(t)
	if err := f.store.FinishDesktopTurn(f.id, "stopped", true); err != nil {
		t.Fatal(err)
	}
	header, _, _ := f.store.Load(f.id)
	if header.ResumeToken == "" {
		t.Fatal("fixture has no stopped token")
	}
	view := rewindGet(t, f)
	rr := rewindPost(t, f, "code", view["revision"].(string), 2)
	if rr.Code != http.StatusOK {
		t.Fatalf("code restore=%d %s", rr.Code, rr.Body.String())
	}
	header, _, _ = f.store.Load(f.id)
	if header.ResumeToken != "" {
		t.Fatal("code rewind retained stale resume capability")
	}
}

func TestDesktopRewindCorruptOrMissingMappingNeverGuessesShadowHead(t *testing.T) {
	for _, scenario := range []string{"corrupt", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRewindFixture(t)
			path := checkpoint.NewManager(f.id, f.workspace, "").RewindMetadataPath()
			if scenario == "corrupt" {
				if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			view := rewindGet(t, f)
			point := view["points"].([]any)[0].(map[string]any)
			if point["codeAvailable"] != false || point["codeUnavailableReason"] == "" {
				t.Fatalf("invalid mapping claimed usable: %#v", point)
			}
			if rr := rewindPost(t, f, "both", view["revision"].(string), 2); rr.Code != http.StatusConflict {
				t.Fatalf("invalid mapping restored code=%d %s", rr.Code, rr.Body.String())
			}
			if rr := rewindPost(t, f, "conversation", view["revision"].(string), 2); rr.Code != http.StatusOK {
				t.Fatalf("conversation failed after invalid mapping=%d %s", rr.Code, rr.Body.String())
			}
			data, _ := os.ReadFile(f.existing)
			if string(data) != "changed" {
				t.Fatal("conversation fallback changed files")
			}
		})
	}
}

func TestDesktopRewindSavedOffRetainsPreviouslyValidCodeRecovery(t *testing.T) {
	f := newRewindFixture(t)
	if _, err := checkpoint.NewManager(f.id, f.workspace, "").Command(t.Context(), "off"); err != nil {
		t.Fatal(err)
	}
	view := rewindGet(t, f)
	if view["checkpoint"].(map[string]any)["enabled"] != false {
		t.Fatal("explicit off lost")
	}
	if point := view["points"].([]any)[0].(map[string]any); point["codeAvailable"] != true {
		t.Fatalf("saved snapshots disabled by off: %#v", point)
	}
	if rr := rewindPost(t, f, "code", view["revision"].(string), 2); rr.Code != http.StatusOK {
		t.Fatalf("saved snapshot restore=%d %s", rr.Code, rr.Body.String())
	}
	if checkpoint.NewManager(f.id, f.workspace, "").ControlStatus().Enabled {
		t.Fatal("code recovery switched automatic checkpoints back on")
	}
}

func TestDesktopRewindFailurePreservesUnrepairedOriginalHistory(t *testing.T) {
	f := newRewindFixture(t)
	orphan := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "unfinished-read", ToolName: "Read", ToolInput: map[string]any{"path": f.existing}}}}
	if err := f.store.AppendMessage(f.id, orphan); err != nil {
		t.Fatal(err)
	}
	_, original, _ := f.store.Load(f.id)
	view := rewindGet(t, f)
	if err := os.WriteFile(f.existing, []byte("external user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := rewindPost(t, f, "both", view["revision"].(string), 2)
	if rr.Code != http.StatusConflict {
		t.Fatalf("external edit allowed=%d %s", rr.Code, rr.Body.String())
	}
	_, after, err := f.store.Load(f.id)
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatalf("failed restore committed unrelated orphan repair: %v", err)
	}
}

func TestDesktopRewindRejectsObservableRunningChild(t *testing.T) {
	f := newRewindFixture(t)
	view := rewindGet(t, f)
	f.server.recordWorkerSubAgentLifecycle(f.id, agent.Event{Kind: agent.EventSubAgentStart, SubAgentID: "agt-running", SubAgentStatus: "running", SubAgentBackground: true})
	busy := rewindGet(t, f)
	point := busy["points"].([]any)[0].(map[string]any)
	if point["codeAvailable"] != false || !strings.Contains(point["codeUnavailableReason"].(string), "子代理") {
		t.Fatalf("running child not disclosed: %#v", point)
	}
	rr := rewindPost(t, f, "both", view["revision"].(string), 2)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "子代理") {
		t.Fatalf("code restore raced child=%d %s", rr.Code, rr.Body.String())
	}
	_, after, _ := f.store.Load(f.id)
	if !reflect.DeepEqual(after, f.history) {
		t.Fatal("running child rejection changed history")
	}
	data, _ := os.ReadFile(f.existing)
	if string(data) != "changed" {
		t.Fatal("running child rejection changed files")
	}
}

func TestDesktopRewindRunningChildRespectsWorkspaceOverlap(t *testing.T) {
	for _, scope := range []string{"same", "ancestor", "unrelated"} {
		t.Run(scope, func(t *testing.T) {
			f := newRewindFixture(t)
			workspace := f.workspace
			if scope == "ancestor" {
				workspace = filepath.Dir(workspace)
			} else if scope == "unrelated" {
				workspace = t.TempDir()
			}
			if err := f.store.WriteHeaderFull(session.Header{ID: "other-parent", WorkDir: workspace}); err != nil {
				t.Fatal(err)
			}
			f.server.recordWorkerSubAgentLifecycle("other-parent", agent.Event{Kind: agent.EventSubAgentStart, SubAgentID: "agt-other", SubAgentStatus: "running", SubAgentBackground: true})
			view := rewindGet(t, f)
			point := view["points"].([]any)[0].(map[string]any)
			wantAvailable := scope == "unrelated"
			if point["codeAvailable"] != wantAvailable {
				t.Fatalf("%s child codeAvailable=%v", scope, point["codeAvailable"])
			}
			rr := rewindPost(t, f, "code", view["revision"].(string), 2)
			wantStatus := http.StatusConflict
			if wantAvailable {
				wantStatus = http.StatusOK
			}
			if rr.Code != wantStatus {
				t.Fatalf("%s child POST=%d %s", scope, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestDesktopRewindUpdatesOnlyCurrentOwnersHistory(t *testing.T) {
	f := newRewindFixture(t)
	f.server.activeSessionID = f.id
	f.server.loop.Restore(f.history)
	view := rewindGet(t, f)
	rr := rewindPost(t, f, "both", view["revision"].(string), 2)
	if rr.Code != http.StatusOK {
		t.Fatalf("active rewind=%d %s", rr.Code, rr.Body.String())
	}
	_, actual, _ := f.store.Load(f.id)
	if !reflect.DeepEqual(actual, f.server.loop.History()) {
		t.Fatal("active runtime did not adopt durable rewind")
	}
	if f.server.loop.System != "other system" {
		t.Fatal("rewind unexpectedly rebound provider/system configuration")
	}
}
