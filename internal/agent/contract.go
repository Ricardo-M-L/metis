package agent

// contract.go — risk-based verification enforcement at the agent-loop
// level. Runtime signals are more reliable than prompt-only thresholds, so
// the tracker combines mutation scope, implementation fan-out, observed
// validation, and high-impact commands. Two gates:
//
//   (1) mid-turn one-time SystemReminder when threshold first
//       crosses — heads-up: "you're doing substantial work; plan
//       to verify before you end."
//
//   (2) end-of-turn gate (up to 2 attempts) that holds the loop
//       and forces another turn if the model tries to end without
//       dispatching verify. Escape hatch: prefix the reply with
//       `OVERRIDE CONTRACT: <reason>` to release immediately; the
//       override is logged so the user sees it in the event stream.
//
// Disabled when METIS_CONTRACT_DISABLE=1 — useful for tests and
// one-off workflows where the contract gets in the way.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

const (
	contractIndependentRiskThreshold = 5
	contractMaxGateAttempts          = 2
	contractOverridePhrase           = "OVERRIDE CONTRACT:"
	contractDisableEnvVar            = "METIS_CONTRACT_DISABLE"
	contractVerifySubagentID         = "verify"
)

// contractTracker accumulates the side-effect signals one Loop.Run needs to
// decide whether the dispatch contract should fire. It lives on the Loop for
// iteration-to-iteration continuity and is reset at every Run boundary.
type contractTracker struct {
	mainWrites           int // Write + Edit + MultiEdit tool_use counts
	agentDispatches      int // Agent tool_use counts (any subagent_type)
	implementationAgents int
	mutatedFiles         map[string]struct{}
	validationObserved   bool
	highImpactAction     bool // destructive, publishing, deployment, or external mutation
	shellMutationAction  bool // file mutation performed through Bash rather than Write/Edit
	verifyDispatched     bool // current work epoch has a standalone subagent_type=verify dispatch
	reminderFired        bool // true iff the mid-turn reminder has already fired
	gateAttempts         int  // number of times end-of-turn gate held the loop

	// Phase B (2026-05-19): track the verify subagent's VERDICT line
	// so the end gate can refuse release on FAIL/PARTIAL/MISSING.
	// Caught on bench-iter10 where the model dispatched verify, got
	// back a FAIL verdict on parser tests, and end_turn'd anyway —
	// the pre-fix shouldGateEnd only checked "did you dispatch?",
	// not "did the verdict actually pass?". Empty string = no verify
	// result observed yet; "MISSING" = verify errored or didn't emit
	// an exact final VERDICT line; others are the literal verdict.
	lastVerifyVerdict   string
	verdictGateAttempts int // separate from gateAttempts so the verdict-gate budget doesn't share with the dispatch-gate budget
}

// observeToolUses tallies the batch the model just emitted. Counted
// at request-time so the tracker reflects intent even if a tool
// fails / is denied — the model still meant to do that work, and
// the verify obligation tracks intent not just success.
func (ct *contractTracker) observeToolUses(toolUses []llm.ContentBlock) {
	freshWorkInBatch := false
	verifyInBatch := false
	for _, tu := range toolUses {
		switch tu.ToolName {
		case "Write", "Edit", "MultiEdit", "NotebookEdit":
			freshWorkInBatch = true
			ct.invalidateVerification()
			ct.mainWrites++
			ct.validationObserved = false
			if path := mutationPath(tu.ToolInput); path != "" {
				if ct.mutatedFiles == nil {
					ct.mutatedFiles = make(map[string]struct{})
				}
				ct.mutatedFiles[path] = struct{}{}
			}
		case "Bash":
			command := toolInputString(tu.ToolInput, "command", "cmd")
			ct.validationObserved = ct.validationObserved || isValidationCommand(command)
			highImpact := isHighImpactCommand(command)
			shellMutation := isBashFileMutationCommand(command)
			if highImpact || shellMutation {
				freshWorkInBatch = true
				ct.invalidateVerification()
			}
			ct.highImpactAction = ct.highImpactAction || highImpact
			ct.shellMutationAction = ct.shellMutationAction || shellMutation
		case "Agent":
			ct.agentDispatches++
			st, _ := tu.ToolInput["subagent_type"].(string)
			if st == contractVerifySubagentID {
				verifyInBatch = true
				ct.verifyDispatched = true
				ct.lastVerifyVerdict = ""
			} else if isImplementationAgent(st) {
				freshWorkInBatch = true
				ct.invalidateVerification()
				ct.implementationAgents++
				ct.validationObserved = false
			}
		}
	}
	// Calls in one model tool batch may execute concurrently. A verifier emitted
	// beside fresh mutations/implementation cannot attest that those siblings
	// finished first, regardless of their array order. Require a later batch.
	if freshWorkInBatch && verifyInBatch {
		ct.verifyDispatched = false
		ct.lastVerifyVerdict = ""
	}
}

