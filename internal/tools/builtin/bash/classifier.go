package bash

import (
	"regexp"
	"strings"
)

// CommandClass classifies a bash command into a risk category.
type CommandClass int

const (
	ClassReadOnly CommandClass = iota
	ClassFileWrite
	ClassNetwork
	ClassSystem
	ClassDangerous
	ClassUnknown
)

func (c CommandClass) String() string {
	switch c {
	case ClassReadOnly:
		return "read_only"
	case ClassFileWrite:
		return "file_write"
	case ClassNetwork:
		return "network"
	case ClassSystem:
		return "system"
	case ClassDangerous:
		return "dangerous"
	default:
		return "unknown"
	}
}

// Classification is the result of classifying a bash command.
type Classification struct {
	Class   CommandClass
	Command string
	Args    []string
	Reason  string
}

// BashClassifier classifies bash commands into risk categories.
// Uses pattern matching on command name and arguments to detect
// potentially destructive or dangerous operations.
type BashClassifier struct {
	// Dangerous patterns: commands that modify/delete system state.
	dangerousCmds map[string]bool
	// System-modifying commands.
	systemCmds map[string]bool
	// File-writing commands.
	writeCmds map[string]bool
	// Network commands.
	networkCmds map[string]bool
	// Patterns for dangerous flags/args.
	dangerousFlags []dangerPattern
	// Patterns for destructive operations.
	destructivePatterns []dangerPattern
}

// dangerPattern pairs a regexp with the human-facing explanation used in
// blocked-command results. claude-code parity: its bashSecurity.ts returns
// prose ("Command contains process substitution <()"), never raw regexes —
// the old "dangerous flag detected: (?i)-\s*rf\s" leaked engine internals
// into the transcript (user feedback 2026-08-15: codex/claude-code never
// show this).
type dangerPattern struct {
	re    *regexp.Regexp
	label string
}

