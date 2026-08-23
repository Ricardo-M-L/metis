package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/artifact"
)

func artifactCommandTestStore(t *testing.T) artifactCommandStore {
	t.Helper()
	store, err := artifact.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	return artifactStoreAdapter{store: store}
}

func TestArtifactCommandRequiresExplicitDeleteConfirmation(t *testing.T) {
	_, err := parseArtifactCommand([]string{"delete", "artifact-id", "--session", "session-a"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("delete without --yes error = %v", err)
	}
}

func TestArtifactCommandRequiresSessionOwnership(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"show", "artifact-id"}, {"delete", "artifact-id", "--yes"}} {
		if _, err := parseArtifactCommand(args); err == nil || !strings.Contains(err.Error(), "--session") {
			t.Errorf("parseArtifactCommand(%v) error = %v, want --session guidance", args, err)
		}
	}
}

func TestArtifactCommandCreateUpdateShowExportDelete(t *testing.T) {
	store := artifactCommandTestStore(t)
	var out bytes.Buffer
	if err := runArtifactCommand(
		[]string{"create", "-", "--session", "session-a", "--title", "Dashboard", "--json"},
		strings.NewReader(`<h1>v1</h1><script>alert(1)</script>`), &out, store,
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	items, err := store.List("session-a")
	if err != nil || len(items) != 1 {
		t.Fatalf("list after create = %#v, %v", items, err)
	}
	id := items[0].ID

	out.Reset()
	if err := runArtifactCommand(
		[]string{"update", id, "-", "--session", "session-a"},
		strings.NewReader(`<h1>v2</h1>`), &out, store,
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(out.String(), "version 2") {
		t.Fatalf("update output = %q", out.String())
	}

	out.Reset()
	if err := runArtifactCommand([]string{"show", id, "--session", "session-a"}, nil, &out, store); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out.String(), "current version: 2") {
		t.Fatalf("show output = %q", out.String())
	}
	if err := runArtifactCommand([]string{"show", id, "--session", "session-b"}, nil, &out, store); !errors.Is(err, artifact.ErrOwnerMismatch) {
		t.Fatalf("cross-session show error = %v", err)
	}

	exported := filepath.Join(t.TempDir(), "dashboard.html")
	out.Reset()
	if err := runArtifactCommand([]string{"export", id, "--session", "session-a", "--version", "1", "--out", exported}, nil, &out, store); err != nil {
		t.Fatalf("export: %v", err)
	}
	body, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("v1")) || bytes.Contains(bytes.ToLower(body), []byte("<script")) {
		t.Fatalf("exported sanitized v1 = %q", body)
	}

	out.Reset()
	if err := runArtifactCommand([]string{"delete", id, "--session", "session-a", "--yes"}, nil, &out, store); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get("session-a", id); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
}

func TestArtifactCommandListJSONAndAliasDiscovery(t *testing.T) {
	store := artifactCommandTestStore(t)
	if _, err := store.Create("session-a", "One", "<p>one</p>"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runArtifactCommand([]string{"list", "--session", "session-a", "--json"}, nil, &out, store); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"title": "One"`) {
		t.Fatalf("list JSON = %q", out.String())
	}
	if idx, ok := findEarlySubcommand([]string{"--mode", "ask", "artifacts", "list"}, 16); !ok || idx != 2 {
		t.Fatalf("findEarlySubcommand artifacts = %d, %v", idx, ok)
	}
}
