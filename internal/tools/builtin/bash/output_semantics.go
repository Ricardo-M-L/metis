package bash

import (
	"regexp"
	"strings"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"
)

var terminalLineResetRE = regexp.MustCompile(`\x1b\[[0-9;?]*[GK]`)
var stderrDevNullRE = regexp.MustCompile(`(?:^|[[:space:];&|])2[[:space:]]*>[[:space:]]*/dev/null(?:$|[[:space:];&|])`)

// Keep captured terminal repaint frames out of the tool result sent back to
// the model. Package managers often write a spinner with carriage returns and
// CSI erase-line sequences; treating that byte stream as ordinary text can
// turn one status row into tens of thousands of context tokens.
//
// This mirrors terminal overwrite semantics while retaining settled status
// and diagnostic lines. It is deliberately applied at the Bash boundary, not
// only by the TUI, so transcripts, exports, and the next model iteration all
// receive the compact result.
func normalizeCapturedOutput(out string) string {
	if out == "" {
		return ""
	}
	out = terminalLineResetRE.ReplaceAllString(out, "\r")
	out = xansi.Strip(out)
	out = strings.ReplaceAll(out, "\r\n", "\n")

	rawLines := strings.Split(out, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, raw := range rawLines {
		frame := raw
		if strings.Contains(raw, "\r") {
			frame = ""
			for _, candidate := range strings.Split(raw, "\r") {
				if strings.TrimSpace(candidate) != "" {
					frame = candidate
				}
			}
		}
		frame = collapseCapturedSpinnerFrames(frame)
		if isCapturedSpinnerLine(frame) && len(lines) > 0 && isCapturedSpinnerLine(lines[len(lines)-1]) {
			lines[len(lines)-1] = frame
			continue
		}
		lines = append(lines, frame)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

const capturedSpinnerRunes = "◒◐◓◑⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"

func isCapturedSpinnerRune(r rune) bool {
	return strings.ContainsRune(capturedSpinnerRunes, r)
}

func isCapturedSpinnerLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(trimmed)
	return isCapturedSpinnerRune(r)
}

func collapseCapturedSpinnerFrames(line string) string {
	count, lastByte := 0, -1
	for byteIdx, r := range line {
		if isCapturedSpinnerRune(r) {
			count++
			lastByte = byteIdx
		}
	}
	// A normal row must begin with a spinner and contain at least two frames.
	// A capped tail can begin mid-frame (for example "G◒ Clone…◐…◓…"); three
	// frame runes are strong enough repaint evidence even with that short junk
	// prefix. This keeps arbitrary repeated prose untouched.
	if lastByte < 0 || (count < 3 && (!isCapturedSpinnerLine(line) || count < 2)) {
		return line
	}
	return line[lastByte:]
}

// interpretSearchExitOne applies command-specific exit-code semantics before
// a Bash result is returned to the model. Claude Code does the same for grep,
// rg, and find: exit 1 can be an ordinary empty/partial search result rather
// than an execution failure. We intentionally support only conservative,
// read-only search chains; unknown shell syntax and mutating flags fail closed.
func interpretSearchExitOne(command, output string) (message string, handled bool) {
	program, ok := semanticSearchProgram(command)
	if !ok {
		return "", false
	}
	switch program {
	case "grep", "egrep", "fgrep", "rg":
		// grep-family exit 1 means no match, but a merged diagnostic from an
		// earlier pipeline stage is still valuable evidence and stays an error.
		if strings.TrimSpace(output) != "" {
			return "", false
		}
		return "No matches found", true
	case "find":
		trimmed := strings.TrimSpace(output)
		if trimmed == "" && findStderrSuppressed(command) {
			return "No matches found; some directories were inaccessible", true
		}
		if trimmed != "" && onlyFindAccessDiagnosticsAndResults(trimmed) {
			return "Search completed with partial access; some directories were inaccessible", true
		}
		// An unsuppressed find exit 1 can be a syntax/primary error. Preserve
		// its diagnostic instead of presenting a broken command as a partial
		// search result.
		return "", false
	default:
		return "", false
	}
}

func findStderrSuppressed(command string) bool {
	return stderrDevNullRE.MatchString(command)
}

// onlyFindAccessDiagnosticsAndResults accepts ordinary result lines mixed
// with the narrow access failures that make find return 1 while still yielding
// useful matches. Any other find-prefixed diagnostic (unknown predicate,
// malformed expression, missing path, and so on) stays a real error.
func onlyFindAccessDiagnosticsAndResults(output string) bool {
	sawAccessDiagnostic := false
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		low := strings.ToLower(trimmed)
		if strings.HasPrefix(low, "find:") {
			if strings.Contains(low, "permission denied") ||
				strings.Contains(low, "operation not permitted") ||
				strings.Contains(low, "access denied") {
				sawAccessDiagnostic = true
				continue
			}
			return false
		}
	}
	return sawAccessDiagnostic
}

// semanticSearchProgram returns the last program (the one whose status Bash
// reports) only when every command segment is a read-only search operation.
func semanticSearchProgram(command string) (string, bool) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" || strings.Contains(trimmed, "$(") || strings.Contains(trimmed, "<(") ||
		strings.Contains(trimmed, ">(") || strings.Contains(trimmed, "`") ||
		strings.ContainsAny(trimmed, "()") {
		return "", false
	}

	// Diagnostic suppression is harmless. Any other redirection makes this
	// classifier decline; it must never bless a search that also writes a file.
	withoutNull := strings.NewReplacer(
		"2>/dev/null", "", "2> /dev/null", "",
		"1>/dev/null", "", "1> /dev/null", "",
		">/dev/null", "", "> /dev/null", "",
	).Replace(trimmed)
	if strings.Contains(withoutNull, ">") || strings.Contains(withoutNull, "<") {
		return "", false
	}

	segments := splitOnAny(trimmed, []string{"\n", ";", "&&", "||", "|"})
	last := ""
	for _, segment := range segments {
		fields := strings.Fields(strings.TrimSpace(segment))
		if len(fields) == 0 {
			continue
		}
		first := 0
		for first < len(fields) && strings.Contains(fields[first], "=") && !strings.HasPrefix(fields[first], "=") {
			first++
		}
		if first >= len(fields) {
			return "", false
		}
		program := strings.Trim(fields[first], "'\"")
		if slash := strings.LastIndexByte(program, '/'); slash >= 0 {
			program = program[slash+1:]
		}
		program = strings.ToLower(program)
		switch program {
		case "grep", "egrep", "fgrep":
			// grep itself has no file-writing flag; shell redirects were
			// rejected above.
		case "rg":
			for _, field := range fields[first+1:] {
				low := strings.ToLower(strings.Trim(field, "'\""))
				if low == "--pre" || strings.HasPrefix(low, "--pre=") || low == "--generate" || strings.HasPrefix(low, "--generate=") {
					return "", false
				}
			}
		case "find":
			for _, field := range fields[first+1:] {
				switch strings.ToLower(strings.Trim(field, "'\"")) {
				case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fls", "-fprint", "-fprint0", "-fprintf":
					return "", false
				}
			}
		default:
			return "", false
		}
		last = program
	}
	return last, last != ""
}
