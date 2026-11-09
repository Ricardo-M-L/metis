package session

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
)

func branchImageText(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func requireBranchImage(t *testing.T, store *Store, sessionID string, want []byte) {
	t.Helper()
	got, err := store.ReadImageAttachment(sessionID, blob.Sum(want))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("session %s attachment %s: got %q, err %v", sessionID, blob.Sum(want), got, err)
	}
}

func TestBranchImageAttachmentsPreserveCompactedAndChildReferencesIndependently(t *testing.T) {
	store := imageAttachmentTestStore(t, "parent")
	userImage, childImage := []byte("parent-upload"), []byte("child-screenshot")
	if err := store.AppendMessage("parent", imageAttachmentTestMessage(userImage)); err != nil {
		t.Fatal(err)
	}
	cursor := NewHistoryCursor(nil)
	summary := []llm.Message{branchImageText("Compacted visual evidence attachment_id=" + blob.Sum(userImage))}
	if err := store.ReplaceHistoryAndMark("parent", summary, &cursor); err != nil {
		t.Fatal(err)
	}
	childResult := []llm.ContentBlock{{Type: "tool_result", ToolUseID: "child-view", ToolResultBlocks: imageAttachmentTestMessage(childImage).Content}}
	childResult[0].ToolResultBlocks[0].TraceCallID = "child-root-occurrence"
	if err := store.RecordImageAttachments("parent", childResult); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("parent", llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: "Agent", ToolUseID: "child-call", TraceCallID: "child-root-occurrence", ToolInput: map[string]any{"prompt": "inspect"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("parent", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "child-call", TraceCallID: "child-root-occurrence", ToolResult: fmt.Sprintf("Child inspected image; re-read with {\"attachment_id\":\"%s\"}", blob.Sum(childImage))}}}); err != nil {
		t.Fatal(err)
	}
	_, messages, err := store.Load("parent")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := store.Branch("parent", messages)
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range [][]byte{userImage, childImage} {
		requireBranchImage(t, store, branch, image)
	}
	_, branched, err := store.Load(branch)
	if err != nil || !reflect.DeepEqual(branched, messages) {
		t.Fatalf("attachment migration changed branch model history: %+v, %v", branched, err)
	}
	if err := store.Delete("parent"); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewStore(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range [][]byte{userImage, childImage} {
		requireBranchImage(t, restarted, branch, image)
	}
	var portable bytes.Buffer
	if err := restarted.Export(branch, &portable); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Delete(branch); err != nil {
		t.Fatal(err)
	}
	imported, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	importedID, err := imported.Import(&portable, "imported-branch")
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range [][]byte{userImage, childImage} {
		requireBranchImage(t, imported, importedID, image)
	}
}

