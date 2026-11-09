package runtime

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestCollectTraceImagesUsesOnlyBoundAdapterStore(t *testing.T) {
	previous := CurrentTraceAdapter()
	defer SetTraceAdapter(previous)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	blocks := []llm.ContentBlock{{Type: "image", Data: base64.StdEncoding.EncodeToString(data.Bytes()), MediaType: "image/png"}}
	store, _ := session.NewStore(t.TempDir())
	first := NewTraceAdapter(nil)
	SetTraceAdapter(first)
	SetTraceAttachmentStore(store)
	images := collectTraceImages("owner", blocks)
	if len(images) != 1 || !images[0].Available || images[0].Width != 2 {
		t.Fatalf("bound images=%+v", images)
	}
	SetTraceAdapter(NewTraceAdapter(nil))
	images = collectTraceImages("owner", blocks)
	if len(images) != 1 || images[0].Available || images[0].AttachmentHash == "" || images[0].Limitation == "" {
		t.Fatalf("new adapter leaked binding: %+v", images)
	}
	images = collectTraceImages("owner", []llm.ContentBlock{{Type: "image", AttachmentHash: images[0].AttachmentHash, MediaType: "image/png"}})
	if len(images) != 1 || images[0].AttachmentHash == "" {
		t.Fatalf("unbound hash lost: %+v", images)
	}
}
