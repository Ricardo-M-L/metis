package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestAttachmentExportImportSelfContained(t *testing.T) {
	source, _ := NewStore(t.TempDir())
	if err := source.WriteHeader("origin", "test", ""); err != nil {
		t.Fatal(err)
	}
	m := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "image", MediaType: "image/png", Data: "aW1hZ2U="}, {Type: "tool_result", ToolUseID: "a", ToolResultBlocks: []llm.ContentBlock{{Type: "text", Text: "before"}, {Type: "image", MediaType: "image/jpeg", Data: "aW1hZ2U="}}}}}
	if err := source.AppendMessage("origin", m); err != nil {
		t.Fatal(err)
	}
	var portable bytes.Buffer
	if err := source.Export("origin", &portable); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(portable.String(), "aW1hZ2U=") {
		t.Fatal("export not self-contained")
	}
	// Remove the source entirely: destination must not depend on any source blob.
	if err := os.RemoveAll(source.Dir); err != nil {
		t.Fatal(err)
	}
	dest, _ := NewStore(t.TempDir())
	id, err := dest.Import(&portable, "imported")
	if err != nil {
		t.Fatal(err)
	}
	_, msgs, err := dest.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Content[0].Data != "aW1hZ2U=" || msgs[0].Content[1].ToolResultBlocks[1].MediaType != "image/jpeg" {
		t.Fatal("portable projection lost")
	}
	raw, _ := os.ReadFile(dest.path(id))
	if strings.Contains(string(raw), "aW1hZ2U=") {
		t.Fatal("import did not externalize")
	}
	branch, err := dest.Branch(id, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := dest.Delete(id); err != nil {
		t.Fatal(err)
	}
	_, branched, err := dest.Load(branch)
	if err != nil || branched[0].Content[0].Data != "aW1hZ2U=" {
		t.Fatalf("branch depends on original attachments: %v", err)
	}
	// Legacy inline JSONL remains accepted by the same Import entry point.
	legacy := `{"type":"header","header":{"id":"old"}}` + "\n" + `{"type":"message","message":{"role":"user","content":[{"type":"image","media_type":"image/png","data":"aW1hZ2U="}]}}` + "\n"
	if _, err := dest.Import(strings.NewReader(legacy), "legacy"); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentDeleteOwnedChildRetryAndSymlinkSafety(t *testing.T) {
	for _, failWhich := range []string{"parent", "child"} {
		t.Run(failWhich, func(t *testing.T) {
			store, _ := NewStore(t.TempDir())
			store.WriteHeader("parent", "test", "")
			childStore, _ := NewStore(filepath.Join(store.Dir, "subagents"))
			childStore.WriteHeaderFull(Header{ID: "child", SubAgentOf: "parent"})
			childStore.WriteHeaderFull(Header{ID: "unrelated", SubAgentOf: "other"})
			image := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "image", MediaType: "image/png", Data: "aW1hZ2U="}}}
			for _, pair := range []struct {
				s  *Store
				id string
			}{{store, "parent"}, {childStore, "child"}, {childStore, "unrelated"}} {
				if err := pair.s.AppendMessage(pair.id, image); err != nil {
					t.Fatal(err)
				}
			}
			failedPath := store.attachmentDir("parent")
			if failWhich == "child" {
				failedPath = childStore.attachmentDir("child")
			}
			store.removeAttachments = func(path string) error {
				if path == failedPath {
					return errors.New("injected remove failure")
				}
				return os.RemoveAll(path)
			}
			if err := store.Delete("parent"); err == nil {
				t.Fatal("must report failed sidecar cleanup")
			}
			if _, err := os.Stat(store.path("parent")); err != nil {
				t.Fatal("parent ownership transcript lost before retry")
			}
			if failWhich == "child" {
				if _, err := os.Stat(childStore.path("child")); err != nil {
					t.Fatal("child ownership lost")
				}
			}
			store.removeAttachments = nil
			if err := store.Delete("parent"); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{store.path("parent"), store.attachmentDir("parent"), childStore.path("child"), childStore.attachmentDir("child")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("private remnant %s: %v", path, err)
				}
			}
			if _, err := os.Stat(childStore.attachmentDir("unrelated")); err != nil {
				t.Fatal("unrelated child attachments deleted")
			}
			outside := t.TempDir()
			secret := filepath.Join(outside, "keep")
			os.WriteFile(secret, []byte("private"), 0600)
			if err := os.Symlink(outside, store.attachmentDir("symlink")); err != nil {
				t.Fatal(err)
			}
			if err := store.Delete("symlink"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(secret); err != nil {
				t.Fatal("attachment root symlink traversed")
			}
			if err := store.Delete("parent"); err != nil {
				t.Fatal("retry not idempotent")
			}
		})
	}
}
