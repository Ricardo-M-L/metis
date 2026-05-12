package skills

import (
	"context"
	"strings"
	"testing"
	"time"

	pubskill "github.com/Ricardo-M-L/metis/pkg/skill"
)

func TestShouldRunInlineShell_TrustGate(t *testing.T) {
	cases := []struct {
		trust string
		want  bool
	}{
		{pubskill.TrustBuiltin, true},
		{pubskill.TrustTrusted, true},
		{pubskill.TrustUser, true},
		{pubskill.TrustProject, true},
		{pubskill.TrustCommunity, false},
		{"mcp", false},
		{"", false},
		{"unknown", false},
	}
	for _, c := range cases {
		got := ShouldRunInlineShell(c.trust)
		if got != c.want {
			t.Errorf("ShouldRunInlineShell(%q) = %v; want %v", c.trust, got, c.want)
		}
	}
}

func TestExpandInlineShell_BacktickForm(t *testing.T) {
	body := "Current dir: !`pwd | sed 's:.*/::'`"
	got := ExpandInlineShell(context.Background(), body, t.TempDir())
	if strings.Contains(got, "!`") {
		t.Errorf("backtick form not consumed; got %q", got)
	}
	if !strings.Contains(got, "Current dir: ") {
		t.Errorf("surrounding text dropped; got %q", got)
	}
}

func TestExpandInlineShell_FencedForm(t *testing.T) {
	body := "Status:\n```!\necho one\necho two\n```\nend"
	got := ExpandInlineShell(context.Background(), body, "")
	if strings.Contains(got, "```!") {
		t.Errorf("fenced form not consumed; got %q", got)
	}
	if !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Errorf("both echo lines should be present; got %q", got)
	}
	// Surrounding "Status:" / "end" markers preserved.
	if !strings.Contains(got, "Status:") || !strings.Contains(got, "end") {
		t.Errorf("surrounding text dropped; got %q", got)
	}
}

func TestExpandInlineShell_FailingCommandYieldsSentinel(t *testing.T) {
	body := "Result: !`false`"
	got := ExpandInlineShell(context.Background(), body, "")
	if !strings.Contains(got, "[shell error:") {
		t.Errorf("failing cmd should produce [shell error: ...] sentinel; got %q", got)
	}
}

func TestExpandInlineShell_NoShellTokenNoop(t *testing.T) {
	body := "Plain skill body, no shell here."
	got := ExpandInlineShell(context.Background(), body, "")
	if got != body {
		t.Errorf("plain body must round-trip; got %q", got)
	}
}

func TestExpandInlineShell_TimeoutBounded(t *testing.T) {
	// sleep 30s; with the 10s per-call timeout the test must finish
	// well under the test deadline — we cap our wait at 12s to
	// catch a runaway.
	done := make(chan string, 1)
	go func() {
		done <- ExpandInlineShell(context.Background(), "x: !`sleep 30`", "")
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, "[shell error:") {
			t.Errorf("timeout should yield error sentinel; got %q", got)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("timeout enforcement broken — call still running after 12s")
	}
}

func TestExpandInlineShell_OutputBytesCapped(t *testing.T) {
	// Generate ~16 KiB; cap is 8 KiB. We expect the truncation marker.
	body := "data: !`yes a | head -c 16384`"
	got := ExpandInlineShell(context.Background(), body, "")
	if !strings.Contains(got, "truncated") {
		t.Errorf("output > 8 KiB should be truncated; got len=%d", len(got))
	}
}
