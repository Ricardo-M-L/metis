package bash

// cappedwriter_test.go — pins the error-aware head+tail truncation
// added 2026-06-13 (MiMo-Code parity; Claude Code truncates head-only).
// A capped Bash output keeps its tail when that tail looks like a
// failure, so compiler/test/stack-trace verdicts at the very end aren't
// lost.

import (
	"strings"
	"testing"
)

func TestCappedWriter_NoTruncationReturnsAll(t *testing.T) {
	w := newCappedWriter(100)
	w.Write([]byte("short output"))
	if w.truncated {
		t.Fatal("should not be truncated")
	}
	if w.preview() != "short output" {
		t.Fatalf("preview = %q", w.preview())
	}
}

func TestCappedWriter_KeepsErrorTail(t *testing.T) {
	w := newCappedWriter(100) // head 70, tail 30
	w.Write([]byte(strings.Repeat("A", 200)))
	w.Write([]byte("\npanic: boom")) // error verdict at the very end
	if !w.truncated {
		t.Fatal("expected truncated")
	}
	got := w.preview()
	if !strings.Contains(got, "panic: boom") {
		t.Errorf("error tail dropped; preview tail = %q", got[len(got)-40:])
	}
	if !strings.HasPrefix(got, "AAAA") {
		t.Error("head dropped")
	}
	if !strings.Contains(got, "error output") {
		t.Errorf("missing omission marker; got %q", got)
	}
}

func TestCappedWriter_HeadOnlyForOrdinaryOutput(t *testing.T) {
	w := newCappedWriter(100)
	w.Write([]byte(strings.Repeat("log line ", 50))) // no error markers
	if !w.truncated {
		t.Fatal("expected truncated")
	}
	got := w.preview()
	if strings.Contains(got, "error output") {
		t.Error("kept tail for non-error output — should be head only")
	}
	// Ordinary output keeps the FULL head cap (max=100), no tail
	// appended — the no-regression property.
	if len(got) > 105 {
		t.Errorf("preview longer than head cap: %d bytes (tail leaked?)", len(got))
	}
	if len(got) < 90 {
		t.Errorf("head shrank below the cap: %d bytes (regression)", len(got))
	}
}

// The tail ring must hold the LAST tailMax bytes across many small
// writes, not the first ones.
func TestCappedWriter_TailRingKeepsMostRecent(t *testing.T) {
	w := newCappedWriter(100)                // tail 30
	w.Write([]byte(strings.Repeat("H", 70))) // fills head
	for i := 0; i < 100; i++ {               // stream lots of tail bytes
		w.Write([]byte("error-marker-here-")) // ensure tail kept + recent
	}
	w.Write([]byte("LASTBYTES_error"))
	got := w.preview()
	if !strings.Contains(got, "LASTBYTES") {
		t.Errorf("tail ring lost the most-recent bytes; tail = %q", got[len(got)-40:])
	}
}
