package tui

import (
	"strings"
	"testing"
	"time"
)

// TestSummarizeToolResult_PerTool locks in the per-tool summary phrasing
// so the chat surface stays consistent if someone tweaks one branch.
// Cases mirror claude-code's actual transcript samples (see
// 2026-04-29 *.txt) — `Read foo.py (350 lines)`, `Added 8 lines, removed
// 4 lines`, etc.
func TestSummarizeToolResult_PerTool(t *testing.T) {
	cases := []struct {
		name     string
		te       ToolEvent
		contains []string
	}{
		{
			name: "Read with path",
			te: ToolEvent{
				ToolName: "Read",
				Input:    map[string]any{"path": "/tmp/foo.py"},
				Output:   "line1\nline2\nline3\n",
				Duration: 12 * time.Millisecond,
			},
			contains: []string{"12ms", "Read foo.py", "(3 lines)"},
		},
		{
			// go-udiff's myers algorithm groups the unchanged "c" into
			// the replacement chunk, so this single mixed edit reports
			// 3 inserts (B!, c, d) and 2 deletes (b, c). Matches the
			// unified-diff line counts claude-code displays.
			name: "Edit add+remove",
			te: ToolEvent{
				ToolName: "Edit",
				Input: map[string]any{
					"old_string": "a\nb\nc\n",
					"new_string": "a\nB!\nc\nd\n",
				},
				Duration: 5 * time.Millisecond,
			},
			contains: []string{"Added 3 lines, removed 2 lines"},
		},
		{
			name: "Write with content",
			te: ToolEvent{
				ToolName: "Write",
				Input: map[string]any{
					"path":    "/tmp/new.go",
					"content": "package x\n\nfunc Y(){}\n",
				},
				Duration: 8 * time.Millisecond,
			},
			contains: []string{"Wrote new.go", "(3 lines)"},
		},
		{
			name: "Bash first-line",
			te: ToolEvent{
				ToolName: "Bash",
				Output:   "  \n  \nhello world\nmore\n",
				Duration: 100 * time.Millisecond,
			},
			contains: []string{"100ms", "hello world"},
		},
		{
			name: "Grep match count",
			te: ToolEvent{
				ToolName: "Grep",
				Output:   "match1\nmatch2\nmatch3",
				Duration: 50 * time.Millisecond,
			},
			contains: []string{"3 matches"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeToolResult(c.te)
			for _, want := range c.contains {
				if !strings.Contains(got, want) {
					t.Errorf("summarizeToolResult: %q missing %q", got, want)
				}
			}
		})
	}
}

// TestSummarizeToolResult_ReadError — error-path Read must NOT report
// "(N lines)" since the Output is the error message, not file content.
// Pre-fix: a stat error rendered as "Read foo.go (1 lines)" which read
// like a tiny successful read; post-fix it's "Read foo.go — failed".
// Image bug 2026-05-15.
func TestSummarizeToolResult_ReadError(t *testing.T) {
	te := ToolEvent{
		ToolName: "Read",
		Input:    map[string]any{"path": "/tmp/missing.go"},
		Output:   "stat /tmp/missing.go: no such file or directory",
		IsError:  true,
		Duration: 0,
	}
	got := summarizeToolResult(te)
	if strings.Contains(got, "lines)") {
		t.Errorf("error summary should NOT claim a line count; got %q", got)
	}
	if !strings.Contains(got, "missing.go") {
		t.Errorf("error summary should still surface basename; got %q", got)
	}
	if !strings.Contains(got, "failed") {
		t.Errorf("error summary should say 'failed'; got %q", got)
	}
}

