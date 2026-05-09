package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// readAll drives Reader to exhaustion, returning all frames + the
// terminal error (io.EOF or otherwise). Test helper.
func readAll(t *testing.T, body string) ([]Frame, error) {
	t.Helper()
	r := NewReader(strings.NewReader(body))
	var frames []Frame
	for {
		f, err := r.Next()
		if err != nil {
			return frames, err
		}
		frames = append(frames, f)
	}
}

func TestNext_SingleSimpleFrame(t *testing.T) {
	frames, err := readAll(t, "data: hello\n\n")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
	if len(frames) != 1 || frames[0].Data != "hello" {
		t.Fatalf("got %+v", frames)
	}
}

func TestNext_EventAndData(t *testing.T) {
	body := "event: ping\ndata: keepalive\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 1 {
		t.Fatalf("count: %d", len(frames))
	}
	f := frames[0]
	if f.Event != "ping" || f.Data != "keepalive" {
		t.Fatalf("got %+v", f)
	}
}

func TestNext_MultiLineDataJoinedWithNewline(t *testing.T) {
	body := "data: line1\ndata: line2\ndata: line3\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 1 {
		t.Fatalf("count: %d, frames: %+v", len(frames), frames)
	}
	if got := frames[0].Data; got != "line1\nline2\nline3" {
		t.Fatalf("Data = %q", got)
	}
}

func TestNext_CommentLineSkipped(t *testing.T) {
	body := ": this is a heartbeat comment\n: another\ndata: real\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 1 || frames[0].Data != "real" {
		t.Fatalf("got %+v", frames)
	}
}

func TestNext_EmptyDataNotDispatched(t *testing.T) {
	// Per spec: a comment-only segment followed by blank line should
	// emit nothing.
	body := ": comment\n\ndata: kept\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 1 || frames[0].Data != "kept" {
		t.Fatalf("got %+v", frames)
	}
}

func TestNext_DataPrefixWithoutSpace(t *testing.T) {
	// Spec: only ONE leading space stripped. Anthropic uses "data: ".
	// Gemini sometimes uses "data:" (no space).
	body := "data:no_space\n\ndata: with_space\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 2 {
		t.Fatalf("count: %d", len(frames))
	}
	if frames[0].Data != "no_space" || frames[1].Data != "with_space" {
		t.Fatalf("got %+v", frames)
	}
}

func TestNext_DataPrefixDoubleSpacePreservesSecondSpace(t *testing.T) {
	body := "data:  doublespace\n\n"
	frames, _ := readAll(t, body)
	if frames[0].Data != " doublespace" {
		t.Fatalf("Data = %q (want exactly one leading space)", frames[0].Data)
	}
}

func TestNext_IDField(t *testing.T) {
	body := "id: abc123\ndata: x\n\n"
	frames, _ := readAll(t, body)
	if frames[0].ID != "abc123" {
		t.Fatalf("ID = %q", frames[0].ID)
	}
}

func TestNext_IDWithNULDropped(t *testing.T) {
	body := "id: bad\x00id\ndata: x\n\n"
	frames, _ := readAll(t, body)
	if frames[0].ID != "" {
		t.Fatalf("NUL-tainted ID should be dropped, got %q", frames[0].ID)
	}
	if frames[0].Data != "x" {
		t.Fatalf("data still expected: %q", frames[0].Data)
	}
}

func TestNext_RetryParsed(t *testing.T) {
	body := "retry: 5000\ndata: x\n\n"
	frames, _ := readAll(t, body)
	if frames[0].Retry != 5000 {
		t.Fatalf("Retry = %d", frames[0].Retry)
	}
}

func TestNext_RetryUnparseableIgnored(t *testing.T) {
	body := "retry: abc\ndata: x\n\n"
	frames, _ := readAll(t, body)
	if frames[0].Retry != 0 {
		t.Fatalf("Retry should default to 0 on bad input, got %d", frames[0].Retry)
	}
}

func TestNext_UnknownFieldIgnored(t *testing.T) {
	body := "weird-field: value\ndata: kept\n\n"
	frames, _ := readAll(t, body)
	if frames[0].Data != "kept" {
		t.Fatalf("got %+v", frames)
	}
}

