package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestSessionHistoryImagesSurviveRestartAndActivation(t *testing.T) {
	// Keep activation's runtime state separate from the developer's sessions,
	// configuration and workspace, as well as the transcript under test.
	t.Setenv("METIS_HOME", t.TempDir())
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	img.SetNRGBA(1, 0, color.NRGBA{B: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	imageBytes := encoded.Bytes()
	imageData := base64.StdEncoding.EncodeToString(imageBytes)
	digest := sha256.Sum256(imageBytes)
	imageHash := hex.EncodeToString(digest[:])
	imageBlock := llm.ContentBlock{Type: "image", MediaType: "image/png", Data: imageData}

	for _, tc := range []struct {
		name    string
		content []llm.ContentBlock
	}{
		{
			name: "text-and-image",
			content: []llm.ContentBlock{
				{Type: "text", Text: "请描述这张图片"},
				imageBlock,
				{Type: "text", Text: "保留图片后的文字"},
			},
		},
		{name: "image-only", content: []llm.ContentBlock{imageBlock}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const sessionID = "history-image"
			if err := store.WriteHeaderFull(session.Header{
				ID: sessionID, Provider: "test-wire", Model: "test-model", System: "system",
				Mode: string(permission.ModeDefault), WorkDir: t.TempDir(), Status: "idle",
			}); err != nil {
				t.Fatal(err)
			}
			messages := []llm.Message{
				{Role: llm.RoleUser, Content: tc.content},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "收到图片"}}},
			}
			for _, message := range messages {
				if err := store.AppendMessage(sessionID, message); err != nil {
					t.Fatal(err)
				}
			}

			want := make([]llm.Message, len(messages))
			for i, message := range messages {
				want[i] = message
				want[i].Content = append([]llm.ContentBlock(nil), message.Content...)
				for j := range want[i].Content {
					if want[i].Content[j].Type == "image" {
						want[i].Content[j].AttachmentHash = imageHash
					}
				}
			}

			transcriptPath := filepath.Join(store.Dir, sessionID+".jsonl")
			canonicalBefore, err := os.ReadFile(transcriptPath)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(canonicalBefore, []byte(imageData)) || bytes.Contains(canonicalBefore, []byte(`"data":`)) {
				t.Fatal("canonical JSONL contains inline image data instead of an attachment reference")
			}
			if !bytes.Contains(canonicalBefore, []byte(`"attachment_hash":"`+imageHash+`"`)) {
				t.Fatal("canonical JSONL is missing the image attachment hash")
			}
			attachment, err := os.ReadFile(filepath.Join(transcriptPath+".attachments", imageHash))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(attachment, imageBytes) {
				t.Fatal("stored attachment differs from the original PNG")
			}

			newServer := func(store *session.Store) *Server {
				provider := &activationTestProvider{name: "test-wire", model: "test-model"}
				loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeDefault), agent.NewHookRegistry(), "system", 2)
				loop.Model = "test-model"
				return NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
					ProviderName: "test-wire", FreshPermissionMode: permission.ModeDefault,
				})
			}
			assertResponse := func(server *Server, method, path string, body []byte) {
				t.Helper()
				rr := httptest.NewRecorder()
				server.handler().ServeHTTP(rr, httptest.NewRequest(method, path, bytes.NewReader(body)))
				if rr.Code != http.StatusOK {
					t.Fatalf("%s %s = %d: %s", method, path, rr.Code, rr.Body.String())
				}
				var response struct {
					Messages []llm.Message `json:"messages"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(response.Messages, want) {
					t.Fatalf("%s %s history = %#v, want %#v", method, path, response.Messages, want)
				}
			}
			assertResponse(newServer(store), http.MethodGet, "/api/sessions/"+sessionID, nil)

			// New Store and Server instances must reconstruct the image from disk;
			// neither endpoint may depend on a still-live upload or runtime loop.
			restartedStore, err := session.NewStore(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			restartedServer := newServer(restartedStore)
			assertResponse(restartedServer, http.MethodGet, "/api/sessions/"+sessionID, nil)
			assertResponse(restartedServer, http.MethodPost, "/api/sessions/activate", []byte(`{"id":"`+sessionID+`"}`))

			_, canonical, err := restartedStore.Load(sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(canonical, want) {
				t.Fatalf("canonical history changed after presentation: %#v", canonical)
			}
			canonicalAfter, err := os.ReadFile(transcriptPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonicalAfter, canonicalBefore) {
				t.Fatal("history presentation rewrote the canonical JSONL")
			}
		})
	}
}