// TestSummarizeToolResult_GlobGrepError — an errored Glob/Grep must not
// count its error-message lines as matches (the "✗ Found 3 files" bug:
// the model called Glob with a `path` and no `pattern`, the 3-line error
// hint got line-counted as files found).
func TestSummarizeToolResult_GlobGrepError(t *testing.T) {
	for _, tool := range []string{"Glob", "Grep"} {
		te := ToolEvent{
			ToolName: tool,
			Input:    map[string]any{"path": "/some/file"},
			Output:   "Glob: `pattern` field is required (e.g. \"**/*.go\").\n\nYou passed a `path` field. Use Read.",
			IsError:  true,
		}
		got := summarizeToolResult(te)
		if strings.Contains(got, "Found") || strings.Contains(got, "matches") || strings.Contains(got, "match ") {
			t.Errorf("%s error summary must not report a count; got %q", tool, got)
		}
		if !strings.Contains(got, "failed") {
			t.Errorf("%s error summary should say 'failed'; got %q", tool, got)
		}
	}
}

func TestNormalizeToolOutput_CollapsesCRAndANSISpinnerFrames(t *testing.T) {
	raw := "\x1b[31m◒ Cloning repository…\x1b[0m\r" +
		"\x1b[2K◐ Cloning repository…\r" +
		"\x1b[2K◇ Installation complete\n" +
		"◇ Installed 1 skill\n" +
		"◇ Installed 1 skill\n"
	got := normalizeToolOutput(raw)
	if strings.ContainsAny(got, "◒◐") || strings.Contains(got, "\x1b[") {
		t.Fatalf("animation/ANSI frames leaked into settled output: %q", got)
	}
	if !strings.Contains(got, "Installation complete") || !strings.Contains(got, "Installed 1 skill") {
		t.Fatalf("final status evidence was lost: %q", got)
	}
	if strings.Count(got, "Installed 1 skill") != 2 {
		t.Fatalf("ordinary repeated status lines are evidence and must be preserved: %q", got)
	}
}

func TestNormalizeToolOutput_PreservesOrdinaryAdjacentDuplicates(t *testing.T) {
	got := normalizeToolOutput("x\nx\n2 passed\n2 passed\n")
	if got != "x\nx\n2 passed\n2 passed" {
		t.Fatalf("ordinary duplicate output was collapsed: %q", got)
	}
}

func TestNormalizeToolOutput_CollapsesInlineSpinnerFramesOnSuccess(t *testing.T) {
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Output:   "◒ Downloading…◐ Downloading…◓ Downloading…◑ Downloaded\nready",
	}
	out := stripANSI(renderToolEvent(te, false))
	for _, stale := range []string{"◒ Downloading", "◐ Downloading", "◓ Downloading"} {
		if strings.Contains(out, stale) {
			t.Fatalf("successful Bash preview leaked stale spinner frame %q:\n%s", stale, out)
		}
	}
	if !strings.Contains(out, "◑ Downloaded") {
		t.Fatalf("successful Bash preview lost the final spinner frame:\n%s", out)
	}
}

func TestNormalizeToolOutput_CollapsesTailCappedSpinnerWithPrefixFragment(t *testing.T) {
	raw := "G◒ Cloning repository…◐ Cloning repository…◓ Cloning repository…◑ Clone failed\n" +
		"■ Failed to clone repository\nAuthentication failed for https://github.com/uizze/ui-radar.git."
	got := normalizeToolOutput(raw)
	for _, stale := range []string{"G◒ Cloning", "◐ Cloning", "◓ Cloning"} {
		if strings.Contains(got, stale) {
			t.Fatalf("tail-capped spinner residue %q leaked: %q", stale, got)
		}
	}
	for _, want := range []string{"◑ Clone failed", "Failed to clone repository", "Authentication failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("settled diagnostic %q was lost: %q", want, got)
		}
	}
}

