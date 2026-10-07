package builtin

import (
	"context"
	"encoding/json"
	"math"
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

func TestArtifactConditionalUpdateSchemaAndPermission(t *testing.T) {
	tool := NewArtifact(permission.New(permission.ModeDefault))
	properties := tool.InputSchema()["properties"].(map[string]any)
	expected, ok := properties["expected_version"].(map[string]any)
	if !ok || expected["type"] != "integer" || expected["minimum"] != 1 {
		t.Fatalf("expected_version schema = %#v", properties["expected_version"])
	}
	in := map[string]any{"action": "update", "expected_version": 1}
	if tool.IsReadOnly(in) || tool.Concurrency(in) != tools.ConcurrencyExclusive {
		t.Fatal("conditional update must remain exclusive and state-changing")
	}
	if decision, _ := tool.CanUse(context.Background(), in); decision != tools.PermissionAsk {
		t.Fatalf("conditional update permission = %v, want ask", decision)
	}
}

func TestArtifactConditionalUpdatePreservesNewerVersionAndLegacyCalls(t *testing.T) {
	store, err := artifactstore.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	tool := NewArtifact(nil, store)
	setArtifactTestSession(t, "session-a")
	created, err := store.Create("session-a", "Original", "<p>first</p>")
	if err != nil {
		t.Fatal(err)
	}
	// Omitting expected_version keeps the existing CLI update behavior.
	legacy, err := tool.Execute(context.Background(), map[string]any{
		"action": "update", "id": created.ID, "title": "Current", "html": "<p>newer</p>",
	})
	if err != nil || legacy.IsError || legacy.Presentation["version"] != 2 {
		t.Fatalf("legacy update: result=%+v err=%v", legacy, err)
	}
	stale, err := tool.Execute(context.Background(), map[string]any{
		"action": "update", "id": created.ID, "title": "Stale", "html": "<p>stale</p>", "expected_version": 1,
	})
	if err != nil || stale == nil || !stale.IsError || !strings.Contains(stale.Output, "version changed") || !strings.Contains(stale.Output, "read the current artifact") || stale.Presentation != nil {
		t.Fatalf("stale update must request a fresh read: result=%+v err=%v", stale, err)
	}
	manifest, err := store.Get("session-a", created.ID)
	if err != nil || manifest.Title != "Current" || manifest.CurrentVersion != 2 || len(manifest.Versions) != 2 {
		t.Fatalf("stale tool update changed metadata: %+v, %v", manifest, err)
	}
	body, _, err := store.ReadVersion("session-a", created.ID, 0)
	if err != nil || !strings.Contains(string(body), "newer") {
		t.Fatalf("stale tool update changed content: %q, %v", body, err)
	}
	for _, expected := range []any{float64(2), int64(3)} {
		fresh, err := tool.Execute(context.Background(), map[string]any{
			"action": "update", "id": created.ID, "html": "<p>fresh</p>", "expected_version": expected,
		})
		if err != nil || fresh.IsError {
			t.Fatalf("valid expected version %v: result=%+v err=%v", expected, fresh, err)
		}
	}
	tasks.SetCurrentSessionID("session-b")
	foreign, err := tool.Execute(context.Background(), map[string]any{
		"action": "update", "id": created.ID, "html": "<p>foreign</p>", "expected_version": 1,
	})
	if err != nil || !foreign.IsError || !strings.Contains(foreign.Output, artifactstore.ErrOwnerMismatch.Error()) || strings.Contains(foreign.Output, "current") || strings.Contains(foreign.Output, "Current") || strings.Contains(foreign.Output, store.Root()) {
		t.Fatalf("foreign conditional update leaked metadata: result=%+v err=%v", foreign, err)
	}
}

func TestArtifactConditionalUpdateRejectsInvalidExpectedVersion(t *testing.T) {
	store, err := artifactstore.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	tool := NewArtifact(nil, store)
	setArtifactTestSession(t, "session-a")
	created, err := store.Create("session-a", "Original", "<p>first</p>")
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []any{nil, 0, -1, int64(-1), 0.0, 1.5, "1", true, []any{1}, math.NaN(), math.Inf(1), float64(math.MaxInt)} {
		result, err := tool.Execute(context.Background(), map[string]any{
			"action": "update", "id": created.ID, "html": "<p>invalid update</p>", "expected_version": invalid,
		})
		if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "expected_version") {
			t.Fatalf("invalid expected version %#v: result=%+v err=%v", invalid, result, err)
		}
	}
	manifest, err := store.Get("session-a", created.ID)
	if err != nil || manifest.CurrentVersion != 1 || len(manifest.Versions) != 1 {
		t.Fatalf("invalid expected versions wrote updates: %+v, %v", manifest, err)
	}
}

func setArtifactTestSession(t *testing.T, id string) {
	t.Helper()
	previous := tasks.CurrentSessionID()
	tasks.SetCurrentSessionID(id)
	t.Cleanup(func() { tasks.SetCurrentSessionID(previous) })
}
