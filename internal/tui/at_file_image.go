package tui

// at_file_image.go — `@path/to/image.png` text-reference expansion.
//
// Mirrors claude-code's tryReadImageFromPath: the user types
// `@./screenshots/bug.png look at this` and the TUI loads + base64-
// encodes the file into an image content block before AppendUserBlocks.
// Without this path the model would just see literal "@./screenshots/
// bug.png" as text and have no way to view the image even when its
// provider supports vision.
//
// Relative paths resolve against cwd. Absolute paths are kept as-is.
// Unrecognized extensions are NOT matched (no false positives on the
// many `@username` / `@-author` patterns models emit).
//
// Failure modes degrade to a warning + leave the @path verbatim so the
// user sees what didn't work instead of a silent drop.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// atFileImagePattern matches `@<path>.<ext>` where ext is in the
// supported image set. The path can contain `/`, `.`, `_`, `-`,
// digits and letters. We stop at whitespace because that's the
// natural word boundary in conversational text.
//
// Examples that match:
//   @screenshot.png
//   @./relative/path/to/img.jpg
//   @/abs/path/to/photo.webp
//
// Examples that DON'T match:
//   @username       — no recognized image extension
//   @file.txt       — wrong extension
//   email@host.com  — leading char isn't whitespace/start (handled via lookbehind below)
var atFileImagePattern = regexp.MustCompile(`(?:^|\s)@(\S+?\.(?:png|jpe?g|gif|webp|bmp))`)

// expandAtFileImageBlocks scans text for `@path.png`-style image
// references, strips successful ones from the text, and returns the
// rewritten text + a list of image content blocks + any per-reference
// errors as user-friendly strings.
//
// Pure: no filesystem mutations. cwd is used only for resolving
// relative paths. A nil/empty cwd treats every relative path as if
// rooted at "." which is the process's getwd — same convention the
// rest of the TUI uses.
func expandAtFileImageBlocks(text, cwd string) (string, []llm.ContentBlock, []string) {
	matches := atFileImagePattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil, nil
	}

	var (
		out    strings.Builder
		blocks []llm.ContentBlock
		errs   []string
	)
	cursor := 0
	for _, m := range matches {
		// FindAllStringSubmatchIndex returns the full match indices in
		// m[0:2] and the first submatch (the path) in m[2:4].
		start, end := m[0], m[1]
		rawPath := text[m[2]:m[3]]
		// Skip leading whitespace in the match — we want to keep it in
		// the output for non-stripped paths.
		matchStart := start
		if matchStart < len(text) && (text[matchStart] == ' ' || text[matchStart] == '\t' || text[matchStart] == '\n') {
			matchStart++
		}

		path := rawPath
		if !filepath.IsAbs(path) {
			if cwd != "" {
				path = filepath.Join(cwd, path)
			}
		}

		block, err := loadAndPrepImage(path)
		if err != nil {
			// Failure path: emit everything up to AND including the @ref
			// verbatim, plus a warning. The model still sees what the
			// user wrote, just without the inflated image data.
			out.WriteString(text[cursor:end])
			errs = append(errs, fmt.Sprintf("@%s: %v", rawPath, err))
			cursor = end
			continue
		}

		// Success path: emit text up to (not including) the @ref, drop
		// the @ref from the text, and accumulate the image block
		// separately. The block is appended to the final user message
		// alongside the rewritten text — image-after-text matches the
		// Anthropic convention for "here's a photo of X" prompts.
		out.WriteString(text[cursor:matchStart])
		blocks = append(blocks, block)
		cursor = end
	}
	out.WriteString(text[cursor:])
	return out.String(), blocks, errs
}