func TestRenderToolEvent_CompletionBeforeTimeoutIsPartialAndExpandable(t *testing.T) {
	te := ToolEvent{
		ID:       "install-1",
		Kind:     "result",
		ToolName: "Bash",
		Input: map[string]any{
			"command":     "npx skills add github/awesome-copilot --skill anti-ui-slop",
			"description": "Install anti-ui-slop skill",
		},
		Output: "◒ Cloning repository…\r\x1b[2K◐ Cloning repository…\r\x1b[2K" +
			"◇ Installation complete\n◇ Installed 1 skill\n[command exceeded timeout 30s]",
		IsError:  true,
		Duration: 30 * time.Second,
	}
	if !completedBeforeTimeout(te) {
		t.Fatal("strong completion marker followed by outer timeout should be partial/recovered")
	}

	compact := stripANSI(renderToolEvent(te, false))
	for _, want := range []string{"reported complete before timeout; verify", "ctrl+O to inspect timeout"} {
		if !strings.Contains(compact, want) {
			t.Fatalf("compact partial state missing %q:\n%s", want, compact)
		}
	}
	if strings.Contains(compact, "command exceeded timeout") {
		t.Fatalf("compact partial state should defer raw timeout to expansion:\n%s", compact)
	}

	expanded := stripANSI(renderToolEvent(te, true))
	if !strings.Contains(expanded, "command exceeded timeout 30s") {
		t.Fatalf("expanded partial state lost original timeout evidence:\n%s", expanded)
	}
	if strings.ContainsAny(expanded, "◒◐") {
		t.Fatalf("expanded diagnostics should keep evidence, not animation frames:\n%s", expanded)
	}
}

func TestRenderToolEvent_PureTimeoutIsOneSemanticResult(t *testing.T) {
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Input:    map[string]any{"command": "sleep 30"},
		Output:   "\n\n[command exceeded timeout 20s]",
		IsError:  true,
		// A restored or forwarded event may not retain its start timestamp.
		// The marker is still authoritative and must not be paired with 0ms.
		Duration: 0,
	}

	out := stripANSI(renderToolEvent(te, false))
	if got := strings.Count(out, "timed out after 20s"); got != 1 {
		t.Fatalf("timeout should appear once as a semantic result, got %d:\n%s", got, out)
	}
	for _, unwanted := range []string{"0ms", "command exceeded timeout", "[command"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("raw/contradictory timeout text %q leaked:\n%s", unwanted, out)
		}
	}
}

func TestRenderToolEvent_TimeoutKeepsPriorOutputWithoutMarker(t *testing.T) {
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Input:    map[string]any{"command": "make all"},
		Output:   "compiled 12 targets\ncache saved\n\n[command exceeded timeout 1m0s]",
		IsError:  true,
	}

	for _, expanded := range []bool{false, true} {
		out := stripANSI(renderToolEvent(te, expanded))
		for _, want := range []string{"timed out after 1m0s", "compiled 12 targets", "cache saved"} {
			if !strings.Contains(out, want) {
				t.Fatalf("expanded=%v timeout rendering lost %q:\n%s", expanded, want, out)
			}
		}
		if strings.Contains(out, "command exceeded timeout") || strings.Contains(out, "0ms") {
			t.Fatalf("expanded=%v leaked raw marker or synthetic duration:\n%s", expanded, out)
		}
	}
}

func TestSplitCommandTimeoutOutput_OnlyMatchesTerminalMarker(t *testing.T) {
	body, limit, ok := splitCommandTimeoutOutput("progress\n[command exceeded timeout 250ms]")
	if !ok || body != "progress" || limit != "250ms" {
		t.Fatalf("terminal timeout split = (%q, %q, %v)", body, limit, ok)
	}

	original := "warning: [command exceeded timeout 20s] but process recovered\nready"
	body, limit, ok = splitCommandTimeoutOutput(original)
	if ok || body != original || limit != "" {
		t.Fatalf("ordinary timeout prose was rewritten: (%q, %q, %v)", body, limit, ok)
	}
}

func TestCompletedBeforeTimeout_RejectsWeakOrContradictoryEvidence(t *testing.T) {
	base := ToolEvent{
		Kind: "result", ToolName: "Bash", IsError: true,
		Input: map[string]any{"command": "npx skills add owner/repo --skill demo"},
	}
	cases := map[string]string{
		"zero count":             "Installation complete\nInstalled 0 skills\n[command exceeded timeout 30s]",
		"negated count":          "not installed 1 skill\n[command exceeded timeout 30s]",
		"fatal after completion": "Installed 1 skill\nValidation failed: SKILL.md missing\n[command exceeded timeout 30s]",
		"timeout before marker":  "[command exceeded timeout 30s]\nInstalled 1 skill",
		"vague completion":       "Installation complete\n[command exceeded timeout 30s]",
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			te := base
			te.Output = output
			if completedBeforeTimeout(te) {
				t.Fatalf("contradictory/weak installer output was mislabeled partial: %q", output)
			}
		})
	}

	t.Run("unrelated command", func(t *testing.T) {
		te := base
		te.Input = map[string]any{"command": "go test ./..."}
		te.Output = "Installed 1 skill\n[command exceeded timeout 30s]"
		if completedBeforeTimeout(te) {
			t.Fatal("arbitrary command output must not trigger installer recovery")
		}
	})
}

