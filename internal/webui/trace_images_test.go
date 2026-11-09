package webui

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func traceImageFixture(t *testing.T) (*Server, []byte, session.TraceImage) {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 4, 2))); err != nil {
		t.Fatal(err)
	}
	store, _ := session.NewStore(t.TempDir())
	if err := store.WriteHeader("owner", "test", ""); err != nil {
		t.Fatal(err)
	}
	images, err := store.PersistTraceImages("owner", []llm.ContentBlock{{Type: "image", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(data.Bytes())}})
	if err != nil {
		t.Fatal(err)
	}
	traces, _ := session.NewTraceStore(t.TempDir())
	t.Cleanup(func() { _ = traces.Close() })
	if err := traces.Append(&session.TraceEvent{SessionID: "owner", Kind: "user", Images: images}); err != nil {
		t.Fatal(err)
	}
	return &Server{store: store, traceStore: traces}, data.Bytes(), images[0]
}

func TestTraceImagesPreviewRequiresSessionReference(t *testing.T) {
	s, data, img := traceImageFixture(t)
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/api/trace/image?session_id=owner&hash=" + img.AttachmentHash, http.StatusOK},
		{"/api/trace/image?session_id=other&hash=" + img.AttachmentHash, http.StatusNotFound},
		{"/api/trace/image?session_id=..%2Fowner&hash=" + img.AttachmentHash, http.StatusBadRequest},
		{"/api/trace/image?session_id=owner&hash=..%2Fsecret", http.StatusBadRequest},
	} {
		rr := httptest.NewRecorder()
		s.handleTraceImage(rr, httptest.NewRequest("GET", tc.path, nil))
		if rr.Code != tc.status {
			t.Fatalf("path=%s status=%d body=%s", tc.path, rr.Code, rr.Body.String())
		}
		if tc.status == http.StatusOK && (!bytes.Equal(rr.Body.Bytes(), data) || rr.Header().Get("Content-Type") != "image/png" || rr.Header().Get("X-Content-Type-Options") != "nosniff") {
			t.Fatalf("preview headers=%v body=%v", rr.Header(), rr.Body.Bytes())
		}
	}
	// Even a blob copied into the other session is not a reference grant.
	if err := os.MkdirAll(filepath.Join(s.store.Dir, "other.jsonl.attachments"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.store.Dir, "other.jsonl.attachments", img.AttachmentHash), data, 0600); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.handleTraceImage(rr, httptest.NewRequest("GET", "/api/trace/image?session_id=other&hash="+img.AttachmentHash, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("orphan blob was exposed: %d", rr.Code)
	}
}

func TestTraceImagesViewDetectsMissingAttachment(t *testing.T) {
	s, _, img := traceImageFixture(t)
	view := traceImagesForView(s, "owner", []session.TraceImage{img})
	if len(view) != 1 || !view[0].Available || !strings.Contains(view[0].URL, "session_id=owner") || view[0].Width != 4 {
		t.Fatalf("view=%+v", view)
	}
	if err := os.Remove(filepath.Join(s.store.Dir, "owner.jsonl.attachments", img.AttachmentHash)); err != nil {
		t.Fatal(err)
	}
	view = traceImagesForView(s, "owner", []session.TraceImage{img})
	if view[0].Available || view[0].URL != "" || view[0].Limitation == "" {
		t.Fatalf("missing view=%+v", view)
	}
}

func TestTraceImageHistoryReferenceSurvivesRestart(t *testing.T) {
	s, data, img := traceImageFixture(t)
	if err := s.store.AppendMessage("owner", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "image", MediaType: "image/png", AttachmentHash: img.AttachmentHash}}}); err != nil {
		t.Fatal(err)
	}
	traces, _ := session.NewTraceStore(t.TempDir())
	defer traces.Close()
	s.traceStore = traces // prove history-only image authorization
	_, messages, err := s.store.Load("owner")
	if err != nil {
		t.Fatal(err)
	}
	images := historyTraceImages(messages[0].Content)
	if len(images) != 1 || !images[0].Available || images[0].Bytes != len(data) {
		t.Fatalf("history images=%+v", images)
	}
	rr := httptest.NewRecorder()
	s.handleTraceImage(rr, httptest.NewRequest("GET", "/api/trace/image?session_id=owner&hash="+img.AttachmentHash, nil))
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), data) {
		t.Fatalf("history-only preview=%d %s", rr.Code, rr.Body.String())
	}
}
