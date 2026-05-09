// Package sse is a pure-function Server-Sent Events parser.
// Spec: https://html.spec.whatwg.org/multipage/server-sent-events.html
//
// What it does:
//
//   - Reads lines from an io.Reader.
//   - Accumulates `event` / `data` / `id` / `retry` fields.
//   - Dispatches a Frame on every blank line that closes a non-empty
//     event (frames whose data buffer is empty are NOT dispatched, per
//     spec — a stray comment line followed by a blank line should not
//     emit anything).
//
// What it does NOT do:
//
//   - Parse JSON. Frame.Data is the raw spec-mandated string (multiple
//     `data:` lines joined with `\n`, trailing newline stripped). The
//     caller does its own json.Unmarshal — different providers want
//     different envelopes.
//   - Decide what event types are valid. Anthropic uses
//     `content_block_delta`, OpenAI omits `event:` entirely (so
//     Frame.Event is ""), Gemini also implicit. We don't take a stance.
//   - Handle reconnection / Last-Event-ID resume. That's a transport
//     concern; this parser is per-stream.
//
// Why a dedicated package: previously each provider in
// internal/llm/{anthropic,openai,gemini} reimplemented line scanning +
// `data: ` prefix detection + multi-line data joining + comment
// skipping. Three copies meant SSE-spec bugs (chunk-edge buffering,
// comment lines, CRLF) had to be fixed three times, and the parsing
// path had no unit-test coverage because tests had to mock entire
// http.Response bodies. With this package: one parser, one test file,
// providers shrink to "Frame → StreamEvent" routing.
package sse

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
)

// DefaultMaxLineBytes caps a single SSE line — protection against a
// pathological server sending a 10 GB data line that would OOM the
// process. 16 MiB matches what each provider sized bufio.Scanner to
// before this package existed (Gemini specifically: tool-call args
// can serialize past 1 MiB on big payloads).
const DefaultMaxLineBytes = 16 * 1024 * 1024

// Frame is one dispatched SSE event. All fields are zero-valued unless
// the upstream stream populated them.
//
// Per spec, Data is the joined value of every `data:` line in the
// event, separated by `\n`. Trailing `\n` is stripped on dispatch.
// Empty data → frame is NOT emitted (spec: "If the data buffer is an
// empty string, set the data buffer and the event type buffer to the
// empty string and abort these steps").
type Frame struct {
	// Event is the value of the most recent `event:` field for this
	// frame. Empty when the stream omitted the field — by spec the
	// browser EventSource API would synthesize "message" here, but we
	// leave it as "" so callers can tell "no event" apart from
	// "explicit `event: message`".
	Event string

	// Data is the concatenation of every `data:` line in this frame,
	// joined with `\n`. Trailing newline stripped.
	Data string

	// ID is the value of the most recent `id:` field. Per spec a NUL
	// byte in the value voids the field; we drop the whole field in
	// that case (rare).
	ID string

	// Retry is the most recent `retry:` value parsed as an integer
	// (milliseconds). 0 when not set or unparseable. Callers
	// implementing reconnection use this; LLM providers ignore it.
	Retry int
}

// Reader iterates SSE Frames from r. Concurrent calls to Next are not
// safe; the bufio.Scanner is stateful and so is the per-frame field
// accumulator.
//
// Reader holds no resources beyond the wrapped Reader and a scanner
// buffer; nothing to Close (the caller closes the underlying body).
type Reader struct {
	sc *bufio.Scanner
}

// NewReader wraps r with the default 16 MiB max line. For tests or
// constrained environments, use NewReaderSize.
func NewReader(r io.Reader) *Reader {
	return NewReaderSize(r, DefaultMaxLineBytes)
}

// NewReaderSize wraps r with a custom max line. The initial buffer is
// min(64 KiB, maxLine) — bufio.Scanner uses max(initial, maxLine), so
// passing a smaller maxLine than the default initial would silently
// have no effect. Tests rely on this to cap line size; production
// callers (NewReader with 16 MiB max) start at 64 KiB and grow.
//
// Oversized lines surface as bufio.ErrTooLong from Next().
func NewReaderSize(r io.Reader, maxLine int) *Reader {
	initial := 64 * 1024
	if maxLine < initial {
		initial = maxLine
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, initial), maxLine)
	sc.Split(splitLineAnyEOL)
	return &Reader{sc: sc}
}