func TestRenderToolEvent_SecurityDenialNeverBecomesRecovered(t *testing.T) {
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Output: "Installation complete\n" +
			"Error: denied by permission policy: newline inside quotes followed by a #-prefixed line — can hide arguments from line-based permission checks\n" +
			"[command exceeded timeout 30s]",
		IsError: true,
	}
	if completedBeforeTimeout(te) {
		t.Fatal("permission/security refusal must remain an important error")
	}
	out := stripANSI(renderToolEvent(te, false))
	if strings.Contains(out, "completed before timeout") || !strings.Contains(out, "denied by permission policy") {
		t.Fatalf("security refusal was hidden or mislabeled:\n%s", out)
	}
}

func TestRenderToolEvent_DenialRendersCompactSummary(t *testing.T) {
	// User-reported 2026-08 regressions, fixed in two steps:
	//  1. "2ms · denied: bash-security rule #23: newline inside an
	//     unclosed …" — a 60-rune truncation that leaked the internal
	//     rule ID and cut the explanation mid-word.
	//  2. "✗ 2ms · denied" — still ugly ("很丑", 2026-08-15): the
	//     widest column spent on a meaningless 0-2ms duration.
	// Final form mirrors claude-code/codex: a status-only row
	// ("Denied", no glyph, no elapsed time — claude-code/codex parity) plus the reason as prose below.
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Input:    map[string]any{"command": "curl -s X | python3 -c \"print(1\nprint(2)\""},
		Output:   "denied: newline inside quotes followed by a #-prefixed line — can hide arguments from line-based permission checks",
		IsError:  true,
		Duration: 2 * time.Millisecond,
	}
	out := stripANSI(renderToolEvent(te, false))
	for _, want := range []string{"Denied", "can hide arguments from line-based permission checks"} {
		if !strings.Contains(out, want) {
			t.Fatalf("denial row/body missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"2ms", "rule #23", "unclosed …", "denied:", "denied by permission policy:", "⛔"} {
		if strings.Contains(out, banned) {
			t.Fatalf("denial row leaked %q:\n%s", banned, out)
		}
	}
}

func TestRenderToolEvent_BlockedRendersCompactRowWithoutEnvelope(t *testing.T) {
	// User-reported 2026-08-15: the Bash safety classifier produced
	// "[⚠️ blocked] command classified as dangerous: dangerous flag
	// detected: (?i)-\s*rf\s\n\nCommand: <full command>…" — a regex
	// leak plus a full command echo. The row must now read "Blocked"
	// (icon-less, no elapsed time) and the body must show only the
	// human reason, no "[blocked]" envelope and no command echo.
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Input:    map[string]any{"command": "cd proj && rm -rf build"},
		Output: "[blocked] dangerous flag: rm -rf style recursive forced delete\n\n" +
			"The command was not executed. Rephrase it to avoid the flagged part, or ask the user to run it manually.",
		IsError:  true,
		Duration: 2 * time.Millisecond,
	}
	out := stripANSI(renderToolEvent(te, false))
	for _, want := range []string{"Blocked", "rm -rf style recursive forced delete", "was not executed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("blocked row/body missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"2ms", "[blocked]", "(?i)", "Command:", "⚠️"} {
		if strings.Contains(out, banned) {
			t.Fatalf("blocked row leaked %q:\n%s", banned, out)
		}
	}
}

