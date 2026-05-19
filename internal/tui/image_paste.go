package tui

// image_paste.go — pasted-image preprocessing pipeline.
//
// When the user presses Ctrl+V on an image, keybind_main.go saves the
// raw clipboard bytes to ~/.metis/cache/ and inserts a `[Image #N]`
// placeholder. At submit time, expandPastedImagesToBlocks runs:
//
//   1. Read the cached file from disk.
//   2. If raw size > IMAGE_TARGET_RAW_SIZE (3.75 MB), or dimensions
//      exceed IMAGE_MAX_PX (1568 px on either side), resize.
//   3. Re-encode (preserving format unless JPEG fallback needed).
//   4. base64 the result, build a llm.ContentBlock{Type:"image"}.
//
// The constants mirror Anthropic's documented limits via openclaude/
// claude-code-sourcemap (`apiLimits.ts`):
//
//   API hard limit  : 5 MiB after base64
//   Target raw size : 5 MiB × 3/4 = 3.75 MiB (b64 inflates ~4/3)
//   Target max-side : 1568 px (Anthropic server-side resize boundary)
//
// Falls back gracefully: a decode failure keeps the placeholder as
// inline text (`[image: <path>]`) instead of bouncing the whole submit.
// Better the LLM sees a path it can't decode than a paste that
// silently disappears.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"regexp"
	"strings"

	xdraw "golang.org/x/image/draw"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

const (
	imageMaxBase64  = 5 * 1024 * 1024          // Anthropic hard ceiling
	imageRawTarget  = (imageMaxBase64 * 3) / 4 // 3.75 MiB raw
	imageMaxPixelSz = 1568                     // claude-code parity
	jpegFallbackQ   = 80                       // openclaude default
)

// pastedImagePattern is the placeholder our Ctrl+V handler inserts.
// Captured separately from render_util.go's pastedImageTag so
// preprocessor changes don't touch the renderer.
var pastedImagePattern = regexp.MustCompile(`\[Image #(\d+)\]`)

// expandPastedImagesToBlocks splits text on `[Image #N]` placeholders,
// loading + preprocessing each image into a llm.ContentBlock and
// emitting interleaved text + image blocks. Pure: caller's idx map is
// not mutated.
//
// Each placeholder with a successful preprocess becomes one image
// block; misses (idx absent, file unreadable, decode error) degrade
// to a `[image: <path> — <reason>]` text fragment so the LLM still
// sees something instead of silently dropping the reference.
//
// Returns the assembled blocks + a slice of error messages suitable
// for surfacing as a single info row in the chat. Empty errors slice
// means everything decoded cleanly.
func expandPastedImagesToBlocks(text string, idx map[int]string) ([]llm.ContentBlock, []string) {
	if len(idx) == 0 {
		return []llm.ContentBlock{{Type: "text", Text: text}}, nil
	}

	var blocks []llm.ContentBlock
	var errs []string
	loc := pastedImagePattern.FindAllStringSubmatchIndex(text, -1)
	if len(loc) == 0 {
		return []llm.ContentBlock{{Type: "text", Text: text}}, nil
	}

	cursor := 0
	for _, m := range loc {
		// m = [start, end, capStart, capEnd]
		start, end := m[0], m[1]
		if start > cursor {
			seg := text[cursor:start]
			if seg != "" {
				blocks = append(blocks, llm.ContentBlock{Type: "text", Text: seg})
			}
		}
		num := text[m[2]:m[3]]
		var n int
		_, parseErr := fmt.Sscanf(num, "%d", &n)
		path, ok := idx[n]
		if parseErr != nil || !ok {
			// Unknown placeholder — leave verbatim so the user sees it.
			blocks = append(blocks, llm.ContentBlock{Type: "text", Text: text[start:end]})
			cursor = end
			continue
		}

		block, err := loadAndPrepImage(path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("[Image #%d]: %v", n, err))
			blocks = append(blocks, llm.ContentBlock{
				Type: "text",
				Text: fmt.Sprintf("[image: %s — %s]", path, err.Error()),
			})
		} else {
			blocks = append(blocks, block)
		}
		cursor = end
	}
	if cursor < len(text) {
		tail := text[cursor:]
		if tail != "" {
			blocks = append(blocks, llm.ContentBlock{Type: "text", Text: tail})
		}
	}
	return blocks, errs
}

