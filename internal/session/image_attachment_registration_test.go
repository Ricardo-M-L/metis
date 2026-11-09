package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
)

func TestRecordImageAttachmentsRemainLogOnlyAndPortable(t *testing.T) {
	s := imageAttachmentTestStore(t, "owner")
	visible := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "visible parent input"}}}
	if err := s.AppendMessage("owner", visible); err != nil {
		t.Fatal(err)
	}
	raw := []byte("child-native-image")
	blocks := []llm.ContentBlock{{Type: "tool_result", ToolUseID: "child-view", ToolResultBlocks: imageAttachmentTestMessage(raw).Content}}
	if err := s.RecordImageAttachments("owner", blocks); err != nil {
		t.Fatal(err)
	}
	_, history, err := s.Load("owner")
	if err != nil || len(history) != 1 || history[0].Content[0].Text != visible.Content[0].Text {
		t.Fatalf("log-only image changed model history: %+v, %v", history, err)
	}
	_, count, err := s.LoadHeader("owner")
	if err != nil || count != 1 {
		t.Fatalf("log-only image changed header message count: %d, %v", count, err)
	}
	var exported bytes.Buffer
	if err := s.Export("owner", &exported); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exported.String(), `"type":"image_attachment"`) {
		t.Fatal("export lost explicit attachment registration")
	}
	imported, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := imported.Import(&exported, "imported")
	if err != nil {
		t.Fatal(err)
	}
	got, err := imported.ReadImageAttachment(id, blob.Sum(raw))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("portable attachment registration: %q, %v", got, err)
	}
	_, history, err = imported.Load(id)
	if err != nil || len(history) != 1 {
		t.Fatalf("import changed model history: %+v, %v", history, err)
	}
}

func TestRecordImageAttachmentsFsyncFailureDoesNotAuthorizeOrphanBlob(t *testing.T) {
	s := imageAttachmentTestStore(t, "owner")
	raw := []byte("committed-before-reference")
	before, _ := os.ReadFile(s.path("owner"))
	writes := 0
	s.syncFile = func(file *os.File) error {
		writes++
		if writes == 1 {
			return errors.New("injected ledger fsync failure")
		}
		return file.Sync()
	}
	if err := s.RecordImageAttachments("owner", imageAttachmentTestMessage(raw).Content); err == nil {
		t.Fatal("failed ledger fsync reported success")
	}
	after, _ := os.ReadFile(s.path("owner"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed durable registration left a visible ledger reference")
	}
	if _, err := os.Stat(filepath.Join(s.attachmentDir("owner"), blob.Sum(raw))); err != nil {
		t.Fatal("test did not reach committed-blob/orphan boundary")
	}
	if _, err := s.ReadImageAttachment("owner", blob.Sum(raw)); err == nil {
		t.Fatal("orphan blob from failed registration became readable")
	}
	// Supplying a bare hash cannot turn the orphan into a readable image.
	if err := s.RecordImageAttachments("owner", []llm.ContentBlock{{Type: "image", AttachmentHash: blob.Sum(raw)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadImageAttachment("owner", blob.Sum(raw)); err == nil {
		t.Fatal("bare hash registration granted authority")
	}
	s.syncFile = nil
	if err := s.RecordImageAttachments("owner", imageAttachmentTestMessage(raw).Content); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadImageAttachment("owner", blob.Sum(raw)); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("registration retry: %q, %v", got, err)
	}
}

func TestRecordImageAttachmentsBlobFailureDoesNotAppendReference(t *testing.T) {
	s := imageAttachmentTestStore(t, "owner")
	before, _ := os.ReadFile(s.path("owner"))
	if err := os.WriteFile(s.attachmentDir("owner"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordImageAttachments("owner", imageAttachmentTestMessage([]byte("image")).Content); err == nil {
		t.Fatal("invalid blob destination reported success")
	}
	after, _ := os.ReadFile(s.path("owner"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed blob write appended a readable reference")
	}
}
