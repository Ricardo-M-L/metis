// Package shellguard rejects raw process-termination commands emitted by the
// model. Metis-owned background jobs must be stopped through BashKill(job_id),
// which can only address processes registered in the current jobs.Registry.
package shellguard

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const maxNestedShellDepth = 8

var ErrProcessTermination = errors.New("raw process termination commands are disabled; use BashKill(job_id) for Metis-owned background jobs")

// Check parses command as Bash and rejects kill-family executables in command
// position, including pipelines, command substitutions, wrappers, xargs /
// parallel targets, and static shell -c payloads. Text such as
// `echo "kill -9"` or `rg "pkill"` remains ordinary data and is allowed.
func Check(command string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	return check(command, 0)
}

func check(command string, depth int) error {
	if depth > maxNestedShellDepth {
		return fmt.Errorf("%w (nested shell command exceeds inspection depth)", ErrProcessTermination)
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		// Execution may use zsh, cmd.exe, or PowerShell even though the common
		// inspection grammar is Bash. If that grammar cannot account for the
		// complete command, allowing it would turn a syntax difference into a
		// process-termination bypass. Fail closed and direct the model to the
		// scoped BashKill tool instead.
		return blocked("shell syntax cannot be inspected safely")
	}
	var guardErr error
	syntax.Walk(file, func(node syntax.Node) bool {
		if guardErr != nil {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		guardErr = inspectCall(call, depth)
		return guardErr == nil
	})
	return guardErr
}

func inspectCall(call *syntax.CallExpr, depth int) error {
	words := make([]shellWord, 0, len(call.Args))
	for _, word := range call.Args {
		value, ok := staticWord(word)
		words = append(words, shellWord{value: value, static: ok})
	}
	if len(words) == 0 {
		return nil
	}
	return inspectWords(words, depth)
}

type shellWord struct {
	value  string
	static bool
}

func inspectWords(words []shellWord, depth int) error {
	if len(words) == 0 {
		return nil
	}
	if !words[0].static || strings.TrimSpace(words[0].value) == "" {
		return blocked("dynamic command position cannot be inspected safely")
	}
	cmd := commandBase(words[0].value)
	if isKillCommand(cmd) {
		return blocked("blocked " + cmd)
	}

	switch cmd {
	case "command":
		if len(words) >= 2 && words[1].static && (words[1].value == "-v" || words[1].value == "-V") {
			return nil
		}
		target, err := commandTarget(words[1:], map[string]bool{"-p": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "builtin":
		target, err := commandTarget(words[1:], nil)
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "exec":
		target, err := commandTarget(words[1:], map[string]bool{"-a": true, "-c": false, "-l": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "env":
		return inspectEnv(words[1:], depth)
	case "sudo", "doas":
		target, err := commandTarget(words[1:], sudoOptions)
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "nohup":
		target, err := commandTarget(words[1:], map[string]bool{"--help": false, "--version": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "nice":
		return inspectNice(words[1:], depth)
	case "timeout":
		return inspectTimeout(words[1:], depth)
	case "setsid":
		target, err := commandTarget(words[1:], map[string]bool{"-c": false, "--ctty": false, "-f": false, "--fork": false, "-w": false, "--wait": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "xargs":
		return inspectXargs(words[1:], depth)
	case "parallel":
		target, err := commandTarget(words[1:], parallelOptions)
		if err != nil {
			return err
		}
		if len(target) == 0 {
			return nil
		}
		// GNU parallel commonly receives the entire target as one quoted word.
		if target[0].static && strings.ContainsAny(target[0].value, " \t;|&()") {
			return check(target[0].value, depth+1)
		}
		return inspectWords(target, depth)
	case "find":
		return inspectFind(words[1:], depth)
	case "busybox", "toybox":
		target, err := commandTarget(words[1:], map[string]bool{"--install": false, "--list": false, "--list-full": false, "--show": false})
		if err != nil {
			return err
		}
		return inspectWords(target, depth)
	case "eval":
		payload, err := joinStatic(words[1:])
		if err != nil {
			return err
		}
		return check(payload, depth+1)
	case "sh", "bash", "zsh", "dash", "ksh":
		payload, found, err := staticShellPayload(words[1:])
		if err != nil {
			return err
		}
		if found {
			return check(payload, depth+1)
		}
	case "cmd", "cmd.exe":
		return inspectCmd(words[1:], depth)
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return inspectPowerShell(words[1:], depth)
	}
	return nil
}

func blocked(reason string) error {
	return fmt.Errorf("%w (%s)", ErrProcessTermination, reason)
}

func staticWord(word *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescapeShellLiteral(p.Value, false))
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				switch q := inner.(type) {
				case *syntax.Lit:
					b.WriteString(unescapeShellLiteral(q.Value, true))
				case *syntax.SglQuoted:
					b.WriteString(q.Value)
				default:
					return "", false
				}
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// mvdan preserves shell escapes in Lit.Value. Resolve only the escapes that
// the shell removes at execution time; single-quoted words bypass this helper
// and therefore keep backslashes as ordinary data.
func unescapeShellLiteral(value string, doubleQuoted bool) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			b.WriteByte(value[i])
			continue
		}
		next := value[i+1]
		if doubleQuoted && !strings.ContainsRune(`$\"`+"\n", rune(next)) {
			b.WriteByte(value[i])
			continue
		}
		if next != '\n' {
			b.WriteByte(next)
		}
		i++
	}
	return b.String()
}

func commandBase(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return strings.ToLower(filepath.Base(raw))
}

func isKillCommand(cmd string) bool {
	switch cmd {
	case "kill", "pkill", "killall", "taskkill", "taskkill.exe", "stop-process", "spps":
		return true
	default:
		return false
	}
}

func commandTarget(words []shellWord, options map[string]bool) ([]shellWord, error) {
	for len(words) > 0 {
		if !words[0].static {
			return nil, blocked("dynamic wrapper option or command cannot be inspected safely")
		}
		word := words[0].value
		if word == "--" {
			return words[1:], nil
		}
		if !strings.HasPrefix(word, "-") || word == "-" {
			return words, nil
		}
		name, hasInlineValue := optionName(word)
		needsValue, known := options[name]
		if !known {
			return nil, blocked("unknown wrapper option " + word + " makes the command target ambiguous")
		}
		words = words[1:]
		if needsValue && !hasInlineValue {
			if len(words) == 0 || !words[0].static {
				return nil, blocked("wrapper option " + name + " has an uninspectable value")
			}
			words = words[1:]
		}
	}
	return nil, nil
}

func optionName(word string) (name string, hasInlineValue bool) {
	if i := strings.IndexByte(word, '='); i >= 0 {
		return word[:i], true
	}
	return word, false
}

func inspectEnv(words []shellWord, depth int) error {
	for len(words) > 0 {
		if !words[0].static {
			return blocked("dynamic env option, assignment, or command cannot be inspected safely")
		}
		word := words[0].value
		if word == "--" {
			return inspectWords(words[1:], depth)
		}
		if strings.Contains(word, "=") && !strings.HasPrefix(word, "-") {
			words = words[1:]
			continue
		}
		name, inline := optionName(word)
		switch name {
		case "-S", "--split-string":
			var payload string
			if inline {
				payload = word[strings.IndexByte(word, '=')+1:]
				words = words[1:]
			} else {
				if len(words) < 2 || !words[1].static {
					return blocked("env split-string payload cannot be inspected safely")
				}
				payload = words[1].value
				words = words[2:]
			}
			if err := check(payload, depth+1); err != nil {
				return err
			}
			continue
		case "-u", "--unset", "-C", "--chdir":
			words = words[1:]
			if !inline {
				if len(words) == 0 || !words[0].static {
					return blocked("env option " + name + " has an uninspectable value")
				}
				words = words[1:]
			}
			continue
		case "-i", "--ignore-environment", "-0", "--null", "-v", "--debug":
			words = words[1:]
			continue
		}
		if strings.HasPrefix(word, "-") && word != "-" {
			return blocked("unknown env option " + word + " makes the command target ambiguous")
		}
		return inspectWords(words, depth)
	}
	return nil
}

func inspectNice(words []shellWord, depth int) error {
	for len(words) > 0 {
		if !words[0].static {
			return blocked("dynamic nice option or command cannot be inspected safely")
		}
		word := words[0].value
		if word == "--" {
			return inspectWords(words[1:], depth)
		}
		if word == "-n" || word == "--adjustment" {
			if len(words) < 2 || !words[1].static {
				return blocked("nice adjustment cannot be inspected safely")
			}
			words = words[2:]
			continue
		}
		if strings.HasPrefix(word, "--adjustment=") || isSignedNumberOption(word) {
			words = words[1:]
			continue
		}
		if strings.HasPrefix(word, "-") && word != "-" {
			return blocked("unknown nice option " + word + " makes the command target ambiguous")
		}
		return inspectWords(words, depth)
	}
	return nil
}

func isSignedNumberOption(word string) bool {
	if len(word) < 2 || (word[0] != '-' && word[0] != '+') {
		return false
	}
	for _, r := range word[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func inspectTimeout(words []shellWord, depth int) error {
	target, err := commandTarget(words, map[string]bool{
		"-k": true, "--kill-after": true, "-s": true, "--signal": true,
		"--foreground": false, "--preserve-status": false, "--verbose": false,
		"--help": false, "--version": false,
	})
	if err != nil {
		return err
	}
	if len(target) == 0 {
		return nil
	}
	if !target[0].static {
		return blocked("dynamic timeout duration cannot be inspected safely")
	}
	return inspectWords(target[1:], depth)
}

func inspectXargs(words []shellWord, depth int) error {
	for len(words) > 0 {
		if !words[0].static {
			return blocked("dynamic xargs option or command cannot be inspected safely")
		}
		word := words[0].value
		if word == "--" {
			return inspectWords(words[1:], depth)
		}
		if !strings.HasPrefix(word, "-") || word == "-" {
			return inspectWords(words, depth)
		}
		name, inline := optionName(word)
		if isAttachedXargsOption(word) {
			words = words[1:]
			continue
		}
		switch name {
		case "-a", "--arg-file", "-d", "--delimiter", "-E", "--eof", "-I", "-J", "--max-replsize", "-L", "--max-lines", "-n", "--max-args", "-P", "--max-procs", "-R", "-S", "-s", "--max-chars", "--process-slot-var":
			words = words[1:]
			if !inline {
				if len(words) == 0 || !words[0].static {
					return blocked("xargs option " + name + " has an uninspectable value")
				}
				words = words[1:]
			}
			continue
		case "--replace":
			if inline {
				words = words[1:]
				continue
			}
			// GNU permits an optional replacement string. Both interpretations
			// must be safe because platform variants disagree about consumption.
			if err := inspectWords(words[1:], depth); err != nil {
				return err
			}
			if len(words) > 2 {
				return inspectWords(words[2:], depth)
			}
			return nil
		case "-0", "--null", "-o", "--open-tty", "-p", "--interactive", "-r", "--no-run-if-empty", "-t", "--verbose", "-x", "--exit", "--show-limits":
			words = words[1:]
			continue
		default:
			return blocked("unknown xargs option " + word + " makes the command target ambiguous")
		}
	}
	return nil
}

func isAttachedXargsOption(word string) bool {
	if len(word) <= 2 || word[0] != '-' || word[1] == '-' {
		return false
	}
	return strings.ContainsRune("aEdIJLnPRSs", rune(word[1]))
}

func inspectFind(words []shellWord, depth int) error {
	for i := 0; i < len(words); i++ {
		if !words[i].static {
			continue
		}
		switch words[i].value {
		case "-exec", "-execdir", "-ok", "-okdir":
			start := i + 1
			end := start
			for end < len(words) {
				if !words[end].static {
					return blocked("dynamic find action cannot be inspected safely")
				}
				if words[end].value == ";" || words[end].value == "+" {
					break
				}
				end++
			}
			if end == len(words) {
				return blocked("unterminated find action cannot be inspected safely")
			}
			if err := inspectWords(words[start:end], depth); err != nil {
				return err
			}
			i = end
		}
	}
	return nil
}

func joinStatic(words []shellWord) (string, error) {
	parts := make([]string, 0, len(words))
	for _, word := range words {
		if !word.static {
			return "", blocked("dynamic shell payload cannot be inspected safely")
		}
		parts = append(parts, word.value)
	}
	return strings.Join(parts, " "), nil
}

func staticShellPayload(words []shellWord) (string, bool, error) {
	for i := 0; i < len(words); i++ {
		if !words[i].static {
			return "", false, blocked("dynamic shell option or payload cannot be inspected safely")
		}
		word := words[i].value
		if word == "--" {
			return "", false, nil
		}
		if word == "-O" || word == "+O" {
			i++
			if i >= len(words) || !words[i].static {
				return "", false, blocked("shell option value cannot be inspected safely")
			}
			continue
		}
		if !strings.HasPrefix(word, "-") || word == "-" {
			return "", false, nil
		}
		if strings.Contains(strings.TrimLeft(word, "-+"), "c") {
			if i+1 >= len(words) || !words[i+1].static {
				return "", false, blocked("dynamic shell -c payload cannot be inspected safely")
			}
			return words[i+1].value, true, nil
		}
	}
	return "", false, nil
}

func inspectCmd(words []shellWord, depth int) error {
	for i, word := range words {
		if !word.static {
			return blocked("dynamic cmd.exe payload cannot be inspected safely")
		}
		if strings.EqualFold(word.value, "/c") || strings.EqualFold(word.value, "/k") {
			payload, err := joinStatic(words[i+1:])
			if err != nil {
				return err
			}
			return check(payload, depth+1)
		}
	}
	return nil
}

func inspectPowerShell(words []shellWord, depth int) error {
	for i, word := range words {
		if !word.static {
			return blocked("dynamic PowerShell payload cannot be inspected safely")
		}
		name := strings.ToLower(word.value)
		switch name {
		case "-encodedcommand", "-enc", "/encodedcommand":
			return blocked("encoded PowerShell command cannot be inspected safely")
		case "-command", "-c", "/command":
			payload, err := joinStatic(words[i+1:])
			if err != nil {
				return err
			}
			return check(payload, depth+1)
		}
	}
	return nil
}

var sudoOptions = map[string]bool{
	"-u": true, "--user": true, "-g": true, "--group": true, "-h": true, "--host": true,
	"-C": true, "--close-from": true, "-p": true, "--prompt": true, "-R": true, "--chroot": true,
	"-T": true, "--command-timeout": true, "-r": true, "--role": true, "-t": true, "--type": true,
	"-n": false, "--non-interactive": false, "-E": false, "--preserve-env": false,
	"-H": false, "--set-home": false, "-S": false, "--stdin": false, "-k": false, "-K": false,
}

var parallelOptions = map[string]bool{
	"-j": true, "--jobs": true, "-S": true, "--sshlogin": true, "--results": true, "--joblog": true,
}
