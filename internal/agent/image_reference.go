package agent

import (
	"encoding/base64"
	"encoding/hex"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
)

// canonicalImageAttachmentID identifies the decoded image bytes, never their
// base64 transport representation. Existing metadata cannot override live bytes.
func canonicalImageAttachmentID(block llm.ContentBlock) string {
	if block.Data != "" {
		if len(block.Data) > base64.StdEncoding.EncodedLen(blob.MaxBytes) {
			return ""
		}
		data, err := base64.StdEncoding.DecodeString(block.Data)
		if err != nil {
			return ""
		}
		return blob.Sum(data)
	}
	hash := block.AttachmentHash
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != 32 || strings.ToLower(hash) != hash {
		return ""
	}
	return hash
}

// canonicalImageReferences preserves visual content while giving user images
// and nested tool screenshots the same stable identity used by session blobs.
// It owns the copied content slices so callers' messages remain unchanged.
func canonicalImageReferences(messages []llm.Message) []llm.Message {
	if messages == nil {
		return nil
	}
	out := append([]llm.Message(nil), messages...)
	for i := range out {
		out[i].Content = canonicalImageReferenceBlocks(out[i].Content)
	}
	return out
}

func canonicalImageReferenceBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := append([]llm.ContentBlock(nil), blocks...)
	for i := range out {
		if out[i].Type == "image" {
			out[i].AttachmentHash = canonicalImageAttachmentID(out[i])
		}
		out[i].ToolResultBlocks = canonicalImageReferenceBlocks(out[i].ToolResultBlocks)
	}
	return out
}
