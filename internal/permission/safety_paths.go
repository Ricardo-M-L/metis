package permission

import "strings"

// SafetyCheckPathFragments are file path fragments that should always
// trigger an ASK, even when the gate is in mode=Bypass. Mirrors
// claude-code's bypass-immune safetyCheck flow (filesystem.ts:55-78,
// permissions.ts:1252-1260).
//
// Lightweight fragment match (not glob) — chosen for speed and to handle
// relative vs absolute path variants uniformly. Directory fragments also use
// path/shell boundaries so `.claude` matches but `.claude-backup` does not.
// The set is the union of:
//
//   - VCS internals (.git/) where a malicious commit hook is a
//     remote-execution vector
//   - Shell startup files (.bashrc / .zshrc / .profile / .zprofile /
//     .bash_profile) where appended commands run on every new shell
//   - SSH key material (.ssh/, authorized_keys, known_hosts) — credential
//     theft + lateral movement
//   - Editor / agent settings (.claude/, .metis/, .vscode/) — meta-attack
//     vector where modifying our own permission rules opens future doors
//   - System trees (/etc/, /usr/, /System/, /Library/) — should always
//     prompt; legitimate systemwide ops are rare and worth the friction
//   - Crontab + scheduler hooks (/etc/cron, /var/spool/cron, launchd
//     plists)
//   - macOS auto-start (LaunchAgents / LaunchDaemons)
var SafetyCheckPathFragments = []string{
	// VCS internals
	".git/",

	// Shell startup
	".bashrc",
	".bash_profile",
	".bash_logout",
	".zshrc",
	".zprofile",
	".zshenv",
	".profile",

	// SSH
	".ssh/",
	"authorized_keys",
	"known_hosts",
	"id_rsa",
	"id_ed25519",
	"id_ecdsa",

	// Agent / editor settings
	".claude/",
	".metis/",
	".vscode/",
	".cursor/",

	// System trees (common attack surface — never auto-allow writes)
	"/etc/",
	"/usr/local/bin/",
	"/usr/bin/",
	"/System/",
	"/Library/LaunchAgents",
	"/Library/LaunchDaemons",

	// Schedulers
	"/var/spool/cron",
	".launchd",

	// Misc credentials
	".aws/credentials",
	".kube/config",
	".docker/config.json",
	"netrc",
	".pypirc",
	".npmrc",
	".gemrc",
}

// matchesSafetyPath reports whether stringInput contains any safety
// path fragment. Used by Gate.Check to bypass-immune the action.
func matchesSafetyPath(stringInput string) bool {
	if stringInput == "" {
		return false
	}
	for _, frag := range SafetyCheckPathFragments {
		if containsSafetyPathFragment(stringInput, frag) {
			return true
		}
	}
	return false
}

// containsSafetyPathFragment preserves the historical substring check while
// also recognizing a protected directory itself, without requiring a trailing
// slash. For example, both `~/.claude/settings.json` and `rm -rf ~/.claude`
// are safety-path hits, while `~/.claude-backup` is not mistaken for the
// protected directory.
func containsSafetyPathFragment(input, fragment string) bool {
	if !strings.HasSuffix(fragment, "/") {
		return strings.Contains(input, fragment)
	}
	base := strings.TrimSuffix(fragment, "/")
	for offset := 0; offset < len(input); {
		idx := strings.Index(input[offset:], base)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(base)
		startOK := start == 0 || isSafetyPathBoundary(input[start-1])
		endOK := end == len(input) || isSafetyPathBoundary(input[end])
		if startOK && endOK {
			return true
		}
		offset = end
	}
	return false
}

func isSafetyPathBoundary(ch byte) bool {
	switch ch {
	case '/', '\\', ' ', '\t', '\n', '\r', '\'', '"', ';', '&', '|', '<', '>', '(', ')', '[', ']', '{', '}', '=', ':', ',':
		return true
	default:
		return false
	}
}

// fileTouchingTools is the set of tools whose stringInput is plausibly
// a path to be safety-checked. Tools not in this set (Read, Grep, ...)
// don't get the safetyCheck treatment because their stringInput is
// either a search query (Grep), a URL (WebFetch), or a benign payload
// the model passes through.
//
// Bash is included because its argv often contains paths and shells
// out to system commands. The bash_security_rules layer covers
// command-content attacks; this layer covers path-target attacks.
var fileTouchingTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"NotebookEdit": true,
	"Bash":         true,
}

// isFileTouchingTool reports whether tool is one whose stringInput
// should be checked against SafetyCheckPathFragments.
func isFileTouchingTool(tool string) bool {
	return fileTouchingTools[tool]
}

// secretReadPathFragments are credential FILES whose CONTENT is itself a
// secret — reading them leaks the credential into the model context, the
// transcript, and the provider request. Unlike SafetyCheckPathFragments
// (which guards WRITES that could plant code), these guard READS, and the
// list is deliberately narrow: reading .git/config or /etc/hosts is benign,
// reading an SSH private key or cloud credentials is not. Common dev files
// (.env, .npmrc) are intentionally excluded to avoid prompt fatigue.
var secretReadPathFragments = []string{
	".ssh/",
	"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
	".aws/credentials",
	".kube/config",
	".docker/config.json",
	".gnupg/",
	".netrc",
	"service-account", "service_account",
	"credentials.json",
}

// readPathTools are tools that expose a file's CONTENT to the model via a
// path in their stringInput — so a secret-path read must be gated to ASK
// even in modes that auto-allow read-only tools.
var readPathTools = map[string]bool{
	"Read": true,
	// Grep returns matching file contents, so scanning a credential root can
	// leak the same secrets as Read. Its permission payload includes `root`.
	"Grep": true,
}

// matchesSecretReadPath reports whether stringInput points at a credential
// file whose content is a secret.
func matchesSecretReadPath(stringInput string) bool {
	if stringInput == "" {
		return false
	}
	low := strings.ToLower(stringInput)
	for _, frag := range secretReadPathFragments {
		if strings.Contains(low, frag) {
			return true
		}
	}
	return false
}

// isSecretReadAttempt reports whether tool would read the content of a
// credential file. Bash participates only when its complete command is
// classified as read-only; mutating Bash calls are already handled by the
// broader safety-path check.
func isSecretReadAttempt(tool, stringInput string) bool {
	if !matchesSecretReadPath(stringInput) {
		return false
	}
	if readPathTools[tool] {
		return true
	}
	return tool == "Bash" && IsReadOnlyBashSafetyOperation(stringInput)
}
