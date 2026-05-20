package bash

// bash_output_cap_test.go — pins the default Bash output byte cap.
//
// Why this test exists (2026-05-13): the cap shipped at 1 MiB for a
// long time, which let a single chatty Bash invocation (e.g. `make
// build` with verbose linker output, `git log --stat` on a busy repo)
// drop hundreds of KB into the agent's history. Because tool_results
// are kept verbatim in subsequent requests, this caused per-turn
// input_tokens to balloon — users reported "turn 3 was 50k tokens"
// after only a couple of commands. claude-code's
// BASH_MAX_OUTPUT_DEFAULT is 30,000 chars; we now match (32 KiB —
// nearest power of two, same magnitude). The cap stays configurable
// via [tools.bash] max_output_bytes for projects that genuinely want
// the full output (e.g. eval harnesses that grep the full log).

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestBashDefaultMaxOutputBytes_CapsAtConfiguredLimit(t *testing.T) {
	// Construct Bash with the zero-value settings struct — this routes
	// to the in-process default (32 KiB after 2026-05-13). Run a
	// command that produces ~100 KiB of stdout and confirm we truncate.
	b := &Bash{gate: permission.New(permission.ModeBypass)}
	res, err := b.Execute(context.Background(), map[string]any{
		// Produce ~110 KiB of output. `yes` is unavailable on some
		// minimal CI runners; awk-loop is portable.
		"command": `awk 'BEGIN{ for(i=0;i<10000;i++) print "abcdefghij" }'`,
		"timeout": 5,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	if !strings.Contains(res.Output, "[output truncated") {
		tail := res.Output
		if len(tail) > 200 {
			tail = tail[len(tail)-200:]
		}
		t.Errorf("expected truncation marker for 100k-byte output; got %d bytes, last 200 chars: %q",
			len(res.Output), tail)
	}
	// Cap is 32 KiB + a few bytes of truncation marker. Allow up to
	// 64 KiB of slack for the marker + the trailing diagnostic.
	if len(res.Output) > 64*1024 {
		t.Errorf("Bash result %d bytes exceeds 64 KiB — the truncation cap regressed", len(res.Output))
	}
}

func TestBashDefaultMaxOutputBytes_SmallOutputUnaffected(t *testing.T) {
	// Belt-and-braces: a small command should round-trip without a
	// truncation marker.
	b := &Bash{gate: permission.New(permission.ModeBypass)}
	res, err := b.Execute(context.Background(), map[string]any{
		"command": `echo hello-world`,
		"timeout": 5,
	})
	if err != nil || res == nil {
		t.Fatalf("execute: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Output, "hello-world") {
		t.Errorf("short output should round-trip; got %q", res.Output)
	}
	if strings.Contains(res.Output, "truncated") {
		t.Errorf("short output must not be truncated; got %q", res.Output)
	}
}
