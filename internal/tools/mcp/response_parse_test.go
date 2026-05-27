package mcp_tools

import (
	"strings"
	"testing"
)

// TestParseMCPResponse_TextOnly — most common MCP shape: one or
// more text parts and zero images. Must end up in tools.Result
// with empty Images slice (not nil-vs-not-nil-distinguishing
// callers downstream; the dispatch path just reads len).
func TestParseMCPResponse_TextOnly(t *testing.T) {
	raw := []byte(`{"content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}],"isError":false}`)
	res, ok := parseMCPResponse(raw)
	if !ok {
		t.Fatal("envelope wasn't recognised")
	}
	if res.Output != "hello\n\nworld" {
		t.Errorf("Output = %q, want %q", res.Output, "hello\n\nworld")
	}
	if len(res.Images) != 0 {
		t.Errorf("text-only response got Images=%d", len(res.Images))
	}
	if res.IsError {
		t.Error("non-error envelope marked IsError")
	}
}

// TestParseMCPResponse_ImageGetsExtracted — the regression that
// motivated this whole parse function (2026-05-27 session
// 87e366f): cu screenshot returns a {text, image} envelope and
// PreviouslyMCPTool.Execute used to dump the whole JSON as the
// Output string. Now the image data lives in tools.Result.Images
// where the dispatch layer can fan it out as an image
// ContentBlock the image-pruner can actually see.
func TestParseMCPResponse_ImageGetsExtracted(t *testing.T) {
	raw := []byte(`{"content":[
		{"type":"text","text":"captured 1280x800 JPEG q=85 (94137 bytes)"},
		{"type":"image","data":"BASE64DATA","mimeType":"image/jpeg"}
	],"isError":false}`)
	res, ok := parseMCPResponse(raw)
	if !ok {
		t.Fatal("envelope wasn't recognised")
	}
	if len(res.Images) != 1 {
		t.Fatalf("Images count = %d, want 1", len(res.Images))
	}
	if res.Images[0].MediaType != "image/jpeg" {
		t.Errorf("MediaType = %q, want image/jpeg", res.Images[0].MediaType)
	}
	if res.Images[0].Data != "BASE64DATA" {
		t.Errorf("Data lost; got %q", res.Images[0].Data)
	}
	// Text part should be the human-readable summary, NOT the
	// base64 blob.
	if !strings.Contains(res.Output, "1280x800") {
		t.Errorf("Output missing text summary; got %q", res.Output)
	}
	if strings.Contains(res.Output, "BASE64DATA") {
		t.Errorf("base64 leaked into Output; got %q", res.Output)
	}
}

// TestParseMCPResponse_DefaultsMimeType — MCP spec lets servers
// omit mimeType for image parts (it defaults to image/png). The
// parser must back-fill so the dispatch layer doesn't ship an
// image block with an empty MediaType (Anthropic rejects those).
func TestParseMCPResponse_DefaultsMimeType(t *testing.T) {
	raw := []byte(`{"content":[{"type":"image","data":"X"}]}`)
	res, ok := parseMCPResponse(raw)
	if !ok || len(res.Images) != 1 {
		t.Fatalf("parse failed: ok=%v images=%d", ok, len(res.Images))
	}
	if res.Images[0].MediaType != "image/png" {
		t.Errorf("missing mimeType should default to image/png; got %q", res.Images[0].MediaType)
	}
}

// TestParseMCPResponse_IsErrorPropagates — error envelopes (tier
// gate rejections, invalid params, broken pipe) carry the
// failure flag through so the dispatch layer marks the
// tool_result with is_error=true. Without this every cu
// `tier denied` would look like a successful call returning
// the error message as data.
func TestParseMCPResponse_IsErrorPropagates(t *testing.T) {
	raw := []byte(`{"content":[{"type":"text","text":"tier \"click\" on iTerm2 does not permit \"full\" operations"}],"isError":true}`)
	res, ok := parseMCPResponse(raw)
	if !ok {
		t.Fatal("parse failed")
	}
	if !res.IsError {
		t.Error("IsError not propagated")
	}
	if !strings.Contains(res.Output, "tier") {
		t.Errorf("error text lost; got %q", res.Output)
	}
}

// TestParseMCPResponse_NonEnvelopeRejected — legacy servers /
// debug dumps that don't wrap in {"content":[…]} must return
// (nil, false) so the caller falls through to the raw pretty-
// print path. Without this we'd silently lose data from
// non-conformant MCP servers.
func TestParseMCPResponse_NonEnvelopeRejected(t *testing.T) {
	for _, raw := range []string{
		`"just a bare string"`,
		`{"hello":"world"}`,                       // no content key
		`[{"type":"text","text":"naked array"}]`,  // top-level array
	} {
		if _, ok := parseMCPResponse([]byte(raw)); ok {
			t.Errorf("non-envelope %q was incorrectly accepted", raw)
		}
	}
}

// TestParseMCPResponse_EmptyImageDataSkipped — an envelope with
// {"type":"image","data":""} would otherwise produce a zero-byte
// image attachment that Anthropic rejects with 400. Defensive:
// drop those parts silently.
func TestParseMCPResponse_EmptyImageDataSkipped(t *testing.T) {
	raw := []byte(`{"content":[{"type":"text","text":"ok"},{"type":"image","data":""}]}`)
	res, ok := parseMCPResponse(raw)
	if !ok {
		t.Fatal("parse failed")
	}
	if len(res.Images) != 0 {
		t.Errorf("empty-data image attached; got %d images", len(res.Images))
	}
	if res.Output != "ok" {
		t.Errorf("Output = %q, want %q", res.Output, "ok")
	}
}
