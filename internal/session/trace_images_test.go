package session

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func traceTestPNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestTraceImagesPersistStableMetadataWithoutPayload(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	data := traceTestPNG(t)
	encoded := base64.StdEncoding.EncodeToString(data)
	blocks := []llm.ContentBlock{{Type: "image", MediaType: "image/png", Data: encoded}}
	images, err := store.PersistTraceImages("owner", blocks)
	if err != nil || len(images) != 1 {
		t.Fatalf("images=%+v err=%v", images, err)
	}
	img := images[0]
	if !img.Available || img.AttachmentHash == "" || img.Bytes != len(data) || img.Width != 3 || img.Height != 2 || img.MediaType != "image/png" || img.Name == "" {
		t.Fatalf("metadata=%+v", img)
	}
	if blocks[0].AttachmentHash != "" || blocks[0].Data != encoded {
		t.Fatal("mutated submitted input")
	}
	raw, _ := json.Marshal(img)
	if bytes.Contains(raw, []byte(encoded)) {
		t.Fatal("trace image included payload")
	}
	read, err := store.ReadTraceImage("owner", img.AttachmentHash)
	if err != nil || !bytes.Equal(read, data) {
		t.Fatalf("read=%v err=%v", read, err)
	}
	if _, err := store.ReadTraceImage("other", img.AttachmentHash); err == nil {
		t.Fatal("read another session's image")
	}
	if _, err := store.ReadTraceImage("../owner", img.AttachmentHash); err == nil {
		t.Fatal("accepted aliased session path")
	}
	if _, err := store.ReadTraceImage("owner", "../image"); err == nil {
		t.Fatal("accepted path as hash")
	}
}

func TestTraceImagesMissingAndUnsupportedAreExplicit(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	images, err := store.PersistTraceImages("owner", []llm.ContentBlock{{Type: "image", MediaType: "image/png", AttachmentHash: strings.Repeat("a", 64)}})
	if err == nil || len(images) != 1 || images[0].Available || images[0].Limitation == "" {
		t.Fatalf("missing image=%+v err=%v", images, err)
	}
	images, err = store.PersistTraceImages("owner", []llm.ContentBlock{{Type: "image", MediaType: "image/svg+xml", Data: base64.StdEncoding.EncodeToString([]byte(`<svg onload="alert(1)"></svg>`))}})
	if err == nil || len(images) != 1 || images[0].Available {
		t.Fatalf("active format=%+v err=%v", images, err)
	}
}
