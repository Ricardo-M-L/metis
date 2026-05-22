package builtin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// viewImageMaxBase64Bytes caps the base64-encoded payload at 5 MiB.
// That matches the chat-side ceiling enforced in
// internal/tui/image_paste.go::loadAndPrepImage, keeping the vision
// pipeline bounded on both ingest paths (user paste vs. tool read).
// Raise both constants in lock-step if a future provider supports
// larger inline images and Anthropic raises its 5 MiB cap.
const viewImageMaxBase64Bytes = 5 * 1024 * 1024

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
	return "Open an image file from disk and deliver it to the model as a vision content block. Use after WebFetch saves a binary, or when the user @-references a local image and you need to actually SEE the contents."
}

func (ViewImage) Description() string {
	return `View an image file from disk by attaching it to the next turn's
tool_result as a vision content block. The agent loop bridges the
bytes from disk into the provider-specific image shape (Anthropic:
` + "`{type:image,source:{type:base64,...}}`" + `; OpenAI: ` + "`image_url`" + ` data URI).

When to use:
  - WebFetch saved an image to disk (` + "`~/.metis/tool-results/*.png`" + `) and
    you need to describe the contents.
  - The user mentioned an image path the chat composer didn't auto-inline.
  - You just generated an image with another tool and want to inspect it.

When NOT to use:
  - Reading text/code → use Read.
  - The bytes aren't an image (use ` + "`file`" + ` via Bash to confirm type first).
  - The current provider doesn't support vision — the image content
    block is silently dropped for non-vision providers; you'll only
    see the textual summary. Check the model's capabilities first.

Hard requirements:
  - ` + "`path`" + ` MUST be a regular file. Relative paths are resolved against
    the agent's cwd; the response always echoes the absolute path.
  - Supported MIME: png, jpeg, gif, webp. Other types are rejected.
  - Base64-encoded payload cap: 5 MiB. Larger images are rejected
    with a hint to resize; there's no automatic downsampling on this
    path (unlike the TUI paste pipeline which compresses).

The textual ` + "`Output`" + ` is a one-line summary (path, byte size,
MIME). The actual visual data rides on the ` + "`Images`" + ` attachment that
the dispatcher promotes to a multi-part tool_result body. On a
vision-incapable provider the textual summary is all the model sees —
do not assume the model can describe the image just because this tool
succeeded.`
}

func (ViewImage) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"path"},
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Image file path (PNG/JPEG/GIF/WebP). Absolute preferred; relative paths are resolved against the agent's cwd.",
			},
		},
	}
}

func (ViewImage) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}

func (v ViewImage) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := v.gate.Check(context.Background(), "ViewImage", strFromAny(in["path"]))
	return mapDecision(d), src
}

func (ViewImage) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	raw, _ := in["path"].(string)
	if raw == "" {
		return nil, errors.New("path required")
	}
	abs := raw
	if !filepath.IsAbs(abs) {
		if resolved, err := filepath.Abs(abs); err == nil {
			abs = resolved
		}
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a file", abs)
	}
	bytes, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(bytes) == 0 {
		return nil, fmt.Errorf("%s is empty", abs)
	}
	// http.DetectContentType sniffs the first 512 bytes; reliable for
	// the four image types we accept, returns "application/octet-stream"
	// for anything else (which falls through to the unsupported branch).
	mime := http.DetectContentType(bytes)
	if _, ok := supportedImageMime[mime]; !ok {
		return &tools.Result{
			Output: fmt.Sprintf(
				"ViewImage: %s is not a supported image (detected MIME: %s). Allowed: png, jpeg, gif, webp.",
				abs, mime,
			),
			IsError: true,
		}, nil
	}
	encoded := base64.StdEncoding.EncodeToString(bytes)
	if len(encoded) > viewImageMaxBase64Bytes {
		return &tools.Result{
			Output: fmt.Sprintf(
				"ViewImage: %s base64-encodes to %d KiB which exceeds the %d KiB cap. Resize the image (e.g. `sips -Z 1600 path`) and retry.",
				abs, len(encoded)/1024, viewImageMaxBase64Bytes/1024,
			),
			IsError: true,
		}, nil
	}
	return &tools.Result{
		Output: fmt.Sprintf("ViewImage: %s (%d bytes, %s)", abs, len(bytes), mime),
		Images: []pubtool.ImageAttachment{{MediaType: mime, Data: encoded}},
	}, nil
}