func NewBashClassifier() *BashClassifier {
	return &BashClassifier{
		dangerousCmds: map[string]bool{
			"rm": true, "rmdir": true, "dd": true, "mkfs": true,
			"fdisk": true, "parted": true, "sfdisk": true,
			":(){:|:&};:": true, // fork bomb
			"shutdown":    true, "reboot": true, "halt": true,
			"init": true, "telinit": true,
			"poweroff": true, "systemctl poweroff": true,
			"chmod": true, "chown": true,
			"curl": true, "wget": true, // can download and execute
		},
		systemCmds: map[string]bool{
			"systemctl": true, "service": true, "journalctl": true,
			"useradd": true, "userdel": true, "usermod": true,
			"groupadd": true, "groupdel": true,
			"passwd": true, "chage": true,
			"iptables": true, "ufw": true, "firewalld": true,
			"docker": true, "kubectl": true, "helm": true,
			"apt": true, "apt-get": true, "yum": true, "dnf": true, "zypper": true,
			"pacman": true, "snap": true, "flatpak": true,
			"launchctl": true, "sc": true, // macOS/Windows service managers
			"crontab": true, "at": true,
			"export": true, "set": true, "env": true,
			"alias": true, "unalias": true,
		},
		writeCmds: map[string]bool{
			"tee": true, "dd": true,
			"sed": true, "awk": true,
			"cat": true, "echo": true, "printf": true,
			"touch": true, "mkdir": true, "ln": true,
			"cp": true, "mv": true, "install": true,
		},
		networkCmds: map[string]bool{
			"curl": true, "wget": true, "nc": true, "netcat": true,
			"ssh": true, "scp": true, "sftp": true,
			"rsync": true, "ftp": true, "telnet": true,
			"ping": true, "traceroute": true, "tracepath": true,
			"nslookup": true, "dig": true, "host": true,
			"netstat": true, "ss": true, "lsof": true,
			"nmap": true, "tcpdump": true, "wireshark": true,
		},
		dangerousFlags: []dangerPattern{
			{regexp.MustCompile(`(?i)-\s*rf\s`), "rm -rf style recursive forced delete"},
			// `-r` flag is only dangerous when paired with rm/rmdir.
			// Earlier rule `(?i)-\s*r\s` matched `grep -r`, `ls -r`,
			// `docker logs -r`, and `tar -r` — all harmless. The
			// 2026-05-16 v3 longrun report flagged this as a real
			// false-positive blocker. Bind to the rm command so only
			// genuine recursive-delete spellings are caught.
			{regexp.MustCompile(`(?i)\b(rm|rmdir)\s+(?:[^|;&]*\s+)?-\s*r\b`), "rm -r recursive delete"},
			{regexp.MustCompile(`(?i)>/dev/sd[a-z]`), "writing directly to a raw disk device"},
			{regexp.MustCompile(`(?i)dd.*of=/dev/`), "dd writing to a device file"},
			{regexp.MustCompile(`(?i)--no-preserve`), "git clean --no-preserve-root (can delete the repo root)"},
			// Permissive chmod only — `chmod 777`, `chmod 0666`, `chmod 776`.
			// Earlier rule (`chmod\s+[0-7][0-7][0-7][0-7]?`) flagged plain
			// `chmod 644 README.md` which is harmless.
			{regexp.MustCompile(`(?i)chmod\s+0?(?:777|766|776|666)\b`), "world-writable chmod (777/666)"},
			{regexp.MustCompile(`(?i)git\s+push\s+(?:[^|]+?\s+)?(?:-f|--force(?:-with-lease)?)`), "force push (rewrites remote history)"},
			{regexp.MustCompile(`(?i)git\s+(?:reset\s+--hard|clean\s+-fdx)`), "git reset --hard / clean -fdx (discards uncommitted work)"},
			{regexp.MustCompile(`(?i)pip\s+install\s+--break-system-packages`), "pip --break-system-packages (bypasses system package protection)"},
			{regexp.MustCompile(`(?i)npm\s+publish\s+(?:[^|]+\s+)?--access\s+public`), "npm publish with public access"},
		},
		destructivePatterns: []dangerPattern{
			// Fork-bomb detection. Earlier rule `(?i)(fork\s*bomb|:.*:.*:.*&)`
			// produced false positives in two ways:
			//   1. `fork\s*bomb` matched any prose containing "fork bomb"
			//      (with `\s*` allowing zero whitespace, even "forkbomb"
			//      appearing inside arbitrary strings).
			//   2. `:.*:.*:.*&` matched any string with 3 colons and an
			//      ampersand — common in `:foo:bar:baz&` substrings.
			// The 2026-05-16 v3 longrun blocked an `echo "Fork: 1 call"`
			// metadata line on this. Tighten to the canonical recursive
			// shell-function shape `:(){...}` which is the actual fork
			// bomb syntax — `:()` defines a recursive function named `:`
			// and `{...}` opens its body. Prose can't accidentally hit
			// this shape.
			{regexp.MustCompile(`:\s*\(\s*\)\s*\{[^}]*:\s*\|\s*:\s*&[^}]*\}`), "fork bomb pattern"},
			{regexp.MustCompile(`(?i)>\s*/etc/`), "writing to /etc"},
			{regexp.MustCompile(`(?i)>\s*/var/`), "writing to /var"},
			{regexp.MustCompile(`(?i)>\s*/usr/`), "writing to /usr"},
			{regexp.MustCompile(`(?i)>\s*~/.ssh/`), "writing to ~/.ssh"},
			{regexp.MustCompile(`(?i)>\s*/sys/`), "writing to /sys"},
			{regexp.MustCompile(`(?i)>\s*/proc/`), "writing to /proc"},
			// Pipe-to-shell: curl … | bash, wget -qO- … | sh, etc.
			// The leading prefix matches a downloader, the tail covers any pipe
			// landing in a shell or eval — including chained `tee` / `head`.
			{regexp.MustCompile(`(?i)(curl|wget|fetch)\b[^|]*\|\s*(?:[^|]*\|\s*)*(?:bash|sh|zsh|fish|eval)\b`), "downloading a script and piping it to a shell (curl | bash)"},
		},
	}
}

// ClassifierHook is an optional pre-classification callback. Returning a
// non-nil Classification short-circuits the rule engine (e.g. for an ML
// classifier, audit log, or test injection). Set to nil to disable.
type ClassifierHook func(cmd string) *Classification

// Hook lets callers inject a custom pre-classifier. If the hook returns a
// non-nil result it overrides the rule-based classification.
var Hook ClassifierHook

// NormalizeCommand strips a trailing variant suffix from a base command.
// Example: "mkfs.ext4" → "mkfs", "systemctl.bin" → "systemctl". The base
// word always wins so dangerousCmds/systemCmds lookups don't miss
// distribution-specific spellings.
func NormalizeCommand(cmd string) string {
	if i := strings.IndexByte(cmd, '.'); i > 0 {
		return cmd[:i]
	}
	return cmd
}

