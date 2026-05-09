package tui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTinyPNG produces a w×h solid-color PNG and writes it to dir/name.
// Tiny so encode/decode round-trips are fast and the size guards
// in preprocessImage stay deterministic.
func makeTinyPNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExpandPastedImagesToBlocks_BasicSplit(t *testing.T) {
	dir := t.TempDir()
	p1 := makeTinyPNG(t, dir, "a.png", 10, 10)
	p2 := makeTinyPNG(t, dir, "b.png", 10, 10)

	text := "hello [Image #1] world [Image #2] end"
	idx := map[int]string{1: p1, 2: p2}

	blocks, errs := expandPastedImagesToBlocks(text, idx)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// Expected: 5 blocks (text, image, text, image, text)
	if len(blocks) != 5 {
		t.Fatalf("want 5 blocks, got %d: %+v", len(blocks), blocks)
	}
	wantTypes := []string{"text", "image", "text", "image", "text"}
	for i, want := range wantTypes {
		if blocks[i].Type != want {
			t.Errorf("blocks[%d].Type = %q, want %q", i, blocks[i].Type, want)
		}
	}
	if blocks[0].Text != "hello " {
		t.Errorf("blocks[0].Text = %q", blocks[0].Text)
	}
	if blocks[1].MediaType != "image/png" {
		t.Errorf("blocks[1].MediaType = %q", blocks[1].MediaType)
	}
	if blocks[1].Data == "" {
		t.Error("blocks[1].Data should be base64-encoded bytes")
	}
}

func TestExpandPastedImagesToBlocks_NoImages(t *testing.T) {
	blocks, errs := expandPastedImagesToBlocks("plain text", nil)
	if len(errs) != 0 || len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("plain path should yield single text block; got %+v", blocks)
	}
}

func TestExpandPastedImagesToBlocks_UnknownPlaceholderKeptVerbatim(t *testing.T) {
	// idx maps 1, but text references 7 — the orphan should land in
	// the text stream verbatim so the user sees what's missing.
	// Splitting into multiple text segments is fine; the LLM
	// concatenates adjacent text blocks anyway.
	dir := t.TempDir()
	p1 := makeTinyPNG(t, dir, "a.png", 10, 10)
	blocks, _ := expandPastedImagesToBlocks("see [Image #7] now", map[int]string{1: p1})

	// No image blocks should leak through.
	for i, b := range blocks {
		if b.Type == "image" {
			t.Errorf("blocks[%d] is image block; expected only text", i)
		}
	}
	// Concatenated text must preserve the orphan placeholder.
	var combined strings.Builder
	for _, b := range blocks {
		combined.WriteString(b.Text)
	}
	if !strings.Contains(combined.String(), "[Image #7]") {
		t.Errorf("orphan placeholder should survive in text: %q", combined.String())
	}
}

func TestLoadAndPrepImage_TinyPassthrough(t *testing.T) {
	dir := t.TempDir()
	p := makeTinyPNG(t, dir, "tiny.png", 32, 32)
	block, err := loadAndPrepImage(p)
	if err != nil {
		t.Fatalf("loadAndPrepImage: %v", err)
	}
	if block.Type != "image" {
		t.Errorf("block.Type = %q", block.Type)
	}
	if block.MediaType != "image/png" {
		t.Errorf("MediaType = %q", block.MediaType)
	}
	if block.Data == "" {
		t.Error("Data should be non-empty base64")
	}
}

func TestLoadAndPrepImage_NotImageRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fake.png")
	os.WriteFile(p, []byte("definitely not an image"), 0o644)
	_, err := loadAndPrepImage(p)
	if err == nil {
		t.Fatal("non-image bytes should reject")
	}
}

func TestLoadAndPrepImage_EmptyFileRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.png")
	os.WriteFile(p, nil, 0o644)
	_, err := loadAndPrepImage(p)
	if err == nil {
		t.Fatal("empty file should reject — Anthropic returns 'image cannot be empty'")
	}
}

func TestPreprocessImage_OversizedDimensionsResize(t *testing.T) {
	// Build a 3000×3000 PNG — well over imageMaxPixelSz (1568).
	img := image.NewRGBA(image.Rect(0, 0, 3000, 3000))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out, mime, err := preprocessImage(buf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("preprocessImage: %v", err)
	}
	// Verify the output decodes and has dimensions ≤ cap.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output decode: %v", err)
	}
	if cfg.Width > imageMaxPixelSz || cfg.Height > imageMaxPixelSz {
		t.Errorf("post-resize dims %dx%d exceed cap %d", cfg.Width, cfg.Height, imageMaxPixelSz)
	}
	if mime != "image/png" && mime != "image/jpeg" {
		t.Errorf("output mime should stay image/* — got %q", mime)
	}
}

func TestPreprocessImage_TinyPassthrough(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	var buf bytes.Buffer
	png.Encode(&buf, img)
	rawIn := buf.Bytes()

	out, mime, err := preprocessImage(rawIn, "image/png")
	if err != nil {
		t.Fatalf("preprocessImage: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("MIME should remain image/png; got %q", mime)
	}
	// Tiny path returns the input slice unmodified — same length.
	if len(out) != len(rawIn) {
		t.Errorf("tiny image should be passthrough; raw=%d out=%d", len(rawIn), len(out))
	}
}

func TestNormaliseMime(t *testing.T) {
	cases := map[string]string{
		"image/jpg":  "image/jpeg",
		"image/png":  "image/png",
		"image/jpeg": "image/jpeg",
	}
	for in, want := range cases {
		if got := normaliseMime(in); got != want {
			t.Errorf("normaliseMime(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResizeToFit_PreservesAspectRatio(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4000, 2000)) // 2:1
	out := resizeToFit(src, 1568)
	b := out.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > 1568 || h > 1568 {
		t.Errorf("output dims %dx%d exceed cap", w, h)
	}
	// 2:1 ratio means width=1568 maps to height=784. Allow ±2px for rounding.
	if w != 1568 {
		t.Errorf("wider side should saturate at 1568; got w=%d", w)
	}
	if h < 780 || h > 790 {
		t.Errorf("aspect ratio drift: w=1568 h=%d (want ~784)", h)
	}
}

func TestResizeToFit_AlreadyFitsPassthrough(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 200))
	out := resizeToFit(src, 1568)
	if out != src {
		t.Error("resize should pass-through when source already fits")
	}
}
