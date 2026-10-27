package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlSessionIsolationAndResume(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	workspace := t.TempDir()
	a := NewManager("session-a", workspace, "")
	if _, err := a.Command(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	b := NewManager("session-b", workspace, "")
	if !b.ControlStatus().Enabled {
		t.Error("session A switch leaked into session B in the same workspace")
	}
	restored := NewManager("session-a", workspace, "")
	if restored.ControlStatus().Enabled || !restored.AutomaticDisabled() {
		t.Error("same session lost its persisted switch on resume")
	}
	data, err := json.Marshal(restored.ControlStatus())
	if err != nil {
		t.Fatal(err)
	}
	var identity map[string]any
	if err := json.Unmarshal(data, &identity); err != nil || identity["sessionId"] != "session-a" {
		t.Errorf("status lacks durable session owner: %s %v", data, err)
	}
	if _, err := restored.Command(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	if err := a.RefreshControl(); err != nil || a.AutomaticDisabled() {
		t.Errorf("same-session CLI/Desktop refresh did not synchronize: %v", err)
	}
}

func TestControlActiveSessionDoesNotLockSiblingSession(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	workspace := t.TempDir()
	a := NewManager("session-a", workspace, "")
	release, _, err := a.BeginRun()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	b := NewManager("session-b", workspace, "")
	if _, err := b.Command(context.Background(), "off"); err != nil {
		t.Errorf("active session A blocked switching unrelated session B: %v", err)
	}
	same := NewManager("session-a", workspace, "")
	if _, err := same.Command(context.Background(), "off"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("active session's own switch must remain locked: %v", err)
	}
}

func TestControlIgnoresLegacyWorkspaceBroadcastAndFreshStatusDoesNotPersist(t *testing.T) {
	metisHome := t.TempDir()
	t.Setenv("METIS_HOME", metisHome)
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(workspace))
	legacyPath := filepath.Join(metisHome, "checkpoint-controls", hex.EncodeToString(sum[:])+".json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(map[string]any{"version": 1, "workspace": workspace, "enabled": false, "roots": []string{"."}, "revision": "legacy"})
	if err := os.WriteFile(legacyPath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := NewManager("", workspace, "")
	if !fresh.ControlStatus().Enabled {
		t.Error("draft session inherited legacy workspace-wide switch")
	}
	if _, err := fresh.Command(context.Background(), "off"); err == nil {
		t.Error("unbound draft session persisted a switch without a durable session ID")
	}
	next := NewManager("session-new", workspace, "")
	if !next.ControlStatus().Enabled {
		t.Error("new session inherited legacy workspace-wide switch")
	}
	legacyAfter, err := os.ReadFile(legacyPath)
	if err != nil || string(legacyAfter) != string(legacy) {
		t.Errorf("legacy settings were mutated: %s %v", legacyAfter, err)
	}
}

func TestControlValidatesPersistedSessionAndWorkspaceIdentity(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	workspace := t.TempDir()
	m := NewManager("session-a", workspace, "")
	if _, err := m.Command(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	_, path, err := controlPaths("session-a", workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"sessionId", "workspace"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var settings map[string]any
		if err := json.Unmarshal(body, &settings); err != nil {
			t.Fatal(err)
		}
		settings[field] = "foreign-identity"
		settings["enabled"] = true
		encoded, _ := json.Marshal(settings)
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		state := NewManager("session-a", workspace, "").ControlStatus()
		if state.Enabled || state.SessionID != "session-a" || !strings.Contains(state.Reason, "identity mismatch") {
			t.Errorf("accepted foreign %s: %+v", field, state)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	otherWorkspace := NewManager("session-a", t.TempDir(), "").ControlStatus()
	if !otherWorkspace.Enabled {
		t.Error("same session spelling in a different workspace inherited foreign settings")
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if NewManager("session-a", canonical, "").ControlStatus().Enabled {
		t.Error("canonical OS workspace alias failed to resolve the same persisted identity")
	}
}
