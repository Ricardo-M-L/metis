package builtin

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/imageprep"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// viewImageMaxBase64Bytes caps the base64-encoded payload at 5 MiB.
// That matches the chat-side ceiling enforced in
// internal/tui/image_paste.go::loadAndPrepImage and the shared
// imageprep package, keeping the vision pipeline bounded on every
// ingest path (user paste, tool read, future drag-drop). All three
// constants are wired to imageprep.MaxBase64Bytes so they move in
// lock-step.
const viewImageMaxBase64Bytes = imageprep.MaxBase64Bytes

// supportedImageMime is the closed allow-list of media types every
// vision-capable provider accepts. Anthropic + OpenAI both speak
// these four; Gemini adds heic/heif which we explicitly omit until a
// provider in this repo actually consumes them — keeping the list
// short means model-side handling stays predictable.
var supportedImageMime = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/gif":  {},
	"image/webp": {},
}

type ViewImage struct {
	tools.BaseTool
	gate *permission.Gate
}

func (ViewImage) Name() string { return "ViewImage" }

func (ViewImage) ShortDescription() string {
	return "Inspect a local image path or a stable attachment_id from the current session as a native vision content block. Use attachment_id to re-read an older image after context compaction."
}

func (ViewImage) Description() string {
	return `View an image from a local file or this session's saved attachment by attaching it to the next turn's
tool_result as a vision content block. The agent loop bridges the
bytes from disk into the provider-specific image shape (Anthropic:
` + "`{type:image,source:{type:base64,...}}`" + `; OpenAI: ` + "`image_url`" + ` data URI).

When to use:
  - WebFetch saved an image to disk (` + "`~/.metis/tool-results/*.png`" + `) and
    you need to describe the contents.
  - The user mentioned an image path the chat composer didn't auto-inline.
  - You just generated an image with another tool and want to inspect it.
  - Context compaction kept an attachment_id for an older image: pass that
    attachment_id to inspect the saved visual content again, even if its
    original temporary file was deleted. The current session must own it.

When NOT to use:
  - Reading text/code → use Read.
  - The bytes aren't an image (use ` + "`file`" + ` via Bash to confirm type first).
  - The current provider doesn't support vision — the image content
    block is silently dropped for non-vision providers; you'll only
    see the textual summary. Check the model's capabilities first.

Hard requirements:
  - Supply exactly one of ` + "`path`" + ` or ` + "`attachment_id`" + `.
  - attachment_id is the full lowercase SHA-256 of decoded image bytes.
    Only current-session image references are readable; remote URLs are not.
  - ` + "`path`" + ` MUST be a regular file. Relative paths are resolved against
    the agent's cwd; the response always echoes the absolute path.
  - Supported MIME: png, jpeg, gif, webp. Other types are rejected.
  - Base64-encoded payload cap: 5 MiB. Oversize images are automatically
    decoded, resized to fit 1568×1568 and re-encoded (PNG → PNG when it
    fits, else JPEG q=80). Same pipeline the TUI paste path uses.
    Only files that still won't fit after that fallback are rejected.

The textual ` + "`Output`" + ` is a one-line summary (path, byte size,
MIME). The actual visual data rides on the ` + "`Images`" + ` attachment that
the dispatcher promotes to a multi-part tool_result body. On a
vision-incapable provider the textual summary is all the model sees —
do not assume the model can describe the image just because this tool
succeeded.`
}

func (ViewImage) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"oneOf": []any{
			map[string]any{"required": []string{"path"}},
			map[string]any{"required": []string{"attachment_id"}},
		},
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Image file path (PNG/JPEG/GIF/WebP). Absolute preferred; relative paths are resolved against the agent's cwd.",
			},
			"attachment_id": map[string]any{
				"type":        "string",
				"description": "Full lowercase SHA-256 image attachment ID referenced in this session. Re-opens the saved image after compaction or restart. Mutually exclusive with path; not a URL or another session's ID.",
			},
		},
	}
}

func (ViewImage) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}

func (v ViewImage) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	path := strFromAny(in["path"])
	if id := strFromAny(in["attachment_id"]); id != "" {
		if path != "" || !validViewImageAttachmentID(id) || !agent.HasImageAttachmentSession(ctx) {
			return tools.PermissionDeny, "invalid or unbound image attachment"
		}
		d, src := v.gate.Check(ctx, "ViewImage", "attachment_id="+id)
		return mapDecision(d), src
	}
	target := resolvePathAgainstAgentCWD(ctx, path)
	d, src := v.gate.CheckPath(ctx, "ViewImage", path, target)
	return mapDecision(d), src
}

