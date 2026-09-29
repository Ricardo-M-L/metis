package agent

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestRecoverSubAgentTerminalPreservesHistoryOwnershipAndExistingResult(t *testing.T) {
	dir := t.TempDir()
	transcript, err := NewSubAgentTranscript(dir, "agt-recover", NewSubAgentHeader("agt-recover", "fixture", "owner", "child", t.TempDir(), "default"))
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(transcript.Path())
	if err != nil {
		t.Fatal(err)
	}
	terminal := SubAgentTerminal{Status: "killed", EndedAt: time.Now(), StopHint: "parent turn stopped"}
	if _, err := RecoverSubAgentTerminal(dir, "foreign", "agt-recover", terminal); err == nil {
		t.Fatal("foreign parent recovered child")
	}
	unchanged, _ := os.ReadFile(transcript.Path())
	if !bytes.Equal(before, unchanged) {
		t.Fatal("foreign recovery changed transcript")
	}
	got, err := RecoverSubAgentTerminal(dir, "owner", "agt-recover", terminal)
	if err != nil || got.Terminal == nil || got.Terminal.Status != "killed" {
		t.Fatalf("recovered=%+v err=%v", got, err)
	}
	after, _ := os.ReadFile(transcript.Path())
	if !bytes.HasPrefix(after, before) {
		t.Fatal("recovery truncated history")
	}
	terminal.Status = "failed"
	got, err = RecoverSubAgentTerminal(dir, "owner", "agt-recover", terminal)
	if err != nil || got.Terminal.Status != "killed" {
		t.Fatalf("existing terminal overwritten=%+v err=%v", got, err)
	}
	repeated, _ := os.ReadFile(transcript.Path())
	if !bytes.Equal(after, repeated) {
		t.Fatal("repeated recovery appended a second terminal")
	}
	terminal.Status = "completed"
	if _, err := RecoverSubAgentTerminal(dir, "owner", "agt-recover", terminal); err == nil {
		t.Fatal("recovery invented success")
	}
}