// Classify returns the classification of a bash command string.
func (c *BashClassifier) Classify(cmd string) Classification {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Classification{Class: ClassUnknown, Command: "", Reason: "empty command"}
	}

	if Hook != nil {
		if got := Hook(cmd); got != nil {
			return *got
		}
	}

	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return Classification{Class: ClassUnknown, Command: "", Reason: "empty"}
	}

	rawCommand := parts[0]
	command := NormalizeCommand(rawCommand)
	args := parts[1:]

	// Check dangerous flags/patterns first
	for _, pattern := range c.dangerousFlags {
		if pattern.re.MatchString(cmd) {
			return Classification{
				Class:   ClassDangerous,
				Command: command,
				Args:    args,
				Reason:  "dangerous flag: " + pattern.label,
			}
		}
	}
	for _, pattern := range c.destructivePatterns {
		if pattern.re.MatchString(cmd) {
			return Classification{
				Class:   ClassDangerous,
				Command: command,
				Args:    args,
				Reason:  "destructive pattern: " + pattern.label,
			}
		}
	}

	// Check dangerous commands
	if c.dangerousCmds[command] {
		reason := "known dangerous command"
		// Some dangerous commands are only dangerous with certain args
		if command == "rm" && len(args) > 0 && !containsRF(args) {
			return Classification{Class: ClassFileWrite, Command: command, Args: args, Reason: "rm without -rf"}
		}
		if command == "curl" || command == "wget" {
			// Check for piping to shell (eval)
			if hasPipeToShell(args) {
				reason = "downloading and executing code"
			} else {
				return Classification{Class: ClassNetwork, Command: command, Args: args, Reason: "network download"}
			}
		}
		if command == "chmod" && len(args) > 0 {
			if isPermissiveChmod(args[0]) {
				reason = "permissive chmod"
			} else {
				// chmod 644 / chmod +x are routine; only world-writable
				// modes get the Dangerous label.
				return Classification{Class: ClassFileWrite, Command: command, Args: args, Reason: "chmod (non-permissive)"}
			}
		}
		return Classification{Class: ClassDangerous, Command: command, Args: args, Reason: reason}
	}

	// Check system commands
	if c.systemCmds[command] {
		reason := "system modification command"
		if command == "curl" || command == "wget" {
			return Classification{Class: ClassNetwork, Command: command, Args: args, Reason: "network request"}
		}
		return Classification{Class: ClassSystem, Command: command, Args: args, Reason: reason}
	}

	// Check network commands
	if c.networkCmds[command] {
		return Classification{Class: ClassNetwork, Command: command, Args: args, Reason: "network operation"}
	}

	// Check file writing commands
	if c.writeCmds[command] {
		if command == "dd" {
			return Classification{Class: ClassDangerous, Command: command, Args: args, Reason: "dd can write raw devices"}
		}
		if command == "sed" || command == "awk" {
			// These can modify files in place
			if hasInplaceFlag(args) {
				return Classification{Class: ClassFileWrite, Command: command, Args: args, Reason: "in-place file modification"}
			}
		}
		if isRedirection(cmd) {
			return Classification{Class: ClassFileWrite, Command: command, Args: args, Reason: "output redirection"}
		}
		return Classification{Class: ClassFileWrite, Command: command, Args: args, Reason: "file write operation"}
	}

	// Default to read-only for common read commands
	readOnlyCmds := map[string]bool{
		"ls": true, "cat": true, "head": true, "tail": true,
		"grep": true, "find": true, "which": true, "whereis": true,
		"pwd": true, "cd": true, "echo": true, "printf": true,
		"wc": true, "sort": true, "uniq": true, "cut": true,
		"diff": true, "cmp": true, "stat": true, "file": true,
		"id": true, "whoami": true, "uptime": true, "df": true, "du": true,
		"free": true, "top": true, "ps": true, "htop": true,
		"git": true, "svn": true, "hg": true,
		"make": true, "go": true, "npm": true, "pip": true,
		"python": true, "python3": true, "ruby": true, "node": true,
		"rustc": true, "cargo": true,
		"vim": true, "nano": true, "less": true, "more": true,
		"tar": true, "gzip": true, "gunzip": true, "zip": true, "unzip": true,
		"watch": true, "xargs": true, "parallel": true,
		"jq": true, "yq": true, "awk": true, "sed": true,
	}
	if readOnlyCmds[command] {
		return Classification{Class: ClassReadOnly, Command: command, Args: args, Reason: "read-only command"}
	}

	return Classification{Class: ClassUnknown, Command: command, Args: args, Reason: "unclassified"}
}

func containsRF(args []string) bool {
	for _, a := range args {
		if strings.Contains(a, "-rf") || strings.Contains(a, "-r") || strings.Contains(a, "-f") {
			return true
		}
	}
	return false
}

func hasPipeToShell(args []string) bool {
	for i, a := range args {
		if a == "|" && i+1 < len(args) && (args[i+1] == "bash" || args[i+1] == "sh" || args[i+1] == "eval") {
			return true
		}
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			if i+1 < len(args) && (strings.Contains(args[i+1], "bash") || strings.Contains(args[i+1], "sh")) {
				return true
			}
		}
	}
	return false
}

func isPermissiveChmod(arg string) bool {
	// Very permissive: 0777, 0766, 0755 that includes world-write
	return regexp.MustCompile(`^0?[0-7]{3,4}$`).MatchString(arg) &&
		(strings.HasSuffix(arg, "777") || strings.HasSuffix(arg, "766"))
}

func hasInplaceFlag(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-i") || a == "-i" {
			return true
		}
	}
	return false
}

func isRedirection(cmd string) bool {
	return regexp.MustCompile(`>\s*[` + "`" + `~/\w]`).MatchString(cmd)
}
