//go:build linux

package jobs

import (
	"context"
	"strings"
	"testing"
)

func TestShellQuoteSingle_Plain(t *testing.T) {
	got := shellQuoteSingle("hello")
	if got != "'hello'" {
		t.Errorf("plain quote: got %q, want %q", got, "'hello'")
	}
}

func TestShellQuoteSingle_EscapesEmbeddedSingleQuote(t *testing.T) {
	// foo'bar should become 'foo'\''bar' — the standard POSIX trick.
	got := shellQuoteSingle("foo'bar")
	want := `'foo'\''bar'`
	if got != want {
		t.Errorf("embedded quote: got %q, want %q", got, want)
	}
}

func TestShellQuoteSingle_HandlesSpecials(t *testing.T) {
	// Backslashes, dollars, semicolons — all neutered by single quotes
	// (no expansion happens inside '…').
	got := shellQuoteSingle(`$(rm -rf /); echo \pwned`)
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Errorf("special chars not single-quoted: %q", got)
	}
	// $(rm -rf /) appears verbatim inside the quotes — sh won't
	// expand it.
	if !strings.Contains(got, "$(rm -rf /)") {
		t.Errorf("inner $(rm -rf /) should appear verbatim: %q", got)
	}
}

func TestOOMWrappedCommand_LinuxBuildsShWrapper(t *testing.T) {
	cmd := OOMWrappedCommand(context.Background(), "/bin/bash", "echo hi")
	if cmd.Path != "/bin/sh" {
		t.Errorf("Linux wrapper should exec /bin/sh, got %q", cmd.Path)
	}
	if len(cmd.Args) < 3 {
		t.Fatalf("expected at least 3 args, got %v", cmd.Args)
	}
	if cmd.Args[0] != "/bin/sh" || cmd.Args[1] != "-c" {
		t.Errorf("first args should be /bin/sh -c, got %v", cmd.Args[:2])
	}
	body := cmd.Args[2]
	if !strings.Contains(body, "oom_score_adj") {
		t.Errorf("wrapper should write oom_score_adj: %q", body)
	}
	if !strings.Contains(body, "echo 1000") {
		t.Errorf("wrapper should write 1000 to oom_score_adj: %q", body)
	}
	if !strings.Contains(body, "2>/dev/null") {
		t.Errorf("wrapper should silence permission errors: %q", body)
	}
	if !strings.Contains(body, "exec '/bin/bash'") {
		t.Errorf("wrapper should exec the original shell: %q", body)
	}
	if !strings.Contains(body, "'echo hi'") {
		t.Errorf("wrapper should pass cmd through quoted: %q", body)
	}
}

func TestOOMWrappedCommand_LinuxQuotesUserCmdSafely(t *testing.T) {
	// User's command contains a single quote — must be escaped so the
	// outer sh -c '…' doesn't terminate early.
	cmd := OOMWrappedCommand(context.Background(), "/bin/bash", `echo "it's fine"`)
	body := cmd.Args[2]
	// The sequence '\'' is the escape; verify it appears for the
	// embedded `'` in "it's".
	if !strings.Contains(body, `'\''`) {
		t.Errorf("embedded single quote not escaped: %q", body)
	}
}

func TestOOMWrappedCommand_LinuxDefaultsToBashWhenShellEmpty(t *testing.T) {
	cmd := OOMWrappedCommand(context.Background(), "", "true")
	body := cmd.Args[2]
	if !strings.Contains(body, "exec '/bin/bash'") {
		t.Errorf("empty shell should default to /bin/bash: %q", body)
	}
}