// loadAndPrepImage reads path, normalises its dimensions / size, and
// returns a base64-encoded image content block. Format detection uses
// http.DetectContentType (sniffs the first 512 bytes the same way the
// stdlib would) so files without a clean extension still get a correct
// MIME type — important because the user pastes from a clipboard cache
// where filenames are arbitrary.
func loadAndPrepImage(path string) (llm.ContentBlock, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return llm.ContentBlock{}, fmt.Errorf("read: %w", err)
	}
	if len(raw) == 0 {
		return llm.ContentBlock{}, fmt.Errorf("empty file")
	}

	mime := http.DetectContentType(raw)
	if !strings.HasPrefix(mime, "image/") {
		return llm.ContentBlock{}, fmt.Errorf("not an image (%s)", mime)
	}

	out, outMime, err := preprocessImage(raw, mime)
	if err != nil {
		return llm.ContentBlock{}, err
	}

	// P1 (2026-05-18) — final base64 size cap. preprocessImage's JPEG
	// q=80 fallback is "empirically reliable at <5 MiB" but not
	// guaranteed (a pathological 1568×1568 high-entropy PNG can
	// survive resize and still exceed the API ceiling). A clean
	// pre-flight reject beats an opaque "request too large" 422 four
	// network round-trips later. Mirrors claude-code's
	// API_IMAGE_MAX_BASE64_SIZE pre-flight check.
	encoded := base64.StdEncoding.EncodeToString(out)
	if len(encoded) > imageMaxBase64 {
		return llm.ContentBlock{}, fmt.Errorf(
			"image too large after compression (%d KiB base64 > %d KiB max) — please resize and re-paste",
			len(encoded)/1024, imageMaxBase64/1024,
		)
	}

	return llm.ContentBlock{
		Type:      "image",
		MediaType: outMime,
		Data:      encoded,
	}, nil
}

// preprocessImage decides whether the raw bytes are already small +
// well-shaped, or need to be decoded → resized → re-encoded. Returns
// the (possibly identical) bytes + canonical MIME.
//
// Cheap path: bytes ≤ target + dimensions ≤ cap → return as-is. This
// is the common case for screenshots ≤ 1.5 MiB.
//
// Expensive path: decode, resize to fit imageMaxPixelSz, encode (PNG
// stays PNG; everything else goes JPEG q=80 because the only reason
// to re-encode is size, and JPEG compresses photos ~10× better).
//
// Fallback chain: if PNG re-encode is still over budget, retry as JPEG.
func preprocessImage(raw []byte, mime string) ([]byte, string, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		// Decode failed but the file IS image/* per MIME sniff. Could be
		// a format Go's stdlib doesn't know (HEIC, AVIF). Send raw bytes
		// + sniffed MIME and hope the API accepts it; modern Anthropic
		// does HEIC.
		return raw, normaliseMime(mime), nil
	}

	smallEnough := len(raw) <= imageRawTarget &&
		cfg.Width <= imageMaxPixelSz && cfg.Height <= imageMaxPixelSz
	if smallEnough {
		return raw, normaliseMime(mime), nil
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}
	resized := resizeToFit(src, imageMaxPixelSz)

	// Try preserving format first (PNG users expect alpha; JPEG users
	// expect baseline JPEG). If the result is still too big, fall
	// through to JPEG q=80 — the universal "small enough" output.
	var buf bytes.Buffer
	switch mime {
	case "image/png":
		if err := png.Encode(&buf, resized); err == nil {
			if buf.Len() <= imageRawTarget {
				return buf.Bytes(), "image/png", nil
			}
			buf.Reset()
		}
	case "image/jpeg":
		if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: jpegFallbackQ}); err == nil {
			if buf.Len() <= imageRawTarget {
				return buf.Bytes(), "image/jpeg", nil
			}
			buf.Reset()
		}
	}
	// Last resort: JPEG q=80 of the resized image. Empirically reliable
	// at < 5 MiB for any 1568×1568 input.
	if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: jpegFallbackQ}); err != nil {
		return nil, "", fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), "image/jpeg", nil
}

// resizeToFit scales src so neither dimension exceeds maxSide,
// preserving aspect ratio. Returns src verbatim when it already fits.
// CatmullRom is the quality/speed sweet spot for downscaling photos —
// noticeably crisper than ApproxBiLinear without the cost of slow
// kernel-based filters.
func resizeToFit(src image.Image, maxSide int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxSide && h <= maxSide {
		return src
	}
	scale := float64(maxSide) / float64(w)
	if float64(maxSide)/float64(h) < scale {
		scale = float64(maxSide) / float64(h)
	}
	newW := int(float64(w) * scale)
	newH := int(float64(h) * scale)
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}

// normaliseMime fixes MIME types Anthropic doesn't accept. The big one
// is "image/jpg" (some terminals report this) → "image/jpeg".
func normaliseMime(m string) string {
	switch m {
	case "image/jpg":
		return "image/jpeg"
	}
	return m
}
