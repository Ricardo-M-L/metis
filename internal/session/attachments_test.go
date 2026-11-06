package session_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestAttachmentsResumeAcrossProviderWires(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.WriteHeader("vision", "test", ""); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString([]byte("test-image-payload"))
	original := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "call1", ToolName: "ViewImage", ToolInput: map[string]any{}}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "call1", ToolResult: "exact stored preview", ToolResultBlocks: []llm.ContentBlock{{Type: "text", Text: "exact stored preview"}, {Type: "image", MediaType: "image/png", Data: data}, {Type: "text", Text: "ordered tail"}}}, {Type: "image", MediaType: "image/jpeg", Data: data}}},
	}
	for _, m := range original {
		if err = store.AppendMessage("vision", m); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(store.Dir, "vision.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), data) || !strings.Contains(string(raw), "attachment_hash") {
		t.Fatal("inline image persisted instead of committed reference")
	}
	_, restored, err := store.Load("vision")
	if err != nil {
		t.Fatal(err)
	}
	if restored[1].Content[0].ToolResultBlocks[1].Data != data || restored[1].Content[1].Data != data {
		t.Fatal("image data missing")
	}
	// Compare actual adapter request bodies before/after restart, not just JSON.
	for _, kind := range []string{"anthropic", "openai", "responses"} {
		t.Run(kind, func(t *testing.T) {
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(body))
				w.Header().Set("Content-Type", "application/json")
				if kind == "anthropic" {
					io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				} else if kind == "responses" {
					io.WriteString(w, `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				} else {
					io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
				}
			}))
			defer server.Close()
			for _, messages := range [][]llm.Message{original, restored} {
				req := llm.Request{Messages: messages}
				if kind == "anthropic" {
					_, err = anthropic.New("test", server.URL, "claude-test", 100, time.Second, "").Complete(context.Background(), req)
				} else if kind == "responses" {
					_, err = openai.NewResponses("test", server.URL, "gpt-4o", 100, time.Second, 0).Complete(context.Background(), req)
				} else {
					_, err = openai.New("test", server.URL, "gpt-4o", 100, time.Second, 0).Complete(context.Background(), req)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(bodies) != 2 || !reflect.DeepEqual(bodies[0], bodies[1]) {
				t.Fatalf("restored model-visible wire differs: %v", bodies)
			}
			if !strings.Contains(bodies[1], data) || !strings.Contains(bodies[1], "ordered tail") {
				t.Fatal("wire missing image or ordered text")
			}
			if kind == "responses" && (!strings.Contains(bodies[1], "data:image/png;base64,"+data) || !strings.Contains(bodies[1], "function_call_output")) {
				t.Fatal("Responses nested tool image or paired output missing")
			}
			if strings.Contains(bodies[1], "attachment_hash") {
				t.Fatal("storage metadata leaked to provider")
			}
		})
	}
	files, _ := os.ReadDir(filepath.Join(store.Dir, "vision.jsonl.attachments"))
	if len(files) != 1 {
		t.Fatalf("deduplication: %d blobs", len(files))
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "vision.jsonl.attachments", files[0].Name()), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("vision"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("corrupt image must fail, got %v", err)
	}
	os.Remove(filepath.Join(store.Dir, "vision.jsonl.attachments", files[0].Name()))
	if _, _, err := store.Load("vision"); err == nil {
		t.Fatal("missing image silently degraded")
	}
}