func TestRenderToolEvent_ReadOnlyExitOneIsNeutralNoMatches(t *testing.T) {
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Input: map[string]any{
			"command": "find /Users/tester/Library -name 'IMG_0309.JPG' 2>/dev/null; " +
				"find /Users/tester/Downloads -name 'IMG_0309.JPG' 2>/dev/null",
			"description": "Search more locations for IMG_0309.JPG",
		},
		Output:   "Error: [exit status 1]",
		IsError:  true,
		Duration: 30 * time.Second,
	}
	if !benignReadOnlyNoMatch(te) {
		t.Fatal("read-only find chain with only exit status 1 should be a neutral empty result")
	}
	out := stripANSI(renderToolEvent(te, false))
	if !strings.Contains(out, "No matches") || strings.Contains(out, "exit status 1") {
		t.Fatalf("read-only no-match was still rendered as an execution error:\n%s", out)
	}
	if strings.Contains(out, "recovered") {
		t.Fatalf("empty search result is neutral, not recovered:\n%s", out)
	}
}

func TestRenderToolEvent_ExitOneForMutatingOrUnknownCommandStaysError(t *testing.T) {
	commands := []string{
		"git diff --quiet",
		"find /tmp -name '*.tmp' -delete",
		"find /tmp -name '*.tmp' -fprint report.txt",
		"find /tmp -name '*.tmp' -fprint0 report.bin",
		"find /tmp -name '*.tmp' -fprintf report.txt '%p\\n'",
		"find /tmp -name '*.tmp' -fls report.txt",
		"rg --pre 'python transform.py' token .",
		"rg token . > matches.txt",
		"awk 'BEGIN { system(\"touch /tmp/pwn\") }' data.txt",
		"sed -n 'w report.txt' data.txt",
		"sort -o sorted.txt data.txt",
		"sed --in-place=.bak 's/a/b/' data.txt",
		"sort --output=sorted.txt data.txt",
		"rg token $(touch /tmp/pwn)",
		"rg token `touch /tmp/pwn`",
		"rg token <(touch /tmp/pwn)",
		"(rg token .)",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			te := ToolEvent{
				Kind: "result", ToolName: "Bash", Input: map[string]any{"command": command},
				Output: "[exit status 1]", IsError: true,
			}
			if benignReadOnlyNoMatch(te) {
				t.Fatalf("non-read-only/unknown exit 1 was mislabeled neutral: %q", command)
			}
			out := stripANSI(renderToolEvent(te, false))
			if !strings.Contains(out, "exit status 1") {
				t.Fatalf("real error evidence disappeared for %q:\n%s", command, out)
			}
		})
	}
}

func TestSummarizeToolResult_CloneFailureUsesActionableTail(t *testing.T) {
	te := ToolEvent{
		Kind: "result", ToolName: "Bash", IsError: true,
		Input: map[string]any{"command": "npx skills add uizze/ui-radar"},
		Output: "│\n◇ Source: https://github.com/uizze/ui-radar.git\n" +
			"◒ Cloning repository…\r\x1b[2K◐ Cloning repository…\r\x1b[2K│\n" +
			"■ Failed to clone repository\n│\n" +
			"│ Authentication failed for https://github.com/uizze/ui-radar.git.\n" +
			"└ Installation failed\n",
	}
	got := summarizeToolResult(te)
	if !strings.Contains(got, "Authentication failed") {
		t.Fatalf("summary chose installer chrome instead of actionable cause: %q", got)
	}
	rendered := stripANSI(renderToolEvent(te, false))
	if !strings.Contains(rendered, "Failed to clone repository") || !strings.Contains(rendered, "Authentication failed") {
		t.Fatalf("normalization dropped final clone diagnostics:\n%s", rendered)
	}
	for _, stale := range []string{"◒ Cloning", "◐ Cloning"} {
		if strings.Contains(rendered, stale) {
			t.Fatalf("clone spinner frame leaked after settlement %q:\n%s", stale, rendered)
		}
	}
}