func TestNext_FieldNoColonTreatedAsName(t *testing.T) {
	// Spec: "If the line contains no U+003A COLON character then…
	// process the field using the whole line as the field name with
	// the empty string as the field value." `data` (just the word)
	// → empty data appended.
	body := "data\ndata: real\n\n"
	frames, _ := readAll(t, body)
	if frames[0].Data != "\nreal" {
		t.Fatalf("Data = %q", frames[0].Data)
	}
}

func TestNext_MultipleFramesInSequence(t *testing.T) {
	body := "data: one\n\ndata: two\n\ndata: three\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 3 {
		t.Fatalf("count: %d", len(frames))
	}
	for i, want := range []string{"one", "two", "three"} {
		if frames[i].Data != want {
			t.Errorf("frames[%d].Data = %q, want %q", i, frames[i].Data, want)
		}
	}
}

func TestNext_AnthropicShape(t *testing.T) {
	// Realistic Anthropic frame.
	body := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 1 {
		t.Fatalf("count")
	}
	f := frames[0]
	if f.Event != "content_block_delta" {
		t.Errorf("Event = %q", f.Event)
	}
	if !strings.Contains(f.Data, "text_delta") {
		t.Errorf("Data missing payload: %q", f.Data)
	}
}

func TestNext_OpenAIShape_NoEventField(t *testing.T) {
	// OpenAI sends `data: {…}\n\n` without event:.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 2 {
		t.Fatalf("count: %d", len(frames))
	}
	if frames[0].Event != "" {
		t.Errorf("OpenAI frame should have empty Event, got %q", frames[0].Event)
	}
	if frames[1].Data != "[DONE]" {
		t.Errorf("DONE sentinel: %q", frames[1].Data)
	}
}

func TestNext_CRLF(t *testing.T) {
	body := "event: x\r\ndata: y\r\n\r\n"
	frames, _ := readAll(t, body)
	if len(frames) != 1 {
		t.Fatalf("count: %d", len(frames))
	}
	if frames[0].Event != "x" || frames[0].Data != "y" {
		t.Errorf("got %+v", frames[0])
	}
}

func TestNext_BareCR(t *testing.T) {
	body := "event: x\rdata: y\r\r"
	frames, _ := readAll(t, body)
	if len(frames) != 1 {
		t.Fatalf("count: %d (frames: %+v)", len(frames), frames)
	}
	if frames[0].Event != "x" || frames[0].Data != "y" {
		t.Errorf("got %+v", frames[0])
	}
}

func TestNext_TrailingFrameWithoutBlankLine(t *testing.T) {
	// Some servers / proxies forget the final blank line. We should
	// still emit the in-progress frame at EOF.
	body := "data: dangling"
	frames, err := readAll(t, body)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if len(frames) != 1 || frames[0].Data != "dangling" {
		t.Fatalf("got %+v", frames)
	}
}

func TestNext_OnlyComments(t *testing.T) {
	body := ": ping\n: ping\n: ping\n\n"
	frames, err := readAll(t, body)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("comments alone shouldn't produce frames: %+v", frames)
	}
}

func TestNext_BlankBeforeData(t *testing.T) {
	// Leading blank lines should be ignored (no in-progress frame to
	// dispatch).
	body := "\n\n\ndata: x\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 1 || frames[0].Data != "x" {
		t.Fatalf("got %+v", frames)
	}
}

func TestNext_ChunkBoundaryMidLine(t *testing.T) {
	// Simulate an HTTP chunk that splits a `data:` line halfway. The
	// underlying scanner should reassemble it across reads. We use a
	// chunkedReader to force this.
	body := "data: hello world\n\ndata: another\n\n"
	r := NewReader(&chunkedReader{
		chunks: []string{
			"data: hel",
			"lo wor",
			"ld\n\ndata: an",
			"other\n\n",
		},
	})
	var frames []Frame
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		frames = append(frames, f)
	}
	if len(frames) != 2 {
		t.Fatalf("count: %d", len(frames))
	}
	if frames[0].Data != "hello world" || frames[1].Data != "another" {
		t.Fatalf("got %+v", frames)
	}
	_ = body
}

