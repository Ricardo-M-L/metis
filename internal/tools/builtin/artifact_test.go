package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	artifactstore "github.com/Ricardo-M-L/metis/internal/artifact"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestArtifactCapabilitiesAndWritePermissionGate(t *testing.T) {
	plan := NewArtifact(permission.New(permission.ModePlan))
	for _, action := range []string{"list", "read"} {
		in := map[string]any{"action": action}
		if !plan.IsReadOnly(in) || plan.Concurrency(in) != tools.ConcurrencySafe {
			t.Fatalf("%s must be concurrency-safe and read-only", action)
		}
		if decision, _ := plan.CanUse(context.Background(), in); decision != tools.PermissionAllow {
			t.Fatalf("%s permission = %v, want allow", action, decision)
		}
	}
	for _, action := range []string{"create", "update"} {
		in := map[string]any{"action": action}
		if plan.IsReadOnly(in) || plan.Concurrency(in) != tools.ConcurrencyExclusive {
			t.Fatalf("%s must be exclusive and state-changing", action)
		}
		if decision, _ := plan.CanUse(context.Background(), in); decision != tools.PermissionDeny {
			t.Fatalf("plan %s permission = %v, want deny", action, decision)
		}
	}

	defaultMode := NewArtifact(permission.New(permission.ModeDefault))
	if decision, _ := defaultMode.CanUse(context.Background(), map[string]any{"action": "create"}); decision != tools.PermissionAsk {
		t.Fatalf("default create permission = %v, want ask", decision)
	}
	bypass := NewArtifact(permission.New(permission.ModeBypassPermissions))
	if decision, _ := bypass.CanUse(context.Background(), map[string]any{"action": "update"}); decision != tools.PermissionAllow {
		t.Fatalf("bypass update permission = %v, want allow", decision)
	}
}

func TestArtifactExecuteUsesCurrentSessionAndReturnsPresentation(t *testing.T) {
	store, err := artifactstore.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	tool := NewArtifact(permission.New(permission.ModeBypassPermissions), store)
	setArtifactTestSession(t, "session-a")

	created, err := tool.Execute(context.Background(), map[string]any{
		"action": "create", "title": "Dashboard", "html": `<h1 onclick="evil()">Hello</h1><script>evil()</script>`,
	})
	if err != nil || created == nil || created.IsError {
		t.Fatalf("create: result=%+v err=%v", created, err)
	}
	if created.Display != "Local artifact" || created.Presentation["kind"] != "artifact" {
		t.Fatalf("create presentation = %+v, display=%q", created.Presentation, created.Display)
	}
	id, _ := created.Presentation["artifact_id"].(string)
	if id == "" || created.Presentation["version"] != 1 {
		t.Fatalf("create presentation missing identity: %+v", created.Presentation)
	}
	manifest, ok := created.Presentation["artifact"].(artifactstore.Manifest)
	if !ok || manifest.SessionID != "session-a" || manifest.ID != id {
		t.Fatalf("nested manifest = %#v", created.Presentation["artifact"])
	}

	updated, err := tool.Execute(context.Background(), map[string]any{
		"action": "update", "id": id, "html": "<p>version two</p>",
	})
	if err != nil || updated.IsError || updated.Presentation["version"] != 2 {
		t.Fatalf("update: result=%+v err=%v", updated, err)
	}

	read, err := tool.Execute(context.Background(), map[string]any{"action": "read", "id": id, "version": 1})
	if err != nil || read.IsError {
		t.Fatalf("read: result=%+v err=%v", read, err)
	}
	var payload struct {
		HTML    string                `json:"html"`
		Version artifactstore.Version `json:"version"`
	}
	if err := json.Unmarshal([]byte(read.Output), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Version.Number != 1 || !strings.Contains(payload.HTML, "Hello") || strings.Contains(payload.HTML, "onclick") || strings.Contains(payload.HTML, "script") {
		t.Fatalf("read payload = %+v", payload)
	}

	listed, err := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if err != nil || listed.IsError || !strings.Contains(listed.Output, id) || listed.Presentation["kind"] != "artifact" {
		t.Fatalf("list: result=%+v err=%v", listed, err)
	}

	tasks.SetCurrentSessionID("session-b")
	foreignRead, err := tool.Execute(context.Background(), map[string]any{"action": "read", "id": id})
	if err != nil || foreignRead == nil || !foreignRead.IsError || !strings.Contains(foreignRead.Output, artifactstore.ErrOwnerMismatch.Error()) {
		t.Fatalf("cross-session read: result=%+v err=%v", foreignRead, err)
	}
	foreignList, err := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if err != nil || foreignList.IsError || strings.Contains(foreignList.Output, id) {
		t.Fatalf("cross-session list: result=%+v err=%v", foreignList, err)
	}
}

func TestArtifactValidationAndRegistration(t *testing.T) {
	store, err := artifactstore.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	tool := NewArtifact(nil, store)
	setArtifactTestSession(t, "session-a")
	for _, in := range []map[string]any{
		nil,
		{"action": "create", "title": "missing html"},
		{"action": "update", "html": "<p>x</p>"},
		{"action": "read", "id": "missing", "version": 1.5},
	} {
		result, err := tool.Execute(context.Background(), in)
		if err != nil || result == nil || !result.IsError {
			t.Fatalf("invalid input %#v: result=%+v err=%v", in, result, err)
		}
	}

	tasks.SetCurrentSessionID("")
	result, err := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, artifactstore.ErrInvalidSession.Error()) {
		t.Fatalf("missing current session: result=%+v err=%v", result, err)
	}

	registry := tools.NewRegistry()
	cfg := &config.Config{}
	cfg.Session.SkillDir = t.TempDir()
	cfg.Session.Dir = t.TempDir()
	Register(registry, cfg, permission.New(permission.ModeDefault))
	registered, ok := registry.Get("Artifact")
	if !ok || registered.Name() != "Artifact" {
		t.Fatalf("Artifact not registered: %v, %v", registered, ok)
	}

	cfg.Tools.Disabled = []string{"Artifact"}
	disabledRegistry := tools.NewRegistry()
	Register(disabledRegistry, cfg, permission.New(permission.ModeDefault))
	if _, ok := disabledRegistry.Get("Artifact"); ok {
		t.Fatal("disabled Artifact tool was registered")
	}
}

func setArtifactTestSession(t *testing.T, id string) {
	t.Helper()
	previous := tasks.CurrentSessionID()
	tasks.SetCurrentSessionID(id)
	t.Cleanup(func() { tasks.SetCurrentSessionID(previous) })
}
