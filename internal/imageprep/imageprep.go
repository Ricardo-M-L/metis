// Package imageprep is the shared "make an image fit Anthropic's vision
// content block limits" pipeline. Originally lived inside
// internal/tui/image_paste.go for the Ctrl+V paste flow; lifted to its
// own package on 2026-05-26 so the ViewImage builtin tool can reuse the
// exact same decode→resize→re-encode→fallback ladder instead of failing
// hard on big Retina screenshots produced by `screencapture` (which
// bypasses the cu MCP's built-in 1280×800 cap).
//
// The constants mirror Anthropic's documented limits via openclaude /
// claude-code-sourcemap (`apiLimits.ts`):
//
//	API hard limit  : 5 MiB after base64
//	Target raw size : 5 MiB × 3/4 = 3.75 MiB (b64 inflates ~4/3)
//	Target max-side : 1568 px (Anthropic server-side resize boundary)
package imageprep

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
)

const (
	// MaxBase64Bytes is the Anthropic hard ceiling after base64 encoding.
	// Both ViewImage (file → tool_result) and the Ctrl+V paste pipeline
	// pre-flight against this so a 5+ MiB input fails fast instead of
	// getting a 422 from the provider four network round-trips later.
	MaxBase64Bytes = 5 * 1024 * 1024

	// RawTarget is the raw-bytes target (base64 inflates by ~4/3, so a
	// 3.75 MiB raw payload safely fits the 5 MiB base64 ceiling). The
	// resize path stops shrinking once it falls under this number.
	RawTarget = (MaxBase64Bytes * 3) / 4

	// MaxPixelSide is the per-side dimension cap before resize triggers.
	// 1568 mirrors Anthropic's own server-side downscale boundary —
	// images that come in larger get resized by the server anyway, so
	// doing it client-side first saves bandwidth + matches what the
	// model actually sees.
	MaxPixelSide = 1568

	// JPEGFallbackQuality is the JPEG quality used when PNG re-encode
	// can't get under RawTarget (photos with high entropy). q=80
	// matches the openclaude default; visually indistinguishable from
	// q=90+ on typical UI screenshots while ~5x smaller.
	JPEGFallbackQuality = 80
)

// Preprocess decides whether the raw bytes are already small +
// well-shaped, or need to be decoded → resized → re-encoded. Returns
// the (possibly identical) bytes + canonical MIME.
//
// Cheap path: bytes ≤ RawTarget + dimensions ≤ MaxPixelSide → return
// as-is. This is the common case for screenshots ≤ ~3.75 MiB that the
// user pastes directly.
//
// Expensive path: decode, resize to fit MaxPixelSide, encode (PNG
// stays PNG; everything else goes JPEG q=JPEGFallbackQuality because
// the only reason to re-encode is size, and JPEG compresses photos
// ~10× better).
//
// Fallback chain: if PNG re-encode is still over budget, retry as
// JPEG q=JPEGFallbackQuality. Empirically reliable at < 5 MiB for any
// 1568×1568 input.
//
// If decode fails entirely (HEIC, AVIF, format Go's stdlib doesn't
// know), the raw bytes + normalised MIME are returned unchanged on the
// theory that modern provider APIs may still accept them. Callers can
// still pre-flight the returned byte length against MaxBase64Bytes.
func Preprocess(raw []byte, mime string) ([]byte, string, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return raw, NormaliseMime(mime), nil
	}

	smallEnough := len(raw) <= RawTarget &&
		cfg.Width <= MaxPixelSide && cfg.Height <= MaxPixelSide
	if smallEnough {
		return raw, NormaliseMime(mime), nil
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}
	resized := ResizeToFit(src, MaxPixelSide)

	var buf bytes.Buffer
	switch mime {
	case "image/png":
		if err := png.Encode(&buf, resized); err == nil {
			if buf.Len() <= RawTarget {
				return buf.Bytes(), "image/png", nil
			}
			buf.Reset()
		}
	case "image/jpeg":
		if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: JPEGFallbackQuality}); err == nil {
			if buf.Len() <= RawTarget {
				return buf.Bytes(), "image/jpeg", nil
			}
			buf.Reset()
		}
	}
	if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: JPEGFallbackQuality}); err != nil {
		return nil, "", fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), "image/jpeg", nil
}

// ResizeToFit scales src so neither dimension exceeds maxSide,
// preserving aspect ratio. Returns src verbatim when it already fits.
// CatmullRom is the quality/speed sweet spot for downscaling photos —
// noticeably crisper than ApproxBiLinear without the cost of slow
// kernel-based filters.
func ResizeToFit(src image.Image, maxSide int) image.Image {
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

// NormaliseMime fixes MIME types Anthropic doesn't accept. The big one
// is "image/jpg" (some terminals report this) → "image/jpeg".
func NormaliseMime(m string) string {
	switch m {
	case "image/jpg":
		return "image/jpeg"
	}
	return m
}