// chunkedReader yields its `chunks` one Read() at a time, verifying
// the parser handles partial reads. mid-line splits are the failure
// mode that motivated extracting this parser in the first place.
type chunkedReader struct {
	chunks []string
	idx    int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.idx >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.idx])
	c.idx++
	return n, nil
}

func TestNext_OversizedLineErrors(t *testing.T) {
	// A line bigger than the cap should trigger bufio.ErrTooLong via
	// scanner.Err(). We use a tiny cap to make the test fast.
	huge := strings.Repeat("x", 1024)
	body := "data: " + huge + "\n\n"
	r := NewReaderSize(strings.NewReader(body), 64) // cap below line size
	_, err := r.Next()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("expected non-EOF error for oversized line, got %v", err)
	}
}

func TestNext_IDPersistsAcrossFramesInSameStream(t *testing.T) {
	// Per spec the lastEventID is global to a stream — but our
	// implementation resets per-frame because we never resume.
	// Document that with a test: each frame's ID is independent.
	body := "id: a\ndata: 1\n\ndata: 2\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 2 {
		t.Fatalf("count: %d", len(frames))
	}
	if frames[0].ID != "a" {
		t.Errorf("first frame ID = %q", frames[0].ID)
	}
	if frames[1].ID != "" {
		t.Errorf("second frame ID should be empty (no carry-over), got %q", frames[1].ID)
	}
}

func TestNext_EmptyReaderImmediatelyEOF(t *testing.T) {
	r := NewReader(strings.NewReader(""))
	_, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF on empty input, got %v", err)
	}
}

func TestNext_TransportError(t *testing.T) {
	// A reader that errors mid-stream: ensure we surface it not as EOF.
	r := NewReader(&erroringReader{err: errors.New("connection reset")})
	_, err := r.Next()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("expected wrapped transport error, got %v", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error should contain underlying message: %v", err)
	}
}

type erroringReader struct {
	err error
}

func (e *erroringReader) Read(p []byte) (int, error) { return 0, e.err }

func TestNext_DataLineOnlyNoCRLFAtAllJustEOF(t *testing.T) {
	// `data: foo` without any line break — should still surface as a
	// dangling frame at EOF (server abruptly closed).
	body := "data: foo"
	frames, err := readAll(t, body)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if len(frames) != 1 || frames[0].Data != "foo" {
		t.Fatalf("got %+v", frames)
	}
}

func TestSplitLineAnyEOL_HandlesAllThree(t *testing.T) {
	// Spot-check the SplitFunc directly so we don't conflate with
	// dispatch logic. Each EOL terminator should yield same line.
	cases := map[string]string{
		"line\n":   "line",
		"line\r\n": "line",
		"line\r":   "line",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			data := []byte(input)
			adv, tok, err := splitLineAnyEOL(data, true)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if adv != len(data) {
				t.Errorf("advance = %d, want %d", adv, len(data))
			}
			if string(tok) != want {
				t.Errorf("token = %q, want %q", tok, want)
			}
		})
	}
}

func TestNext_DataWithEmptyValuePreservesNewline(t *testing.T) {
	// `data:` with no value — produces empty string in the buffer,
	// but the appended "\n" means the data buffer is non-empty so
	// the frame IS dispatched (with Data == "").
	body := "data:\n\n"
	frames, _ := readAll(t, body)
	if len(frames) != 1 {
		t.Fatalf("count: %d", len(frames))
	}
	if frames[0].Data != "" {
		t.Fatalf("Data = %q, want empty", frames[0].Data)
	}
}

func TestNext_ManyFramesPerformance(t *testing.T) {
	// Sanity: 10k frames should parse without choking. Not strict
	// timing, just a smoke check that we don't have quadratic
	// behaviour somewhere.
	var b bytes.Buffer
	for i := 0; i < 10000; i++ {
		b.WriteString("data: ")
		b.WriteString(strings.Repeat("x", 50))
		b.WriteString("\n\n")
	}
	r := NewReader(&b)
	count := 0
	for {
		_, err := r.Next()
		if err != nil {
			break
		}
		count++
	}
	if count != 10000 {
		t.Fatalf("expected 10000 frames, got %d", count)
	}
}
