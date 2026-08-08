//go:build !windows
// +build !windows

package uv

import (
	"os"
	"testing"
)

func TestReader(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}
	defer pw.Close()
	defer pr.Close()

	pollReader, err := newPollReader(pr)
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}

	msg := "hello"
	n, err := pw.Write([]byte(msg))
	if n != 5 {
		t.Errorf("expected 5 bytes written but got %d", n)
	}
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}

	// Cancel before Read so the assertion is deterministic. Starting the
	// goroutine first races a ready pipe byte against Cancel: on a fast macOS
	// runner Read can consume the leading "h", making the remaining read
	// observe "ello" even though neither behavior tests renderer semantics.
	if !pollReader.Cancel() {
		t.Errorf("expected cancellation to be success")
	}
	p := make([]byte, 1)
	n, err = pollReader.Read(p)
	if n != 0 {
		t.Errorf("expected 0 bytes read but got %d", n)
	}
	if err != ErrCanceled {
		t.Errorf("expected cancel error but got %s", err)
	}

	// Test that read is still possible after cancellation.
	pollReader, err = newPollReader(pr)
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}
	p = make([]byte, 5)
	n, err = pollReader.Read(p)
	if n != 5 {
		t.Errorf("expected 5 bytes written but got %d", n)
	}
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}
	if string(p[:n]) != msg[:n] {
		t.Errorf("expected to read %q but got %q", msg[:n], string(p[:n]))
	}
}