// Next reads the next dispatched Frame. Returns io.EOF at end of
// stream after dispatching any pending frame (e.g. some servers omit
// the trailing blank line; a non-empty in-progress frame at EOF is
// still emitted).
//
// Returns a wrapped error when the underlying scanner errored
// (network reset, oversized line, transport-level failure).
func (r *Reader) Next() (Frame, error) {
	var (
		event   string
		dataBuf strings.Builder
		id      string
		hasID   bool
		retry   int
		hasData bool
	)

	dispatch := func() (Frame, bool) {
		if !hasData {
			return Frame{}, false
		}
		s := dataBuf.String()
		// Strip ONE trailing newline — per spec data is "buffered with
		// LF separators", and the final LF appended by the field
		// handler is dropped on dispatch.
		s = strings.TrimSuffix(s, "\n")
		f := Frame{Event: event, Data: s, Retry: retry}
		if hasID {
			f.ID = id
		}
		return f, true
	}

	for r.sc.Scan() {
		line := r.sc.Text()

		// Empty line → dispatch the accumulated frame.
		if line == "" {
			if f, ok := dispatch(); ok {
				return f, nil
			}
			// Else: empty data, reset and keep scanning. Per spec the
			// id/retry persist across non-dispatched frames in a real
			// EventSource, but for our use (one parse per HTTP body,
			// no reconnection) it's simpler and equivalent to reset.
			event = ""
			dataBuf.Reset()
			id = ""
			hasID = false
			retry = 0
			hasData = false
			continue
		}

		// Comment line: starts with ":". Drop entirely.
		if line[0] == ':' {
			continue
		}

		// Field parsing. Spec: split on first ':'. If no colon, the
		// whole line is the field name (with empty value).
		var field, value string
		if i := strings.IndexByte(line, ':'); i >= 0 {
			field = line[:i]
			value = line[i+1:]
			// Spec: strip ONE leading space if present.
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		} else {
			field = line
		}

		switch field {
		case "event":
			event = value
		case "data":
			// Append value + "\n" — even for the first data line, so
			// the final dispatch can strip exactly one trailing "\n"
			// to recover the original data, and multi-line data lines
			// end up joined by "\n".
			dataBuf.WriteString(value)
			dataBuf.WriteByte('\n')
			hasData = true
		case "id":
			// Spec: NUL bytes void the id. Most LLM streams don't use
			// id at all; cheap defensive check.
			if !strings.ContainsRune(value, '\x00') {
				id = value
				hasID = true
			}
		case "retry":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n >= 0 {
				retry = n
			}
		default:
			// Unknown field — per spec, ignored.
		}
	}

	// Scanner ran out. Two cases:
	//   1. Clean EOF after a blank-line-terminated frame: dataBuf
	//      empty, return io.EOF.
	//   2. Frame in progress (server forgot the trailing blank line —
	//      Anthropic's `[DONE]` sentinel + abrupt close occasionally
	//      lands here). Dispatch what we have, then EOF on next call.
	if err := r.sc.Err(); err != nil {
		// bufio.ErrTooLong wraps as a transport error so callers can
		// distinguish "your line was too big" from "EOF".
		return Frame{}, err
	}
	if hasData {
		// Mark next call as EOF by clearing the scanner state via
		// errSentinelEOF; we can't easily without restructuring, so
		// we re-use bufio.Scanner's own behaviour: subsequent Scan()
		// returns false with no Err, so the next Next() falls through
		// to the io.EOF return below. Emit the dangling frame now.
		f, _ := dispatch()
		return f, nil
	}
	return Frame{}, io.EOF
}

// splitLineAnyEOL is a bufio.SplitFunc accepting LF, CRLF, and bare CR
// as line terminators (per SSE spec). bufio.ScanLines only handles LF
// and CRLF, missing CR-only — rare in practice but cheap to support
// since some Anthropic gateways through CDNs have been observed
// replacing CRLF with CR after gzip decompression.
func splitLineAnyEOL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		switch b {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			// Look-ahead for CRLF; otherwise treat CR alone as EOL.
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			// Need more bytes to know if it's CRLF.
			return 0, nil, nil
		}
	}
	if atEOF {
		// Final line without EOL.
		return len(data), data, nil
	}
	// Need more.
	return 0, nil, nil
}

// ErrTruncated is returned when Next encounters a non-EOF read error
// before completing a frame — kept for callers that want to
// distinguish stream truncation from clean end-of-stream.
var ErrTruncated = errors.New("sse: stream ended mid-frame")