func (ViewImage) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	rawPath, _ := in["path"].(string)
	attachmentID, _ := in["attachment_id"].(string)
	if (rawPath == "") == (attachmentID == "") {
		return nil, errors.New("exactly one of path or attachment_id is required")
	}
	var raw []byte
	var err error
	abs := "attachment_id=" + attachmentID
	if attachmentID != "" {
		if !validViewImageAttachmentID(attachmentID) {
			return nil, errors.New("attachment_id must be a full lowercase SHA-256 image ID")
		}
		raw, err = agent.ReadImageAttachment(ctx, attachmentID)
		if err != nil {
			return nil, fmt.Errorf("read session image attachment: %w", err)
		}
	} else {
		abs = resolvePathAgainstAgentCWD(ctx, rawPath)
		if !filepath.IsAbs(abs) {
			if resolved, err := filepath.Abs(abs); err == nil {
				abs = resolved
			}
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return nil, fmt.Errorf("stat: %w", statErr)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a directory, not a file", abs)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular image file", abs)
		}
		raw, err = os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s is empty", abs)
	}
	// http.DetectContentType sniffs the first 512 bytes; reliable for
	// the four image types we accept, returns "application/octet-stream"
	// for anything else (which falls through to the unsupported branch).
	mime := http.DetectContentType(raw)
	if _, ok := supportedImageMime[mime]; !ok {
		return &tools.Result{
			Output: fmt.Sprintf(
				"ViewImage: %s is not a supported image (detected MIME: %s). Allowed: png, jpeg, gif, webp.",
				abs, mime,
			),
			IsError: true,
		}, nil
	}

	// 2026-05-26 — try the cheap path first (pass raw bytes through
	// unchanged when they already fit the 5 MiB base64 cap AND the
	// decoded image is ≤ 1568px on each side, mirroring imageprep's
	// own RawTarget/MaxPixelSide check). When the cheap path would
	// trip the cap we route through imageprep.Preprocess (decode →
	// resize → re-encode → JPEG q=80 fallback), the same pipeline the
	// Ctrl+V paste flow uses. Resolves session 41040bea where a
	// 2940×1912 Retina screencapture → ViewImage → reject loop forced
	// the model into 8 rounds of `screencapture + sips -Z 1200` before
	// it could see the screen at all.
	outBytes := raw
	outMime := mime
	encoded := base64.StdEncoding.EncodeToString(outBytes)
	if len(encoded) > viewImageMaxBase64Bytes {
		processed, processedMime, err := imageprep.Preprocess(raw, mime)
		if err != nil {
			return &tools.Result{
				Output: fmt.Sprintf(
					"ViewImage: %s base64-encodes to %d KiB which exceeds the %d KiB cap, and auto-resize failed (%v). Resize manually (e.g. `sips -Z 1600 path`) and retry.",
					abs, len(encoded)/1024, viewImageMaxBase64Bytes/1024, err,
				),
				IsError: true,
			}, nil
		}
		reEncoded := base64.StdEncoding.EncodeToString(processed)
		if len(reEncoded) > viewImageMaxBase64Bytes {
			return &tools.Result{
				Output: fmt.Sprintf(
					"ViewImage: %s still exceeds the %d KiB cap after auto-resize (%d KiB base64). Resize manually (e.g. `sips -Z 1200 path`) and retry.",
					abs, viewImageMaxBase64Bytes/1024, len(reEncoded)/1024,
				),
				IsError: true,
			}, nil
		}
		outBytes = processed
		outMime = processedMime
		encoded = reEncoded
		return &tools.Result{
			Output: fmt.Sprintf(
				"ViewImage: %s (%d bytes raw → %d bytes after auto-resize to ≤1568px, %s; attachment_id=%s)",
				abs, len(raw), len(outBytes), outMime, blob.Sum(outBytes),
			),
			Images: []pubtool.ImageAttachment{{MediaType: outMime, Data: encoded}},
		}, nil
	}
	return &tools.Result{
		Output: fmt.Sprintf("ViewImage: %s (%d bytes, %s; attachment_id=%s)", abs, len(outBytes), outMime, blob.Sum(outBytes)),
		Images: []pubtool.ImageAttachment{{MediaType: outMime, Data: encoded}},
	}, nil
}

func validViewImageAttachmentID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 32 && strings.ToLower(id) == id
}
