package builtin

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/security"
)

// A native source page must remain comfortably below the default 8000-token
// tool-result budget, even under the conservative multilingual estimator.
// Unlike an arbitrary line count this also bounds one minified/JSON line.
const readPageBytes = 8192

func readLineRange(data []byte, offset, limit int) (int, int) {
	start, line := 0, 1
	for line < offset && start < len(data) {
		i := bytes.IndexByte(data[start:], '\n')
		if i < 0 {
			return len(data), len(data)
		}
		start += i + 1
		line++
	}
	end := start
	for n := 0; n < limit && end < len(data); n++ {
		i := bytes.IndexByte(data[end:], '\n')
		if i < 0 {
			end = len(data)
			break
		}
		end += i + 1
	}
	return start, end
}

func readRuneStart(data []byte, at int) int {
	for at > 0 && at < len(data) && !utf8.RuneStart(data[at]) {
		at--
	}
	return at
}

func renderReadPage(ctx context.Context, path string, data []byte, offset, limit, byteOffset, byteLimit int, byteMode bool, redactor security.FileCredentialRedactor) (string, int, int, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, 0, false, err
	}
	start, end := readLineRange(data, offset, limit)
	if byteMode {
		start = min(byteOffset, len(data))
		start = readRuneStart(data, start)
		end = len(data)
	} else {
		// Retain the familiar numbered-line format when the complete page
		// fits. Stop building before any single huge line is copied.
		var out strings.Builder
		cursor, line := start, offset
		for cursor < end {
			if err := ctx.Err(); err != nil {
				return "", 0, 0, false, err
			}
			next := end
			if i := bytes.IndexByte(data[cursor:end], '\n'); i >= 0 {
				next = cursor + i + 1
			}
			lineEnd := next
			if lineEnd > cursor && data[lineEnd-1] == '\n' {
				lineEnd--
			}
			if lineEnd > cursor && data[lineEnd-1] == '\r' {
				lineEnd--
			}
			if lineEnd-cursor > readPageBytes {
				byteMode = true
				break
			}
			rendered := fmt.Sprintf("%6d\t%s\n", line, redactor.RedactLineAt(cursor, string(data[cursor:lineEnd])))
			if out.Len()+len(rendered) > readPageBytes {
				byteMode = true
				break
			}
			out.WriteString(rendered)
			cursor, line = next, line+1
		}
		if !byteMode {
			return out.String(), start, end, false, nil
		}
	}
	redactor = security.NewBytePageCredentialRedactor(data)
	pageEnd := min(start+byteLimit, end)
	pageEnd = readRuneStart(data, pageEnd)
	if pageEnd == start && start < end {
		_, size := utf8.DecodeRune(data[start:end])
		pageEnd = min(start+max(size, 1), end)
	}
	renderBody := func(end int) string {
		return redactor.RedactLineAt(start, string(data[start:end]))
	}
	body := renderBody(pageEnd)
	// Redaction markers may be longer than their source values. Fit the
	// source boundary before reporting its next offset; trimming the rendered
	// string would claim unseen public bytes were delivered and skip them.
	for len(security.RedactSubprocessText(body)) > readPageBytes {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, false, err
		}
		next := readRuneStart(data, start+(pageEnd-start)/2)
		if next == start {
			_, size := utf8.DecodeRune(data[start:pageEnd])
			next = min(start+max(size, 1), pageEnd)
		}
		if next >= pageEnd {
			return "", 0, 0, false, fmt.Errorf("redacted source unit exceeds Read page budget")
		}
		pageEnd = next
		body = renderBody(pageEnd)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "[Read source bytes %d-%d of %d; end exclusive]\n\n%s", start, pageEnd, len(data), body)
	if pageEnd < len(data) {
		fmt.Fprintf(&out, "\n\n[Partial view. Continue the SAME source file with Read(path=%q, byte_offset=%d, byte_limit=%d). Do not read a spill copy.]", path, pageEnd, byteLimit)
	} else if start > 0 {
		out.WriteString("\n\n[End of file. A whole-file Write requires confirmed, unredacted coverage of every source byte; a tail-only read is insufficient.]")
	}
	return out.String(), start, pageEnd, redactor.HasRedactions(), nil
}

// ObserveResultProjection applies a receipt only to the exact pinned read
// snapshot that produced it. Successful source IO alone does not mean the
// model received the full body after the batch budget/transport projection.
func (r Read) ObserveResultProjection(_ context.Context, presentation map[string]any, visible bool) {
	view, ok := presentation["read_view"].(map[string]any)
	if !ok {
		return
	}
	path, _ := view["path"].(string)
	hash, _ := view["hash"].(string)
	revisionText, _ := view["revision"].(string)
	revision, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil || path == "" || hash == "" || revision == 0 {
		return
	}
	r.state.confirmVisible(path, hash, revision, visible)
}
