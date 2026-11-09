package session

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
)

func imageAttachmentTestStore(t *testing.T, id string) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteHeader(id, "test", ""); err != nil {
		t.Fatal(err)
	}
	return s
}

func imageAttachmentTestMessage(raw []byte) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "image", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(raw)}}}
}

func TestReadImageAttachmentRetainsHistoricalUserAndNestedToolImages(t *testing.T) {
	s := imageAttachmentTestStore(t, "chat")
	user, toolImage := []byte("uploaded-image"), []byte("tool-screenshot")
	if err := s.AppendMessage("chat", imageAttachmentTestMessage(user)); err != nil {
		t.Fatal(err)
	}
	toolMessage := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "view-1", ToolResultBlocks: []llm.ContentBlock{{Type: "text", Text: "screenshot"}, {Type: "tool_result", ToolResultBlocks: imageAttachmentTestMessage(toolImage).Content}}}}}
	if err := s.AppendMessage("chat", toolMessage); err != nil {
		t.Fatal(err)
	}
	cursor := NewHistoryCursor(nil)
	if err := s.ReplaceHistoryAndMark("chat", []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "compacted summary"}}}}, &cursor); err != nil {
		t.Fatal(err)
	}
	// Reopen rather than relying on any process-local image registry.
	restarted, err := NewStore(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{user, toolImage} {
		got, err := restarted.ReadImageAttachment("chat", blob.Sum(want))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("historical attachment %s: got %q, err %v", blob.Sum(want), got, err)
		}
	}
}