// invalidateVerification drops evidence that predates newly observed work.
// A verifier can only attest to the mutations that existed when it ran; an
// Edit, high-impact command, Bash mutation, or implementation sub-agent
// afterwards creates a new verification epoch and cannot inherit an older PASS.
func (ct *contractTracker) invalidateVerification() {
	if !ct.verifyDispatched && ct.lastVerifyVerdict == "" {
		return
	}
	ct.verifyDispatched = false
	ct.lastVerifyVerdict = ""
	ct.gateAttempts = 0
	ct.verdictGateAttempts = 0
}

func mutationPath(input map[string]any) string {
	return toolInputString(input, "file_path", "path", "notebook_path")
}

func toolInputString(input map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := input[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isImplementationAgent(subagentType string) bool {
	switch strings.ToLower(strings.TrimSpace(subagentType)) {
	case "explore", "plan", contractVerifySubagentID:
		return false
	default:
		// Contract tracking does not receive resolved profile capabilities, so
		// only the narrow built-in read-only/verification slugs are exempt.
		// Profiles with Bash (including reviewers/debuggers), creator, teammate,
		// general, and unknown/custom profiles are conservatively implementation.
		// A project/user override reusing an exempt slug remains an architectural
		// limitation until the resolved tool palette is passed into the tracker.
		return true
	}
}

func isValidationCommand(command string) bool {
	lower := strings.ToLower(command)
	for _, marker := range []string{
		"go test", "go vet", "cargo test", "pytest", "npm test",
		"npm run test", "pnpm test", "yarn test", "bun test",
		"mvn test", "gradle test", "./gradlew test", "make test",
		"golangci-lint", "npm run lint", "pnpm lint", "yarn lint",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isHighImpactCommand(command string) bool {
	lower := strings.ToLower(command)
	for _, marker := range []string{
		"rm -rf", "gh pr create", "npm publish",
		"goreleaser", "drop table", "drop database", "truncate table",
		"terraform apply", "kubectl apply", "helm upgrade",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// Unlike the legacy high-impact markers above, recognize git push only
	// when it is the executed command. This covers global options such as
	// `git -C worktree push` without treating quoted/example text as an action.
	for _, segment := range splitShellCommandSegments(command) {
		fields := splitShellWords(segment)
		commandIndex := shellCommandIndex(fields)
		if commandIndex < 0 {
			continue
		}
		name := strings.ToLower(filepath.Base(strings.Trim(fields[commandIndex], "'\"()")))
		if name != "git" {
			continue
		}
		subcommand, _ := gitSubcommand(fields[commandIndex+1:])
		if subcommand == "push" {
			return true
		}
	}
	return false
}

var (
	pythonPathWritePattern = regexp.MustCompile(`(?is)\.(?:write_text|write_bytes)\s*\(`)
	pythonWriteOpenPattern = regexp.MustCompile(`(?is)(?:\bopen\s*\([^,\n]+,\s*|\.open\s*\(\s*)(?:mode\s*=\s*)?["'](?:[wax][bt+]*|r[bt]*\+)["']`)
	nodeBareWritePattern   = regexp.MustCompile(`(?i)\b(?:writeFile|appendFile)(?:Sync)?\s*\(`)
)

// isBashFileMutationCommand recognizes common ways a Bash invocation writes
// repository files. These commands bypass the structured Write/Edit tools, so
// conservatively treat them as independent-verification risk.
func isBashFileMutationCommand(command string) bool {
	return isBashFileMutationCommandDepth(command, 0)
}

func isBashFileMutationCommandDepth(command string, depth int) bool {
	if depth > 4 {
		return false
	}
	for _, payload := range shellCommandSubstitutions(command) {
		if isBashFileMutationCommandDepth(payload, depth+1) {
			return true
		}
	}
	if hasShellOutputRedirection(command) {
		return true
	}

	pythonHeredoc := false
	for _, segment := range splitShellCommandSegments(command) {
		fields := splitShellWords(segment)
		commandIndex := shellCommandIndex(fields)
		if commandIndex < 0 {
			continue
		}
		name := strings.ToLower(filepath.Base(strings.Trim(fields[commandIndex], "'\"()")))
		switch name {
		case "tee":
			if hasNonOptionOperand(fields[commandIndex+1:]) {
				return true
			}
		case "touch", "cp", "mv", "install", "mkdir", "ln", "truncate":
			if hasNonOptionOperand(fields[commandIndex+1:]) {
				return true
			}
		case "rm", "rmdir", "unlink":
			args := fields[commandIndex+1:]
			if !hasAnyShellOption(args, "--help", "--version") && hasNonOptionOperand(args) {
				return true
			}
		case "patch":
			if !hasAnyShellOption(fields[commandIndex+1:], "--dry-run", "--help", "--version") {
				return true
			}
		case "dd":
			if hasShellArgumentPrefix(fields[commandIndex+1:], "of=") {
				return true
			}
		case "rsync":
			if !hasAnyShellOption(fields[commandIndex+1:], "-n", "--dry-run") && hasNonOptionOperand(fields[commandIndex+1:]) {
				return true
			}
		case "gofmt":
			args := fields[commandIndex+1:]
			if hasAnyShellOption(args, "-w") && hasNonOptionOperand(args) {
				return true
			}
		case "prettier":
			args := fields[commandIndex+1:]
			if hasAnyShellOption(args, "-w", "--write") && hasNonOptionOperand(args) {
				return true
			}
		case "eslint":
			args := fields[commandIndex+1:]
			if hasAnyShellOption(args, "--fix") && hasNonOptionOperand(args) {
				return true
			}
		case "py":
			if pythonCommandWrites(segment) {
				return true
			}
			pythonHeredoc = pythonHeredoc || strings.Contains(segment, "<<")
		case "node", "nodejs":
			if nodeCommandWrites(segment) {
				return true
			}
		case "bash", "sh", "zsh", "dash", "ksh":
			if payload, ok := shellDashCPayload(fields[commandIndex+1:]); ok && isBashFileMutationCommandDepth(payload, depth+1) {
				return true
			}
		case "ruby":
			if strings.Contains(strings.ToLower(segment), "file.write(") {
				return true
			}
		case "php":
			if strings.Contains(strings.ToLower(segment), "file_put_contents(") {
				return true
			}
		case "apply_patch":
			return true
		case "git":
			subcommand, rest := gitSubcommand(fields[commandIndex+1:])
			switch subcommand {
			case "apply":
				if hasAnyShellOption(rest, "--apply") || !hasAnyShellOption(rest, "--check", "--stat", "--numstat", "--summary") {
					return true
				}
			case "rm":
				if !hasShortShellFlag(rest, 'n') && !hasAnyShellOption(rest, "--dry-run", "--help") && len(rest) > 0 {
					return true
				}
			case "restore":
				if !hasAnyShellOption(rest, "--help") && len(rest) > 0 {
					return true
				}
			case "checkout":
				if !hasAnyShellOption(rest, "-h", "--help") && len(rest) > 0 {
					return true
				}
			case "reset":
				if !hasAnyShellOption(rest, "-h", "--help") {
					return true
				}
			case "clean":
				if !hasShortShellFlag(rest, 'n') && !hasAnyShellOption(rest, "--dry-run", "-h", "--help") {
					return true
				}
			case "cherry-pick", "merge", "switch":
				if !hasAnyShellOption(rest, "-h", "--help") {
					return true
				}
			case "rebase":
				if !hasAnyShellOption(rest, "-h", "--help", "--show-current-patch") {
					return true
				}
			}
		case "sed", "gsed":
			if sedEditsInPlace(fields[commandIndex+1:]) {
				return true
			}
		case "perl":
			if perlEditsInPlace(fields[commandIndex+1:]) {
				return true
			}
		default:
			if isPythonInterpreter(name) {
				if pythonCommandWrites(segment) {
					return true
				}
				pythonHeredoc = pythonHeredoc || strings.Contains(segment, "<<")
			}
		}
	}
	if pythonHeredoc && pythonCommandWrites(command) {
		return true
	}
	return false
}

func splitShellWords(command string) []string {
	fields := make([]string, 0, 8)
	var current strings.Builder
	inSingle, inDouble, escaped := false, false, false
	flush := func() {
		if current.Len() == 0 {
			return
		}
		fields = append(fields, current.String())
		current.Reset()
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if escaped {
			current.WriteByte(ch)
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			} else {
				current.WriteByte(ch)
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			} else {
				current.WriteByte(ch)
			}
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case ' ', '\t':
			flush()
		default:
			current.WriteByte(ch)
		}
	}
	flush()
	return fields
}

func splitShellCommandSegments(command string) []string {
	segments := make([]string, 0, 4)
	start := 0
	inSingle, inDouble, escaped := false, false, false
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case ';', '|', '&', '\n', '\r':
			if segment := strings.TrimSpace(command[start:i]); segment != "" {
				segments = append(segments, segment)
			}
			start = i + 1
		}
	}
	if segment := strings.TrimSpace(command[start:]); segment != "" {
		segments = append(segments, segment)
	}
	return segments
}

// shellCommandSubstitutions returns executable payloads from $(...) and
// backtick substitutions. Substitutions remain active inside double quotes but
// are literal inside single quotes. Arithmetic expansion is intentionally
// skipped: `$((2 > 1))` is an expression, not a nested shell command.
func shellCommandSubstitutions(command string) []string {
	payloads := make([]string, 0, 2)
	inSingle, inDouble, escaped := false, false, false
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = true
			continue
		}
		if ch == '"' {
			inDouble = !inDouble
			continue
		}
		if ch == '$' && i+1 < len(command) && command[i+1] == '(' {
			if i+2 < len(command) && command[i+2] == '(' {
				continue
			}
			if end := matchingShellCommandParen(command, i+1); end >= 0 {
				payloads = append(payloads, command[i+2:end])
				i = end
			}
			continue
		}
		if ch == '`' {
			if end := matchingBacktick(command, i+1); end >= 0 {
				payloads = append(payloads, command[i+1:end])
				i = end
			}
		}
	}
	return payloads
}