// TestTruncateMiddle_PreservesBothEnds — for path-bearing error lines
// the basename at the END is what tells the user what failed; the
// pre-fix tail-cut form hid it. Middle truncation keeps both ends
// visible. Image bug 2026-05-15. Uses the same 120-rune cap that
// renderErrorBody passes in production.
func TestTruncateMiddle_PreservesBothEnds(t *testing.T) {
	long := "stat /Users/foo/Documents/公司学习文件/opensource-contributions/claude-code-sourcemap/restored-src/src/coordinator/index.ts/loop.go: no such file or directory"
	got := truncateMiddle(long, 120)
	if len([]rune(got)) > 121 {
		t.Errorf("output too long: %d runes (target ≤120)", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "stat ") {
		t.Errorf("head not preserved: %q", got)
	}
	// The basename `loop.go` and the syscall error tail must survive.
	if !strings.Contains(got, "loop.go") {
		t.Errorf("basename loop.go lost in truncation: %q", got)
	}
	if !strings.Contains(got, "no such file") {
		t.Errorf("error tail lost in truncation: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("middle ellipsis missing: %q", got)
	}
}

// TestTruncateMiddle_ShortLeavesUntouched — strings shorter than
// maxRunes pass through unchanged. Guards against silly off-by-ones.
func TestTruncateMiddle_ShortLeavesUntouched(t *testing.T) {
	in := "stat /tmp/x: no such file"
	if got := truncateMiddle(in, 120); got != in {
		t.Errorf("short input mutated:\n  in:  %q\n  out: %q", in, got)
	}
}

// TestCountEditDiff verifies our line-count math against go-udiff for
// the kinds of inputs Edit tool typically gets — pure-add, pure-remove,
// mixed, identical (no-op).
func TestCountEditDiff(t *testing.T) {
	cases := []struct {
		name         string
		old, new     string
		wantA, wantR int
	}{
		{"identical", "a\nb\n", "a\nb\n", 0, 0},
		{"pure add", "a\n", "a\nb\nc\n", 2, 0},
		{"pure remove", "a\nb\nc\n", "a\n", 0, 2},
		// myers groups the unchanged "c" into the replacement chunk:
		// 3 inserts (B!, c, d) and 2 deletes (b, c) — matches what
		// claude-code shows for similar edits.
		{"mixed", "a\nb\nc\n", "a\nB!\nc\nd\n", 3, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, r := countEditDiff(map[string]any{
				"old_string": c.old,
				"new_string": c.new,
			})
			if a != c.wantA || r != c.wantR {
				t.Errorf("got added=%d removed=%d, want added=%d removed=%d",
					a, r, c.wantA, c.wantR)
			}
		})
	}
}

// TestFormatElapsed covers the spinner-row elapsed clock. Sub-second
// renders as ms, single-digit seconds as `X.Ys`, two-digit seconds as
// `Xs`, then the same Mm Ss / Hh Mm brackets that formatTurnDuration
// uses. The user reported the spinner reading e.g. `120s` instead of
// `2m 0s` for long turns — this regression-tests the M/H switchover.
func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "500ms"},
		{3 * time.Second, "3.0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m 30s"},
		{120 * time.Second, "2m 0s"},
		{55 * time.Minute, "55m 0s"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
	}
	for _, c := range cases {
		if got := formatElapsed(c.d); got != c.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestFormatTurnDuration covers the three brackets in the turn-end
// summary phrasing: under a minute, under an hour, longer.
func TestFormatTurnDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m 30s"},
		{55 * time.Minute, "55m 0s"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
	}
	for _, c := range cases {
		if got := formatTurnDuration(c.d); got != c.want {
			t.Errorf("formatTurnDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestLineCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"a\nb\nc\n", 3},
	}
	for _, c := range cases {
		if got := lineCount(c.in); got != c.want {
			t.Errorf("lineCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFirstNonEmptyLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"\n\nhello", "hello"},
		{"  \n  \n  real line  \nmore", "real line"},
		{"only-line", "only-line"},
	}
	for _, c := range cases {
		if got := firstNonEmptyLine(c.in); got != c.want {
			t.Errorf("firstNonEmptyLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRenderEditDiff_TruncatesAt20Lines keeps a perf guarantee — Edit
// tools that wholesale rewrite a 200-line file shouldn't drown the chat
// surface; we cap at 20 visible lines plus a "+N more" tail.
func TestRenderEditDiff_TruncatesAt20Lines(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 0; i < 50; i++ {
		oldB.WriteString("old line ")
		oldB.WriteString("X\n")
		newB.WriteString("new line ")
		newB.WriteString("Y\n")
	}
	out := renderEditDiff(map[string]any{
		"old_string": oldB.String(),
		"new_string": newB.String(),
	}, false)
	if !strings.Contains(out, "more diff lines") {
		t.Errorf("expected truncation marker; got:\n%s", out)
	}
	// Crude line-count check: count "\n" in the rendered output. The
	// actual diff body is ~20 lines plus the "+N more" tail.
	n := strings.Count(out, "\n")
	if n > 25 {
		t.Errorf("rendered too many lines (%d); cap should kick in", n)
	}
}

// TestBuildTurnRecap covers the deterministic recap synthesizer's
// behavior — short turns produce nothing, mixed-tool turns produce
// structured summaries.
func TestBuildTurnRecap(t *testing.T) {
	read := func(path string) ToolEvent {
		return ToolEvent{Kind: "result", ToolName: "Read", Input: map[string]any{"path": path}}
	}
	edit := func(path string) ToolEvent {
		return ToolEvent{Kind: "result", ToolName: "Edit", Input: map[string]any{"path": path}}
	}
	bash := func(cmd string) ToolEvent {
		return ToolEvent{Kind: "result", ToolName: "Bash", Input: map[string]any{"command": cmd}}
	}

	cases := []struct {
		name     string
		events   []ToolEvent
		want     string
		wantNone bool
	}{
		{name: "empty", events: nil, wantNone: true},
		{name: "single tool", events: []ToolEvent{read("a.go")}, wantNone: true},
		{
			name:   "edit + read",
			events: []ToolEvent{read("a.go"), edit("b.go")},
			want:   "edited b.go · 1 reads",
		},
		{
			name:   "two edits same file",
			events: []ToolEvent{edit("foo.go"), edit("foo.go")},
			want:   "edited foo.go",
		},
		{
			name:   "bash + edit",
			events: []ToolEvent{edit("foo.go"), bash("go test ./...")},
			want:   "edited foo.go · ran `go test ./...`",
		},
		{
			name:   "many edits collapse",
			events: []ToolEvent{edit("a"), edit("b"), edit("c"), edit("d")},
			want:   "edited 4 files",
		},
		{
			name: "errors skipped",
			events: []ToolEvent{
				edit("foo.go"),
				{Kind: "result", ToolName: "Bash", IsError: true, Input: map[string]any{"command": "broken"}},
				read("bar.go"),
			},
			want: "edited foo.go · 1 reads",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildTurnRecap(c.events)
			if c.wantNone {
				if got != "" {
					t.Errorf("expected empty recap, got %q", got)
				}
				return
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestBuildTurnRecap_UsesCurrentTurnSuffixOnly(t *testing.T) {
	m := &Model{
		toolEvents: []ToolEvent{
			{Kind: "result", ToolName: "Read"},
			{Kind: "result", ToolName: "Read"},
			{Kind: "result", ToolName: "Glob"},
			{Kind: "result", ToolName: "Bash", Input: map[string]any{"command": "go test ./..."}},
		},
		turnToolEventStart: 2,
	}
	recap := buildTurnRecap(m.currentTurnToolEvents())
	if strings.Contains(recap, "reads") {
		t.Fatalf("recap accumulated prior-turn reads: %q", recap)
	}
	for _, want := range []string{"1 searches", "ran `go test ./...`"} {
		if !strings.Contains(recap, want) {
			t.Errorf("current-turn recap missing %q: %q", want, recap)
		}
	}
}

// TestRenderToolEvent_LeaderRowFormat ensures the ⏺/⎿ hierarchy holds
// on both the in-flight and completed paths.
func TestRenderToolEvent_LeaderRowFormat(t *testing.T) {
	start := renderToolEvent(ToolEvent{
		Kind:     "start",
		ToolName: "Read",
		Input:    map[string]any{"path": "/tmp/foo.py"},
	}, false)
	if !strings.Contains(start, glyphBullet) {
		t.Errorf("in-flight leader missing bullet: %s", start)
	}
	if strings.Contains(start, glyphTreeLeaf) {
		t.Errorf("in-flight should not have tree-leaf yet: %s", start)
	}

	done := renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Read",
		Input:    map[string]any{"path": "/tmp/foo.py"},
		Output:   "line1\nline2\n",
		Duration: 5 * time.Millisecond,
	}, false)
	if !strings.Contains(done, glyphBullet) {
		t.Errorf("done leader missing bullet: %s", done)
	}
	if !strings.Contains(done, glyphTreeLeaf) {
		t.Errorf("done missing tree-leaf summary: %s", done)
	}
}

func TestRenderToolEvent_ExplorationOutputCompactAndExpandable(t *testing.T) {
	te := ToolEvent{
		Kind:     "result",
		ToolName: "Read",
		Input:    map[string]any{"path": "/tmp/large.go"},
		Output:   "line one\nline two\nline three\nline four\nline five\nline six\n",
	}

	compact := stripANSI(renderToolEvent(te, false))
	if !strings.Contains(compact, "ctrl+O to expand") {
		t.Fatalf("compact exploration result should advertise expansion: %q", compact)
	}
	if strings.Contains(compact, "line one") || strings.Contains(compact, "more lines") || strings.Contains(compact, "… +") {
		t.Fatalf("compact exploration result leaked preview/ellipsis wall: %q", compact)
	}

	expanded := stripANSI(renderToolEvent(te, true))
	if !strings.Contains(expanded, "line one") || !strings.Contains(expanded, "line six") {
		t.Fatalf("expanded exploration result should contain the full output: %q", expanded)
	}
	if strings.Contains(expanded, "more lines") || strings.Contains(expanded, "… +") {
		t.Fatalf("expanded exploration result should not truncate with ellipses: %q", expanded)
	}
}

// TestRenderToolEvent_SubAgentIndent — sub-agent tool calls (carrying a
// SubAgentParentID + "sub: " name prefix) must render INDENTED under
// the parent agent, with the "sub: " prefix stripped (the indent says
// "this is from the sub-agent"). Fixes the flat "sub: glob" display
// the user flagged (2026-06-14).
func TestRenderToolEvent_SubAgentIndent(t *testing.T) {
	// A top-level tool: baseline indent (2 spaces before the bullet).
	top := renderToolEvent(ToolEvent{
		Kind: "start", ToolName: "Glob", Input: map[string]any{"pattern": "**/*.go"},
	}, false)
	// A forwarded sub-agent tool: same Glob, but with the sub markers.
	sub := renderToolEvent(ToolEvent{
		Kind: "start", ToolName: "sub: Glob", Input: map[string]any{"pattern": "**/*.go"},
		SubAgentParentID: "tu_parent",
	}, false)

	// The sub row must NOT show the literal "sub: " prefix.
	if strings.Contains(sub, "sub:") {
		t.Errorf("sub-agent row should strip the 'sub: ' prefix; got:\n%q", sub)
	}
	// Both should name the tool (glob, lowercased).
	if !strings.Contains(top, "glob") || !strings.Contains(sub, "glob") {
		t.Fatalf("expected 'glob' in both rows; top=%q sub=%q", top, sub)
	}
	// The sub row must be indented deeper than the top row. Compare the
	// leading whitespace before the first non-space, non-escape glyph.
	if subLead, topLead := leadingSpaces(sub), leadingSpaces(top); subLead <= topLead {
		t.Errorf("sub-agent row should be indented deeper: subLead=%d topLead=%d\ntop=%q\nsub=%q",
			subLead, topLead, top, sub)
	}
}

// leadingSpaces counts leading ASCII spaces, skipping ANSI escape
// sequences (lipgloss color codes) so the count reflects visual indent.
func leadingSpaces(s string) int {
	n := 0
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' { // skip an ANSI escape: ESC ... 'm'
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // consume the 'm'
			continue
		}
		if s[i] == ' ' {
			n++
			i++
			continue
		}
		break
	}
	return n
}