func TestReadImageAttachmentRejectsForeignAndUnreferencedBlobs(t *testing.T) {
	s := imageAttachmentTestStore(t, "origin")
	if err := s.WriteHeader("other", "test", ""); err != nil {
		t.Fatal(err)
	}
	raw := []byte("private-origin-image")
	if err := s.AppendMessage("origin", imageAttachmentTestMessage(raw)); err != nil {
		t.Fatal(err)
	}
	hash := blob.Sum(raw)
	if _, err := s.ReadImageAttachment("other", hash); err == nil {
		t.Fatal("foreign session hash authorized")
	}
	if _, err := blob.Put(s.attachmentDir("other"), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadImageAttachment("other", hash); err == nil {
		t.Fatal("orphaned blob without a ledger image reference authorized")
	}
	// Mentioning an ID in prose does not establish image ownership.
	if err := s.AppendMessage("other", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: hash}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadImageAttachment("other", hash); err == nil {
		t.Fatal("textual hash mention authorized")
	}
}

func TestReadImageAttachmentValidatesIdentityAndBlob(t *testing.T) {
	s := imageAttachmentTestStore(t, "chat")
	raw := []byte("original-image")
	hash := blob.Sum(raw)
	if err := s.AppendMessage("chat", imageAttachmentTestMessage(raw)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../" + hash, "https://example.com/" + hash, strings.ToUpper(hash), hash[:63]} {
		if _, err := s.ReadImageAttachment("chat", bad); err == nil {
			t.Fatalf("invalid attachment identity %q accepted", bad)
		}
	}
	for _, bad := range []string{"", "..", "../chat", "chat/other", "chat\\other", "chat\n"} {
		if _, err := s.ReadImageAttachment(bad, hash); err == nil {
			t.Fatalf("invalid session identity %q accepted", bad)
		}
	}
	var nilStore *Store
	if _, err := nilStore.ReadImageAttachment("chat", hash); err == nil {
		t.Fatal("nil store accepted")
	}
	file := filepath.Join(s.attachmentDir("chat"), hash)
	if err := os.WriteFile(file, []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadImageAttachment("chat", hash); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupted blob: %v", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadImageAttachment("chat", hash); err == nil {
		t.Fatal("missing blob accepted")
	}
}

func TestReadImageAttachmentSupportsLegacyInlineAndSnapshotReferences(t *testing.T) {
	s := imageAttachmentTestStore(t, "legacy")
	raw := []byte("legacy-inline")
	entry := Entry{Type: "history_replace", Messages: []llm.Message{imageAttachmentTestMessage(raw)}}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.path("legacy"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadImageAttachment("legacy", blob.Sum(raw))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("legacy inline read: %q, %v", got, err)
	}
}

func TestReadImageAttachmentToleratesTornTailButRejectsMiddleCorruption(t *testing.T) {
	for _, middle := range []bool{false, true} {
		t.Run(map[bool]string{false: "tail", true: "middle"}[middle], func(t *testing.T) {
			s := imageAttachmentTestStore(t, "chat")
			raw := []byte("durable-image")
			if err := s.AppendMessage("chat", imageAttachmentTestMessage(raw)); err != nil {
				t.Fatal(err)
			}
			ledger, err := os.ReadFile(s.path("chat"))
			if err != nil {
				t.Fatal(err)
			}
			ledger = append(ledger, []byte(`{"type":"message",`)...)
			if middle {
				ledger = append(ledger, []byte("\n{\"type\":\"feedback\"}\n")...)
			}
			if err := os.WriteFile(s.path("chat"), ledger, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := s.ReadImageAttachment("chat", blob.Sum(raw))
			if middle && err == nil {
				t.Fatal("middle corruption silently ignored")
			}
			if !middle && (err != nil || !bytes.Equal(got, raw)) {
				t.Fatalf("clean prefix unavailable after torn tail: %q, %v", got, err)
			}
		})
	}
}

func TestReadImageAttachmentConcurrentSessionsStayScoped(t *testing.T) {
	s := imageAttachmentTestStore(t, "alpha")
	if err := s.WriteHeader("beta", "test", ""); err != nil {
		t.Fatal(err)
	}
	images := map[string][]byte{"alpha": []byte("alpha-image"), "beta": []byte("beta-image")}
	for id, raw := range images {
		if err := s.AppendMessage(id, imageAttachmentTestMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for id, raw := range images {
		for range 8 {
			wg.Add(1)
			go func(id string, raw []byte) {
				defer wg.Done()
				got, err := s.ReadImageAttachment(id, blob.Sum(raw))
				if err != nil || !bytes.Equal(got, raw) {
					t.Errorf("%s own image = %q, %v", id, got, err)
				}
				for foreign, other := range images {
					if foreign != id {
						if _, err := s.ReadImageAttachment(id, blob.Sum(other)); err == nil {
							t.Errorf("%s accessed %s image", id, foreign)
						}
					}
				}
			}(id, raw)
		}
	}
	wg.Wait()
}

func imageAttachmentSymlinkPNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestReadImageAttachmentRejectsSessionLedgerSymlink(t *testing.T) {
	s := imageAttachmentTestStore(t, "chat")
	raw := imageAttachmentSymlinkPNG(t)
	if err := s.AppendMessage("chat", imageAttachmentTestMessage(raw)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadImageAttachment("chat", blob.Sum(raw)); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("regular ledger prerequisite: %q, %v", got, err)
	}
	ledger, target := s.path("chat"), s.path("chat")+".target"
	if err := os.Rename(ledger, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadImageAttachment("chat", blob.Sum(raw)); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("session ledger symlink must be rejected: %v", err)
	}
}

func TestReadImageAttachmentRejectsAttachmentDirectorySymlink(t *testing.T) {
	s := imageAttachmentTestStore(t, "chat")
	raw := imageAttachmentSymlinkPNG(t)
	if err := s.AppendMessage("chat", imageAttachmentTestMessage(raw)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadImageAttachment("chat", blob.Sum(raw)); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("regular attachment directory prerequisite: %q, %v", got, err)
	}
	dir, target := s.attachmentDir("chat"), s.attachmentDir("chat")+".target"
	if err := os.Rename(dir, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadImageAttachment("chat", blob.Sum(raw)); err == nil || !strings.Contains(err.Error(), "attachment directory") {
		t.Fatalf("attachment directory symlink must be rejected: %v", err)
	}
}
