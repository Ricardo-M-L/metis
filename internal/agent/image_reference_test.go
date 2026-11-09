package agent

import (
	"encoding/base64"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
)

func TestActiveImageReferencesUseCanonicalDecodedID(t *testing.T) {
	data := []byte("the same image bytes")
	encoded := base64.StdEncoding.EncodeToString(data)
	want := blob.Sum(data)
	for _, nested := range []bool{false, true} {
		block := llm.ContentBlock{Type: "image", Data: encoded, AttachmentHash: "stale", MediaType: "image/png"}
		if nested {
			block = llm.ContentBlock{Type: "tool_result", ToolUseID: "screenshot", ToolResultBlocks: []llm.ContentBlock{block}}
		}
		input := []llm.ContentBlock{block}
		loop := NewLoop(nil, nil, nil, nil, "", 1)
		loop.AppendUserBlocks(input)
		got := loop.History()[0].Content[0]
		if nested {
			got = got.ToolResultBlocks[0]
		}
		if got.AttachmentHash != want || got.Data != encoded {
			t.Fatalf("nested=%v: active image = %+v, want ID %s and original bytes", nested, got, want)
		}
		original := input[0]
		if nested {
			original = original.ToolResultBlocks[0]
		}
		if original.AttachmentHash != "stale" {
			t.Fatal("caller-owned blocks were mutated")
		}
		if nested {
			loop.Reset()
			if err := loop.commitToolBatch(input); err != nil {
				t.Fatal(err)
			}
			if got := loop.History()[0].Content[0].ToolResultBlocks[0].AttachmentHash; got != want {
				t.Fatalf("committed tool image ID = %q, want %q", got, want)
			}
		}
		loop.Restore([]llm.Message{{Role: llm.RoleUser, Content: input}})
		got = loop.History()[0].Content[0]
		if nested {
			got = got.ToolResultBlocks[0]
		}
		if got.AttachmentHash != want {
			t.Fatalf("restored image ID = %q, want %q", got.AttachmentHash, want)
		}
	}
}