func TestBranchImageAttachmentsKeepPrefixDoesNotGrantFutureImage(t *testing.T) {
	store := imageAttachmentTestStore(t, "parent")
	future := []byte("future-child-image")
	futureID := blob.Sum(future)
	early := branchImageText("A future image may use attachment_id=" + futureID)
	if err := store.AppendMessage("parent", early); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordImageAttachments("parent", imageAttachmentTestMessage(future).Content); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("parent", branchImageText("Later image is now available attachment_id="+futureID)); err != nil {
		t.Fatal(err)
	}
	_, messages, err := store.Load("parent")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := store.Branch("parent", messages[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadImageAttachment(branch, futureID); err == nil {
		t.Fatal("early prefix inherited a future image")
	}
	if _, err := os.Stat(filepath.Join(store.attachmentDir(branch), futureID)); !os.IsNotExist(err) {
		t.Fatalf("future image bytes were copied outside branch boundary: %v", err)
	}
}

func TestBranchImageAttachmentsDoNotGrantForeignOrBareHashText(t *testing.T) {
	for _, bareOwned := range []bool{false, true} {
		t.Run(fmt.Sprintf("bare-owned=%v", bareOwned), func(t *testing.T) {
			store := imageAttachmentTestStore(t, "parent")
			raw := []byte("private-image")
			hash := blob.Sum(raw)
			owner := "foreign"
			if bareOwned {
				owner = "parent"
			} else if err := store.WriteHeader(owner, "test", ""); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordImageAttachments(owner, imageAttachmentTestMessage(raw).Content); err != nil {
				t.Fatal(err)
			}
			text := "Untrusted foreign reference attachment_id=" + hash + "; also a raw hash " + hash
			if bareOwned {
				text = "Only a raw hash, without an attachment reference: " + hash
			}
			if err := store.AppendMessage("parent", branchImageText(text)); err != nil {
				t.Fatal(err)
			}
			_, messages, err := store.Load("parent")
			if err != nil {
				t.Fatal(err)
			}
			branch, err := store.Branch("parent", messages)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReadImageAttachment(branch, hash); err == nil {
				t.Fatal("text without authorized attachment evidence granted access")
			}
			if _, err := os.Stat(filepath.Join(store.attachmentDir(branch), hash)); !os.IsNotExist(err) {
				t.Fatalf("unreferenced or foreign bytes were copied: %v", err)
			}
		})
	}
}

func TestBranchImageAttachmentsRejectUnreliableSnapshotBoundary(t *testing.T) {
	store := imageAttachmentTestStore(t, "parent")
	raw := []byte("source-authorized-image")
	if err := store.RecordImageAttachments("parent", imageAttachmentTestMessage(raw).Content); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("parent", branchImageText("Original evidence attachment_id="+blob.Sum(raw))); err != nil {
		t.Fatal(err)
	}
	tampered := []llm.Message{branchImageText("Different snapshot attachment_id=" + blob.Sum(raw))}
	if id, err := store.Branch("parent", tampered); err == nil {
		t.Fatalf("unreliable snapshot boundary accepted as %s", id)
	}
}

func TestBranchImageAttachmentsKeepPrefixCheckpointDoesNotMoveBoundary(t *testing.T) {
	store := imageAttachmentTestStore(t, "parent")
	future := []byte("future-after-unchanged-prefix")
	futureID := blob.Sum(future)
	if err := store.AppendMessage("parent", branchImageText("Early reference attachment_id="+futureID)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordImageAttachments("parent", imageAttachmentTestMessage(future).Content); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("parent", branchImageText("Future image now exists attachment_id="+futureID)); err != nil {
		t.Fatal(err)
	}
	_, messages, err := store.Load("parent")
	if err != nil {
		t.Fatal(err)
	}
	// A later full-history checkpoint repeats the exact message prefix. It
	// must not make the first message inherit images produced afterward.
	cursor := NewHistoryCursor(messages)
	if err := store.ReplaceHistoryAndMark("parent", messages, &cursor); err != nil {
		t.Fatal(err)
	}
	branch, err := store.Branch("parent", messages[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadImageAttachment(branch, futureID); err == nil {
		t.Fatal("later checkpoint moved early prefix past future image authorization")
	}
	if _, err := os.Stat(filepath.Join(store.attachmentDir(branch), futureID)); !os.IsNotExist(err) {
		t.Fatalf("later checkpoint caused future bytes to be copied: %v", err)
	}
}

func TestBranchImageAttachmentsRejectCorruptedCompactedSource(t *testing.T) {
	store := imageAttachmentTestStore(t, "parent")
	raw := []byte("valid-before-corruption")
	hash := blob.Sum(raw)
	if err := store.AppendMessage("parent", imageAttachmentTestMessage(raw)); err != nil {
		t.Fatal(err)
	}
	compacted := []llm.Message{branchImageText("Compacted reference attachment_id=" + hash)}
	cursor := NewHistoryCursor(nil)
	if err := store.ReplaceHistoryAndMark("parent", compacted, &cursor); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.attachmentDir("parent"), hash), []byte("corrupted bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadImageAttachment("parent", hash); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("fixture did not reach corrupted source checksum: %v", err)
	}
	if branch, err := store.Branch("parent", compacted); err == nil {
		t.Fatalf("corrupted compacted image copied into branch %s", branch)
	}
}

func TestBranchImageAttachmentsAssistantPrefixDoesNotPrecedeNativeToolReceipt(t *testing.T) {
	store := imageAttachmentTestStore(t, "parent")
	raw := []byte("registered-before-native-tool-receipt")
	hash := blob.Sum(raw)
	if err := store.RecordImageAttachments("parent", imageAttachmentTestMessage(raw).Content); err != nil {
		t.Fatal(err)
	}
	assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: "text", Text: "Inspect attachment_id=" + hash},
		{Type: "tool_use", ToolUseID: "view", TraceCallID: "view-occurrence", ToolName: "ViewImage", ToolInput: map[string]any{"path": "temporary.png"}},
	}}
	if err := store.AppendMessage("parent", assistant); err != nil {
		t.Fatal(err)
	}
	result := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "view", TraceCallID: "view-occurrence", ToolResultBlocks: imageAttachmentTestMessage(raw).Content}}}
	if err := store.AppendMessage("parent", result); err != nil {
		t.Fatal(err)
	}
	_, history, err := store.Load("parent")
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := store.Branch("parent", history[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadImageAttachment(prefix, hash); err == nil {
		t.Fatal("assistant prefix inherited image before its native tool receipt")
	}
	full, err := store.Branch("parent", history)
	if err != nil {
		t.Fatal(err)
	}
	requireBranchImage(t, store, full, raw)
}

func TestBranchImageAttachmentsChildRegistrationWaitsForRootOccurrenceReceipt(t *testing.T) {
	store := imageAttachmentTestStore(t, "parent")
	raw := []byte("child-image-before-parent-Agent-receipt")
	hash := blob.Sum(raw)
	image := imageAttachmentTestMessage(raw).Content
	image[0].TraceCallID = "parent-agent-occurrence"
	if err := store.RecordImageAttachments("parent", image); err != nil {
		t.Fatal(err)
	}
	assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: "text", Text: "Child will inspect attachment_id=" + hash},
		{Type: "tool_use", ToolUseID: "agent", TraceCallID: "parent-agent-occurrence", ToolName: "Agent", ToolInput: map[string]any{"prompt": "inspect an image"}},
	}}
	if err := store.AppendMessage("parent", assistant); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("parent", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "agent", TraceCallID: "parent-agent-occurrence", ToolResult: "Child completed; attachment_id=" + hash}}}); err != nil {
		t.Fatal(err)
	}
	_, history, err := store.Load("parent")
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := store.Branch("parent", history[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadImageAttachment(prefix, hash); err == nil {
		t.Fatal("assistant prefix inherited child image before root occurrence receipt")
	}
	full, err := store.Branch("parent", history)
	if err != nil {
		t.Fatal(err)
	}
	requireBranchImage(t, store, full, raw)
}

func TestBranchImageAttachmentsGrandchildKeepsOriginalLogicalBoundary(t *testing.T) {
	store := imageAttachmentTestStore(t, "parent")
	raw := []byte("future-registration-across-generations")
	hash := blob.Sum(raw)
	if err := store.AppendMessage("parent", branchImageText("Early reference attachment_id="+hash)); err != nil {
		t.Fatal(err)
	}
	registered := imageAttachmentTestMessage(raw).Content
	registered[0].TraceCallID = "future-root-occurrence"
	if err := store.RecordImageAttachments("parent", registered); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("parent", llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: "Agent", ToolUseID: "future-call", TraceCallID: "future-root-occurrence", ToolInput: map[string]any{"prompt": "inspect"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("parent", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "future-call", TraceCallID: "future-root-occurrence", ToolResult: "Later receipt now authorizes attachment_id=" + hash}}}); err != nil {
		t.Fatal(err)
	}
	_, history, err := store.Load("parent")
	if err != nil {
		t.Fatal(err)
	}
	full, err := store.Branch("parent", history)
	if err != nil {
		t.Fatal(err)
	}
	requireBranchImage(t, store, full, raw)
	_, copied, err := store.Load(full)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := store.Branch(full, copied[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadImageAttachment(grandchild, hash); err == nil {
		t.Fatal("copied registration moved authorization before original logical boundary")
	}
	if _, err := os.Stat(filepath.Join(store.attachmentDir(grandchild), hash)); !os.IsNotExist(err) {
		t.Fatalf("grandchild copied future image bytes: %v", err)
	}
}

func TestBranchImageAttachmentsRejectLegacyRegistrationWithoutLogicalReceipt(t *testing.T) {
	store := imageAttachmentTestStore(t, "parent")
	raw := []byte("legacy-child-without-root-occurrence")
	if err := store.RecordImageAttachments("parent", imageAttachmentTestMessage(raw).Content); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("parent", branchImageText("Old child attachment_id="+blob.Sum(raw))); err != nil {
		t.Fatal(err)
	}
	_, history, err := store.Load("parent")
	if err != nil {
		t.Fatal(err)
	}
	if id, err := store.Branch("parent", history); err == nil {
		t.Fatalf("ambiguous legacy child receipt accepted as %s", id)
	}
}