func matchingShellCommandParen(command string, opening int) int {
	depth := 1
	inSingle, inDouble, escaped := false, false, false
	for i := opening + 1; i < len(command); i++ {
		ch := command[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func matchingBacktick(command string, start int) int {
	escaped := false
	for i := start; i < len(command); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch command[i] {
		case '\\':
			escaped = true
		case '`':
			return i
		}
	}
	return -1
}

func shellCommandIndex(fields []string) int {
	for i := 0; i < len(fields); {
		field := strings.Trim(fields[i], "'\"()")
		if field == "" || strings.Contains(field, "=") && !strings.HasPrefix(field, "=") {
			i++
			continue
		}
		lower := strings.ToLower(filepath.Base(field))
		switch lower {
		case "sudo", "doas":
			i++
			for i < len(fields) {
				option := strings.Trim(fields[i], "'\"()")
				if option == "--" {
					i++
					break
				}
				if !strings.HasPrefix(option, "-") || option == "-" {
					break
				}
				name, _, hasInlineValue := strings.Cut(option, "=")
				i++
				if !hasInlineValue && shellWrapperOptionTakesValue(name) && i < len(fields) {
					i++
				}
			}
			continue
		case "command", "builtin", "exec", "env":
			i++
			for i < len(fields) {
				arg := strings.Trim(fields[i], "'\"()")
				if arg == "--" {
					i++
					break
				}
				if lower == "env" && strings.Contains(arg, "=") && !strings.HasPrefix(arg, "=") {
					i++
					continue
				}
				if !strings.HasPrefix(arg, "-") || arg == "-" {
					break
				}
				i++
			}
			continue
		case "then", "do", "if":
			i++
			continue
		}
		if strings.HasPrefix(field, "-") {
			i++
			continue
		}
		return i
	}
	return -1
}

func shellWrapperOptionTakesValue(option string) bool {
	switch option {
	case "-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-r", "--role", "-t", "--type", "-C", "--chdir":
		return true
	default:
		return false
	}
}

func gitSubcommand(fields []string) (string, []string) {
	for i := 0; i < len(fields); i++ {
		field := strings.Trim(fields[i], "'\"()")
		name, _, _ := strings.Cut(field, "=")
		switch name {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace":
			if !strings.Contains(field, "=") {
				i++
			}
			continue
		}
		if field == "" || strings.HasPrefix(field, "-") {
			continue
		}
		return strings.ToLower(field), fields[i+1:]
	}
	return "", nil
}

func pythonCommandWrites(script string) bool {
	lower := strings.ToLower(script)
	if pythonPathWritePattern.MatchString(script) || pythonWriteOpenPattern.MatchString(script) {
		return true
	}
	for _, marker := range []string{
		"os.rename(", "os.replace(", "os.remove(", "os.unlink(",
		"shutil.copy(", "shutil.copy2(", "shutil.copyfile(", "shutil.move(",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isPythonInterpreter(name string) bool {
	for _, prefix := range []string{"python", "pypy"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if suffix == "" {
			return true
		}
		valid := true
		for _, r := range suffix {
			if (r < '0' || r > '9') && r != '.' {
				valid = false
				break
			}
		}
		if valid {
			return true
		}
	}
	return false
}

func shellDashCPayload(args []string) (string, bool) {
	for i, arg := range args {
		if arg == "-c" || strings.HasPrefix(arg, "-") && strings.Contains(arg[1:], "c") {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if arg == "--" {
			return "", false
		}
	}
	return "", false
}

func nodeCommandWrites(script string) bool {
	lower := strings.ToLower(script)
	for _, marker := range []string{
		"fs.writefile(", "fs.writefilesync(",
		"fs.promises.writefile(", ".writefile(", ".writefilesync(",
		"fs.appendfile(", "fs.appendfilesync(",
		"fs.promises.appendfile(", ".appendfile(", ".appendfilesync(",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if !nodeBareWritePattern.MatchString(script) {
		return false
	}
	for _, moduleMarker := range []string{
		"fs/promises", "node:fs", `require("fs")`, "require('fs')", `from "fs"`, "from 'fs'",
	} {
		if strings.Contains(lower, moduleMarker) {
			return true
		}
	}
	return false
}

func hasNonOptionOperand(fields []string) bool {
	afterOptions := false
	for _, field := range fields {
		field = strings.Trim(field, "'\"()")
		if field == "--" {
			afterOptions = true
			continue
		}
		if field == "" || !afterOptions && strings.HasPrefix(field, "-") {
			continue
		}
		if field != "/dev/null" {
			return true
		}
	}
	return false
}

func hasAnyShellOption(fields []string, options ...string) bool {
	for _, field := range fields {
		field = strings.Trim(field, "'\"()")
		name, _, _ := strings.Cut(field, "=")
		for _, option := range options {
			if name == option {
				return true
			}
		}
	}
	return false
}

func hasShortShellFlag(fields []string, want byte) bool {
	for _, field := range fields {
		field = strings.Trim(field, "'\"()")
		if len(field) > 1 && field[0] == '-' && field[1] != '-' && strings.ContainsRune(field[1:], rune(want)) {
			return true
		}
	}
	return false
}

func hasShellArgumentPrefix(fields []string, prefix string) bool {
	for _, field := range fields {
		if strings.HasPrefix(strings.Trim(field, "'\"()"), prefix) {
			return true
		}
	}
	return false
}

func sedEditsInPlace(args []string) bool {
	for _, arg := range args {
		arg = strings.Trim(arg, "'\"")
		if arg == "--" {
			return false
		}
		if arg == "--in-place" || strings.HasPrefix(arg, "--in-place=") {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "i") {
			return true
		}
	}
	return false
}

func perlEditsInPlace(args []string) bool {
	for _, arg := range args {
		arg = strings.Trim(arg, "'\"")
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg[1:], "i") {
			return true
		}
	}
	return false
}

func hasShellOutputRedirection(command string) bool {
	inSingle, inDouble, escaped := false, false, false
	arithmeticDepth := 0
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			}
			continue
		}
		if arithmeticDepth > 0 {
			switch ch {
			case '(':
				arithmeticDepth++
			case ')':
				arithmeticDepth--
			}
			continue
		}
		if ch == '(' && i+1 < len(command) && command[i+1] == '(' {
			// `(( expression ))` and `$(( expression ))` use > as a
			// comparison operator, not an output redirection.
			arithmeticDepth = 2
			i++
			continue
		}
		switch ch {
		case '\\':
			escaped = true
			continue
		case '\'':
			inSingle = true
			continue
		case '"':
			inDouble = true
			continue
		case '>':
		default:
			continue
		}
		if i+1 < len(command) && command[i+1] == '=' {
			continue
		}
		if i > 0 && (command[i-1] == '=' || command[i-1] == '-') {
			continue
		}
		target := i + 1
		if target < len(command) && command[target] == '>' {
			target++
		}
		for target < len(command) && (command[target] == ' ' || command[target] == '\t') {
			target++
		}
		// File-descriptor duplication (2>&1) and /dev/null do not write a
		// repository file and must not turn ordinary validation into fresh work.
		if target < len(command) && command[target] == '&' {
			target++
			for target < len(command) && (command[target] == ' ' || command[target] == '\t') {
				target++
			}
			end := target
			for end < len(command) && !strings.ContainsRune(" \t;|&\n\r", rune(command[end])) {
				end++
			}
			fdTarget := command[target:end]
			if fdTarget == "-" || fdTarget != "" && allASCIIDigits(fdTarget) {
				continue
			}
		}
		if strings.HasPrefix(command[target:], "/dev/null") {
			continue
		}
		return true
	}
	return false
}

func allASCIIDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

// observeToolResults pairs the just-completed tool_results with
// their originating toolUses, extracts the VERDICT line from any
// verify-subagent result, and stashes it on the tracker for the
// end-gate to consult. Phase B (2026-05-19) — see lastVerifyVerdict
// field comment for the bench-iter10 case this catches.
//
// VERDICT extraction:
//   - Match only an exact `VERDICT: PASS|FAIL|PARTIAL` final non-empty line.
//   - If verify errored or its final non-empty line is not an exact verdict,
//     record "MISSING" so the gate holds (no valid verifier evidence).
//   - Only set when subagent_type was exactly "verify"; other
//     subagent types are out of scope for this gate.
func (ct *contractTracker) observeToolResults(toolUses, results []llm.ContentBlock) {
	batchVerdict := ""
	for i, tu := range toolUses {
		if tu.Type != "tool_use" || tu.ToolName != "Agent" {
			continue
		}
		st, _ := tu.ToolInput["subagent_type"].(string)
		if st != contractVerifySubagentID {
			continue
		}
		verdict := "MISSING"
		if i < len(results) && !results[i].IsError {
			verdict = extractVerdict(results[i].ToolResult)
		} else if i < len(results) {
			// A failed/denied Agent call cannot attest to the work, even when
			// an error payload happens to contain a PASS-looking string.
		}
		// A batch is only PASS when every verifier in it passed. Keep the
		// first non-PASS outcome so a later sibling cannot overwrite it.
		if batchVerdict == "" || batchVerdict == "PASS" {
			batchVerdict = verdict
		}
	}
	if batchVerdict != "" {
		ct.lastVerifyVerdict = batchVerdict
	}
}

// extractVerdict scans subagent body for the mandated VERDICT line.
// Returns "PASS", "FAIL", "PARTIAL", or "MISSING" (no VERDICT line
// found). The match is case-sensitive on "VERDICT:" per profile;
// the verdict word itself is also case-sensitive — the profile
// specifies upper-case and the verifier subagent must honor it.
//
// Only the final non-empty logical line is considered. It must equal the
// protocol line exactly; quoted, embedded, suffixed, or prefix-matching text
// is not verifier evidence.
func extractVerdict(body string) string {
	lines := strings.Split(body, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSuffix(lines[i], "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		switch line {
		case "VERDICT: PASS":
			return "PASS"
		case "VERDICT: FAIL":
			return "FAIL"
		case "VERDICT: PARTIAL":
			return "PARTIAL"
		default:
			return "MISSING"
		}
	}
	return "MISSING"
}

// contractDisabled returns true when METIS_CONTRACT_DISABLE=1 is
// set in env. Read each call so test rigs can flip mid-run.
func contractDisabled() bool {
	return os.Getenv(contractDisableEnvVar) == "1"
}

// riskScore combines mutation volume, distinct-file scope, implementation
// fan-out, observed validation, high-impact external actions, and file changes
// performed through Bash. Runtime owns this policy so the base prompt only
// needs to state the invariant.
func (ct *contractTracker) riskScore() int {
	if ct.highImpactAction || ct.shellMutationAction {
		return contractIndependentRiskThreshold
	}
	score := minInt(ct.mainWrites, 3)
	score += minInt(len(ct.mutatedFiles), 3)
	score += minInt(ct.implementationAgents*2, 6)
	if ct.validationObserved && score > 0 {
		score--
	}
	return score
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// thresholdMet reports whether this run warrants independent verification.
func (ct *contractTracker) thresholdMet() bool {
	return ct.riskScore() >= contractIndependentRiskThreshold
}

// shouldFireMidTurnReminder returns the reminder body when the
// threshold was just crossed for the first time, the model hasn't
// already dispatched verify, and we haven't fired before. Empty
// otherwise. Marks reminderFired on a non-empty return so the
// caller doesn't need to track that bit separately.
func (ct *contractTracker) shouldFireMidTurnReminder() string {
	if contractDisabled() {
		return ""
	}
	if !ct.thresholdMet() || ct.verifyDispatched || ct.reminderFired {
		return ""
	}
	ct.reminderFired = true
	return fmt.Sprintf(
		"<system-reminder>\n"+
			"CONTRACT REMINDER (heads-up). Runtime risk is now %d/%d "+
			"(%d mutation(s) across %d file(s), %d implementation agent(s), "+
			"validation observed: %t, high-impact action: %t, "+
			"Bash file mutation: %t). Non-trivial "+
			"implementation MUST end with a verify sub-agent that returns "+
			"`VERDICT: PASS`. Plan your remaining moves so the work ends "+
			"with:\n"+
			"    Agent({subagent_type: \"verify\", prompt: \"<what to check "+
			"+ the VERDICT line you expect back>\"})\n"+
			"Running `go build` / `npm test` / etc. yourself does NOT "+
			"substitute. Only the verifier issues a verdict.\n"+
			"</system-reminder>",
		ct.riskScore(), contractIndependentRiskThreshold,
		ct.mainWrites, len(ct.mutatedFiles), ct.implementationAgents,
		ct.validationObserved, ct.highImpactAction, ct.shellMutationAction,
	)
}

// shouldGateEnd runs when the model emits an assistant message with
// stop_reason != tool_use (it's trying to end the turn). Returns
// non-empty body when the gate should HOLD the loop — caller
// injects the body as a user message and continues iterating
// instead of returning. Returns empty when the gate decides the
// model is allowed to end.
//
// Gate releases when ANY of:
//   - threshold not met (small task; nothing to verify), unless a dispatched
//     verifier has already returned a non-PASS verdict
//   - verify already dispatched
//   - we've already gated MaxGateAttempts times (don't infinite-loop)
//   - assistantText contains the OVERRIDE CONTRACT: escape phrase
//   - env override disables the contract entirely
//
// Increments gateAttempts on a non-empty return so the cap holds.
func (ct *contractTracker) shouldGateEnd(assistantText string) string {
	if contractDisabled() {
		return ""
	}
	// Override applies to BOTH the dispatch-gate (no verify yet) and
	// the verdict-gate (verify ran but didn't pass) — it's an escape
	// hatch for the whole contract.
	if strings.Contains(assistantText, contractOverridePhrase) {
		return ""
	}

	// Phase B: verdict gate. If verify was dispatched and we observed
	// a verdict, only PASS releases. FAIL / PARTIAL / MISSING all
	// hold — model must address findings and re-verify. Separate
	// attempt budget so a model that keeps getting FAIL doesn't burn
	// its dispatch-gate budget too.
	if ct.verifyDispatched && ct.lastVerifyVerdict != "" && ct.lastVerifyVerdict != "PASS" {
		if ct.verdictGateAttempts >= contractMaxGateAttempts {
			return ""
		}
		ct.verdictGateAttempts++
		return fmt.Sprintf(
			"<system-reminder>\n"+
				"VERDICT GATE — HALT (attempt %d of %d). Your verify "+
				"subagent returned VERDICT: %s. Per the contract, end "+
				"is not allowed until VERDICT: PASS. Pick one:\n\n"+
				"  (a) Address the verifier's findings (read its report, "+
				"fix the failing tests / missing pieces), then RE-DISPATCH "+
				"Agent({subagent_type: \"verify\", ...}) and wait for PASS.\n\n"+
				"  (b) Override by writing this exact phrase as a line in "+
				"your next reply:\n"+
				"          %s <one-line reason why a non-PASS verdict is OK here>\n"+
				"      (Logged for audit. Genuine cases: the verifier was "+
				"wrong about the scope, or PARTIAL is the intended end "+
				"state for this task. Don't override just because fixing "+
				"is hard.)\n\n"+
				"After attempt %d/%d, the loop releases with a warning so "+
				"we don't burn tokens infinitely.\n"+
				"</system-reminder>",
			ct.verdictGateAttempts, contractMaxGateAttempts,
			ct.lastVerifyVerdict,
			contractOverridePhrase,
			ct.verdictGateAttempts, contractMaxGateAttempts,
		)
	}
	if !ct.thresholdMet() {
		return ""
	}

	// Original dispatch gate: verify never dispatched.
	if ct.verifyDispatched {
		return ""
	}
	if ct.gateAttempts >= contractMaxGateAttempts {
		return ""
	}
	ct.gateAttempts++
	return fmt.Sprintf(
		"<system-reminder>\n"+
			"CONTRACT GATE — HALT (attempt %d of %d). Runtime risk is %d/%d "+
			"after %d mutation(s) across %d file(s), %d implementation "+
			"agent(s), validation observed=%t, high-impact action=%t, "+
			"Bash file mutation=%t. You "+
			"tried to end the turn without spawning a verifier. Per the "+
			"contract, end is not allowed yet. Pick one:\n\n"+
			"  (a) Spawn the verifier now and wait for VERDICT: PASS:\n"+
			"      Agent({subagent_type: \"verify\",\n"+
			"             prompt: \"<what to check + expected VERDICT line>\"})\n\n"+
			"  (b) Override the contract by writing this exact phrase as a "+
			"line in your next reply:\n"+
			"          %s <one-line reason>\n"+
			"      (Logged for audit. Use only when verification genuinely "+
			"does not apply — pure refactors, dry runs, documentation-only "+
			"changes, etc.)\n\n"+
			"After this attempt %d/%d, if you still try to end without "+
			"(a) or (b), the loop releases with a warning so we don't burn "+
			"tokens infinitely.\n"+
			"</system-reminder>",
		ct.gateAttempts, contractMaxGateAttempts,
		ct.riskScore(), contractIndependentRiskThreshold,
		ct.mainWrites, len(ct.mutatedFiles), ct.implementationAgents,
		ct.validationObserved, ct.highImpactAction, ct.shellMutationAction,
		contractOverridePhrase,
		ct.gateAttempts, contractMaxGateAttempts,
	)
}

// wasOverridden reports whether the model used the escape phrase
// in its assistant text. Caller uses this to log a one-line event
// so the user sees "model overrode contract because X" in the
// transcript rather than a silent release.
func (ct *contractTracker) wasOverridden(assistantText string) bool {
	return strings.Contains(assistantText, contractOverridePhrase)
}

// overrideBypassesGate reports whether an explicit override released a gate
// that would otherwise have held this turn. It covers both contract phases:
// no verifier was dispatched for threshold-crossing work, or a verifier ran
// but returned a non-PASS verdict. Keeping this decision beside the gate state
// prevents the Loop's audit event from silently missing verdict overrides.
func (ct *contractTracker) overrideBypassesGate(assistantText string) bool {
	if !ct.wasOverridden(assistantText) {
		return false
	}
	if ct.verifyDispatched && ct.lastVerifyVerdict != "" && ct.lastVerifyVerdict != "PASS" {
		return true
	}
	return ct.thresholdMet() && !ct.verifyDispatched
}

// reset clears all counters. Called at each Loop.Run boundary and from the
// explicit reset paths so every user turn/session starts clean. Done as a
// pointer-method so the caller doesn't accidentally copy a fresh
// zero value onto the Loop and lose any other state we add later.
func (ct *contractTracker) reset() {
	*ct = contractTracker{}
}

// assistantText joins all text blocks in an assistant message into
// one string. Used by the end-of-turn gate to look for the
// OVERRIDE CONTRACT: escape phrase. Tool-use blocks contribute
// nothing here; only the model's user-facing prose counts.
func assistantText(content []llm.ContentBlock) string {
	var b strings.Builder
	for _, blk := range content {
		if blk.Type == "text" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
