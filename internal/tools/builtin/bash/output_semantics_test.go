package bash

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestNormalizeCapturedOutputCollapsesTerminalSpinnerFrames(t *testing.T) {
	raw := "\x1b[31m◒ Cloning repository…\x1b[0m\r" +
		"\x1b[2K◐ Cloning repository…\r" +
		"\x1b[2K◇ Installation complete\n" +
		"◇ Installed 1 skill\n"
	got := normalizeCapturedOutput(raw)
	if strings.ContainsAny(got, "◒◐") || strings.Contains(got, "\x1b[") {
		t.Fatalf("stale animation frames leaked into model output: %q", got)
	}
	if !strings.Contains(got, "Installation complete") || strings.Count(got, "Installed 1 skill") != 1 {
		t.Fatalf("settled status was lost or duplicated: %q", got)
	}
}

func TestNormalizeCapturedOutputPreservesRepeatedOrdinaryLines(t *testing.T) {
	got := normalizeCapturedOutput("x\nx\n")
	if got != "x\nx" {
		t.Fatalf("ordinary repeated output is evidence and must be preserved: %q", got)
	}
}

func TestNormalizeCapturedOutputKeepsFailureAfterSpinner(t *testing.T) {
	raw := "◒ Cloning repository…\r◐ Cloning repository…\r" +
		"■ Failed to clone repository\nAuthentication failed for https://example.invalid/repo.git\n"
	got := normalizeCapturedOutput(raw)
	for _, want := range []string{"Failed to clone repository", "Authentication failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("settled diagnostic %q was lost: %q", want, got)
		}
	}
	if strings.ContainsAny(got, "◒◐") {
		t.Fatalf("stale spinner frames leaked: %q", got)
	}
}

func TestNormalizeCapturedOutputCollapsesSpinnerFromMidFrameTail(t *testing.T) {
	raw := "G◒ Cloning repository…◐ Cloning repository…◓ Cloning repository…" +
		"◑ Final clone frame\n■ Failed to clone repository\n"
	got := normalizeCapturedOutput(raw)
	for _, stale := range []string{"G◒", "◐ Cloning", "◓ Cloning"} {
		if strings.Contains(got, stale) {
			t.Fatalf("capped mid-frame prefix %q leaked: %q", stale, got)
		}
	}
	if !strings.Contains(got, "◑ Final clone frame") || !strings.Contains(got, "Failed to clone repository") {
		t.Fatalf("settled frame/diagnostic lost: %q", got)
	}
}

func TestInterpretSearchExitOne(t *testing.T) {
	tests := []struct {
		name    string
		command string
		output  string
		want    string
		handled bool
	}{
		{name: "rg no match", command: "rg needle .", want: "No matches found", handled: true},
		{name: "grep no match", command: "/usr/bin/grep needle file", want: "No matches found", handled: true},
		{
			name: "read only find chain",
			command: `find "/Users/tester/Library/Photos" -name "IMG_0309.JPG" 2>/dev/null; ` +
				`find /Users/tester/Downloads -name "IMG_0309.JPG" 2>/dev/null`,
			want: "No matches found; some directories were inaccessible", handled: true,
		},
		{name: "find with unproven partial output", command: "find /tmp -name x 2>/dev/null", output: "/tmp/x", handled: false},
		{name: "find with access diagnostic", command: "find /tmp -name x", output: "/tmp/x\nfind: /tmp/private: Permission denied", want: "Search completed with partial access; some directories were inaccessible", handled: true},
		{name: "find unsuppressed empty exit", command: "find /tmp -name x", handled: false},
		{name: "find syntax error", command: "/usr/bin/find /tmp -definitely-invalid-predicate", output: "find: -definitely-invalid-predicate: unknown primary or operator", handled: false},
		{name: "earlier suppression cannot hide later syntax error", command: "find /tmp -name none 2>/dev/null; find /tmp -definitely-invalid-predicate", output: "find: -definitely-invalid-predicate: unknown primary or operator", handled: false},
		{name: "grep pipeline diagnostic", command: "grep needle missing", output: "grep: missing: No such file", handled: false},
		{name: "find delete", command: "find /tmp -name x -delete", handled: false},
		{name: "find exec", command: "find /tmp -name x -exec echo {} ;", handled: false},
		{name: "find fprint0", command: "find /tmp -name x -fprint0 report.bin", handled: false},
		{name: "rg preprocessor", command: "rg --pre='python transform.py' needle .", handled: false},
		{name: "output redirect", command: "rg needle . > matches.txt", handled: false},
		{name: "command substitution", command: "rg needle $(pwd)", handled: false},
		{name: "unknown chain member", command: "echo x; rg needle .", handled: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, handled := interpretSearchExitOne(tt.command, tt.output)
			if handled != tt.handled || got != tt.want {
				t.Fatalf("interpretSearchExitOne() = (%q, %v), want (%q, %v)", got, handled, tt.want, tt.handled)
			}
		})
	}
}

func TestBashExecuteReturnsNoMatchAsSuccessfulSemanticResult(t *testing.T) {
	tool := New(permission.New(permission.ModeBypassPermissions), config.ToolBashSettings{
		Shell:          "/bin/sh",
		TimeoutSeconds: 5,
		MaxOutputBytes: 4096,
	})
	res, err := tool.Execute(context.Background(), map[string]any{
		"command":     "grep -q needle /dev/null",
		"description": "exercise grep no-match semantics",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError || !strings.Contains(res.Output, "No matches found") || strings.Contains(res.Output, "exit status 1") {
		t.Fatalf("exit 1 no-match result = %#v, want successful semantic result", res)
	}
}
