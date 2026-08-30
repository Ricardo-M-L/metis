package permission

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var (
	metisHomeVariableRE = regexp.MustCompile(`(?i)\$\{METIS_HOME\}|\$env:METIS_HOME\b|\$METIS_HOME\b|%METIS_HOME%`)
	homeVariableRE      = regexp.MustCompile(`(?i)\$\{HOME\}|\$env:HOME\b|\$HOME\b|%USERPROFILE%`)
	dotEnvCredentialRE  = regexp.MustCompile(`(?:^|[/\s"'=:(])(\.env(?:\.[a-z0-9_-]+)*)(?:$|[/\s"';|&),])`)
)

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
	// macOS and Windows commonly resolve these paths case-insensitively. Keep
	// the security comparison conservative on every platform so a repository
	// on a case-insensitive volume cannot bypass protection with `.GiT`,
	// `.SSH`, or `.MeTiS` aliases.
	stringInput = strings.ToLower(stringInput)
	for _, frag := range SafetyCheckPathFragments {
		if containsSafetyPathFragment(stringInput, strings.ToLower(frag)) {
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
// list is deliberately about credential-bearing content: reading .git/config
// or /etc/hosts is benign, while package-manager auth, cloud credentials and
// real .env files can exfiltrate secrets into the provider request. Example
// templates such as .env.example remain readable.
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
	".npmrc",
	".pypirc",
	".gemrc",
	".gem/credentials",
	".git-credentials",
	".config/gh/hosts.yml",
	".config/gcloud/application_default_credentials.json",
	".azure/accesstokens.json",
	".azure/msal_token_cache.json",
	".cargo/credentials",
	".cargo/credentials.toml",
	".terraform.d/credentials.tfrc.json",
	".config/rclone/rclone.conf",
	// METIS keeps provider, MCP OAuth, and explicitly configured MCP
	// credentials in these files. They must never enter model context through
	// Read/Grep or a read-only Bash command.
	".metis/auth.json",
	".metis/mcp-oauth.json",
	".metis/mcp.toml",
	// Inline provider keys are deprecated but still supported in user and
	// project-local configuration, so treat both config variants as secret.
	".metis/config.toml",
	".metis/config.local.toml",
}

// readPathTools are tools that expose a file's CONTENT to the model via a
// path or code literal in their stringInput — so a secret-path read must be
// gated in modes that otherwise auto-allow these tools. Interactive modes ask;
// bypassPermissions fails closed without prompting.
var readPathTools = map[string]bool{
	"Read": true,
	// Grep returns matching file contents, so scanning a credential root can
	// leak the same secrets as Read. Its permission payload includes `root`.
	"Grep":    true,
	"RunCode": true,
}

// matchesSecretReadPath reports whether stringInput points at a credential
// file whose content is a secret.
func matchesSecretReadPath(stringInput string) bool {
	if stringInput == "" {
		return false
	}
	// Expand only the two trusted home variables used by METIS credential
	// paths. This is deliberately not a general shell expansion (which would
	// execute or guess model-controlled syntax), but it closes the common
	// `cat "$METIS_HOME/auth.json"` spelling that otherwise contains neither
	// the relocated absolute root nor the literal `.metis` directory name.
	expandedInput := expandKnownCredentialRoots(stringInput)
	// Compare both Windows-style separators and POSIX shell escape spelling.
	// A backslash means a directory separator to PowerShell, but removes the
	// special meaning of the following byte in an unquoted POSIX shell token.
	for _, low := range credentialMatchForms(expandedInput) {
		for _, frag := range secretReadPathFragments {
			if strings.Contains(low, frag) {
				return true
			}
		}
		if matchesRealDotEnvPath(low) {
			return true
		}
		// METIS_HOME may be relocated to an arbitrary directory whose spelling no
		// longer contains `.metis`. Protect its raw, absolute, and symlink-resolved
		// spellings so direct in-process Read/Grep checks match the same roots as
		// the OS sandbox.
		for _, credentialPath := range metisSecretReadPaths() {
			for _, normalized := range credentialMatchForms(filepath.Clean(credentialPath)) {
				if normalized != "." && containsCredentialPathAfterLexicalClean(low, normalized) {
					return true
				}
			}
		}
	}
	return false
}

// IsSecretReadPath exposes the credential-file classifier to in-process file
// tools. Permission checks normally stop these paths before Execute, while
// Grep also needs it per walked file because its user-facing root may be a
// broad project directory containing a hidden .env or package-manager token.
func IsSecretReadPath(path string) bool { return matchesSecretReadPath(path) }

func matchesRealDotEnvPath(input string) bool {
	for _, match := range dotEnvCredentialRE.FindAllStringSubmatch(input, -1) {
		if len(match) < 2 {
			continue
		}
		switch match[1] {
		case ".env.example", ".env.sample", ".env.template", ".env.dist", ".env.default":
			continue
		default:
			return true
		}
	}
	return false
}

// normalizePathSeparatorsForMatch creates a comparison-only copy of text that
// contains path spellings. It deliberately does not feed back into command
// execution. Collapsing repeated separators closes aliases such as
// $METIS_HOME//auth.json and PowerShell/Windows spellings that expand to a
// Unix root followed by one or more backslashes. Changing URL or UNC spelling
// in this private comparison copy cannot affect their runtime semantics.
func normalizePathSeparatorsForMatch(input string) string {
	input = strings.ReplaceAll(strings.ToLower(input), "\\", "/")
	// Shells concatenate adjacent quoted and unquoted fragments, so
	// `"/tmp/metis"/auth.json` names the same file as
	// `/tmp/metis/auth.json`. This is a comparison-only copy: dropping quote
	// delimiters cannot alter execution and deliberately errs toward protecting
	// credentials when the command uses mixed quoting to obscure a path.
	input = strings.NewReplacer(`"`, "", `'`, "").Replace(input)
	for strings.Contains(input, "//") {
		input = strings.ReplaceAll(input, "//", "/")
	}
	return input
}

func credentialMatchForms(input string) []string {
	forms := []string{normalizePathSeparatorsForMatch(input), normalizePOSIXShellPathForMatch(input)}
	if forms[0] == forms[1] {
		return forms[:1]
	}
	return forms
}

// normalizePOSIXShellPathForMatch removes quoting delimiters and conservative
// backslash escapes in a comparison-only copy. It closes spellings such as
// `au\th.json`, which the POSIX shell executes as `auth.json`. False positives
// are preferable to exposing a credential; this string is never executed.
func normalizePOSIXShellPathForMatch(input string) string {
	input = strings.ToLower(input)
	var b strings.Builder
	b.Grow(len(input))
	for i := 0; i < len(input); i++ {
		switch input[i] {
		case '\'', '"':
			continue
		case '\\':
			if i+1 < len(input) {
				i++
				b.WriteByte(input[i])
			}
		default:
			b.WriteByte(input[i])
		}
	}
	out := b.String()
	for strings.Contains(out, "//") {
		out = strings.ReplaceAll(out, "//", "/")
	}
	return out
}

func expandKnownCredentialRoots(input string) string {
	if metisHome := strings.TrimSpace(os.Getenv("METIS_HOME")); metisHome != "" {
		if abs, err := filepath.Abs(metisHome); err == nil {
			metisHome = abs
		}
		input = metisHomeVariableRE.ReplaceAllStringFunc(input, func(string) string { return metisHome })
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		input = homeVariableRE.ReplaceAllStringFunc(input, func(string) string { return home })
		input = strings.ReplaceAll(input, "~/", home+string(filepath.Separator))
		input = strings.ReplaceAll(input, `~\`, home+string(filepath.Separator))
	}
	return input
}

// containsCredentialPathAfterLexicalClean recognizes path aliases such as
// $METIS_HOME/sub/../auth.json without treating the whole shell command as a
// filesystem path. The comparison copy is already lower-cased and slash
// normalized; cleaning only the token that starts at the known credential
// root cannot change execution semantics.
func containsCredentialPathAfterLexicalClean(input, credential string) bool {
	if strings.Contains(input, credential) {
		return true
	}
	root := path.Dir(credential)
	for searchFrom := 0; searchFrom < len(input); {
		rel := strings.Index(input[searchFrom:], root)
		if rel < 0 {
			return false
		}
		start := searchFrom + rel
		end := start + len(root)
		for end < len(input) && !strings.ContainsRune(" \t\r\n\"'`;|&(){}[]<>,", rune(input[end])) {
			end++
		}
		if path.Clean(input[start:end]) == credential {
			return true
		}
		searchFrom = start + len(root)
	}
	return false
}

func metisSecretReadPaths() []string {
	roots := []string{strings.TrimSpace(os.Getenv("METIS_HOME"))}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".metis"))
	}
	files := []string{"auth.json", "mcp-oauth.json", "mcp.toml", "config.toml", "config.local.toml"}
	rootVariants := make([]string, 0, len(roots)*3)
	rootSeen := make(map[string]struct{}, cap(rootVariants))
	addRootVariant := func(candidate string) {
		candidate = filepath.Clean(candidate)
		if _, ok := rootSeen[candidate]; !ok {
			rootSeen[candidate] = struct{}{}
			rootVariants = append(rootVariants, candidate)
		}
		// On macOS, /var, /tmp, and /etc live below /private. EvalSymlinks
		// returns the /private spelling, while a shell command may use the
		// user-facing spelling. Treat both as the same comparison-only root.
		if runtime.GOOS == "darwin" && strings.HasPrefix(candidate, "/private/") {
			alias := strings.TrimPrefix(candidate, "/private")
			if _, ok := rootSeen[alias]; !ok {
				rootSeen[alias] = struct{}{}
				rootVariants = append(rootVariants, alias)
			}
		}
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		addRootVariant(root)
		if abs, err := filepath.Abs(root); err == nil {
			addRootVariant(abs)
			// Resolve independently of the absolute-path de-duplication above.
			// METIS_HOME is commonly already absolute, so nesting this under the
			// !rootSeen[abs] branch silently skipped its canonical symlink target.
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				addRootVariant(resolved)
			}
		}
	}
	paths := make([]string, 0, len(rootVariants)*len(files))
	seen := make(map[string]struct{}, cap(paths))
	for _, root := range rootVariants {
		for _, name := range files {
			path := filepath.Clean(filepath.Join(root, name))
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			paths = append(paths, path)
		}
	}
	return paths
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
	return tool == "Bash" && IsReadOnlyBashSafetyOperation(expandKnownCredentialRoots(stringInput))
}

// isBypassImmuneSafetyAttempt covers both the built-in safety fragments and
// credential files rooted at a relocated METIS_HOME. The latter must protect
// writes as well as reads: otherwise a model could overwrite auth/config and
// influence a later unattended run even though reading the same file is
// correctly denied.
func isBypassImmuneSafetyAttempt(tool, stringInput string) bool {
	return isFileTouchingTool(tool) && (matchesSafetyPath(stringInput) || matchesSecretReadPath(stringInput))
}

// metisSecretCoveredByRoot reports one protected file below root. Grep reads
// recursively, so checking only the spelling of the directory would miss a
// relocated METIS_HOME whose name does not contain `.metis`.
func metisSecretCoveredByRoot(root string) (string, bool) {
	if abs, err := filepath.Abs(root); err == nil {
		root = filepath.Clean(abs)
	} else {
		return "", false
	}
	for _, secret := range metisSecretReadPaths() {
		rel, err := filepath.Rel(root, secret)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return secret, true
		}
	}
	return "", false
}
