//go:build darwin

package sandbox

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const darwinSandboxExecutable = "/usr/bin/sandbox-exec"

func doctorPlatform() Diagnostic {
	d := Diagnostic{
		Platform:   runtime.GOOS,
		Backend:    "seatbelt",
		Supported:  true,
		Executable: darwinSandboxExecutable,
	}
	path, err := exec.LookPath(darwinSandboxExecutable)
	if err != nil {
		d.Err = fmt.Errorf("%w: %s: %v", ErrDependencyMissing, darwinSandboxExecutable, err)
		return d
	}
	d.Available = true
	d.Executable = path
	return d
}

func wrapPlatform(cmd *exec.Cmd, req platformRequest) error {
	diagnostic := doctorPlatform()
	if !diagnostic.Available {
		return diagnostic.Err
	}
	profile := buildDarwinProfile(req.cwd, req.tempDir, req.home, req.metisHome, req.network)
	originalArgv := append([]string(nil), cmd.Args...)
	if len(originalArgv) == 0 {
		originalArgv = []string{cmd.Path}
	}
	cmd.Path = diagnostic.Executable
	cmd.Args = append([]string{"sandbox-exec", "-p", profile}, originalArgv...)
	return nil
}

func buildDarwinProfile(cwd, tempDir, home, metisHome string, network NetworkPolicy) string {
	quote := func(path string) string {
		path = strings.ReplaceAll(path, `\`, `\\`)
		path = strings.ReplaceAll(path, `"`, `\"`)
		return `"` + path + `"`
	}
	regex := func(pattern string) string {
		pattern = strings.ReplaceAll(pattern, `"`, `\"`)
		return `#"` + pattern + `"`
	}

	rules := []string{
		`(version 1)`,
		`(deny default)`,
		`(allow process-exec)`,
		`(allow process-fork)`,
		`(allow signal (target self))`,
		`(allow mach-lookup)`,
		`(allow sysctl-read)`,
		`(allow iokit-open)`,
		`(allow file-read*)`,
		fmt.Sprintf(`(allow file-write* (subpath %s))`, quote(cwd)),
		fmt.Sprintf(`(allow file-write* (subpath %s))`, quote(tempDir)),
		`(allow file-write* (literal "/dev/null"))`,
		`(allow file-write* (literal "/dev/stdout"))`,
		`(allow file-write* (literal "/dev/stderr"))`,
		`(allow file-write-data (literal "/dev/tty"))`,
	}
	if network == NetworkAllow {
		rules = append(rules, `(allow network*)`)
	}

	// The repository remains writable, except for configuration and automatic
	// code-loading locations that can turn a later trusted invocation into
	// arbitrary code execution.
	gitConfig := filepath.Join(cwd, ".git", "config")
	gitHooks := filepath.Join(cwd, ".git", "hooks")
	metisDir := filepath.Join(cwd, ".metis")
	protectedWrites := []string{
		fmt.Sprintf(`(deny file-write* (literal %s))`, quote(gitConfig)),
		fmt.Sprintf(`(deny file-write* (subpath %s))`, quote(gitHooks)),
		fmt.Sprintf(`(deny file-write* (subpath %s))`, quote(filepath.Join(metisDir, "agents"))),
		fmt.Sprintf(`(deny file-write* (subpath %s))`, quote(filepath.Join(metisDir, "commands"))),
		fmt.Sprintf(`(deny file-write* (subpath %s))`, quote(filepath.Join(metisDir, "skills"))),
	}
	configPattern := "^" + regexp.QuoteMeta(filepath.Join(metisDir, "config")) + `[^/]*\.toml$`
	protectedWrites = append(protectedWrites,
		fmt.Sprintf(`(deny file-write* (regex %s))`, regex(configPattern)))
	seenDangerousWrites := make(map[string]struct{})
	for _, root := range []string{cwd, home} {
		for _, file := range dangerousWriteFiles(root) {
			for _, path := range policyPathVariants(file) {
				if _, seen := seenDangerousWrites["f:"+path]; seen {
					continue
				}
				seenDangerousWrites["f:"+path] = struct{}{}
				protectedWrites = append(protectedWrites,
					fmt.Sprintf(`(deny file-write* (literal %s))`, quote(path)))
			}
		}
		for _, dir := range dangerousWriteDirectories(root) {
			for _, path := range policyPathVariants(dir) {
				if _, seen := seenDangerousWrites["d:"+path]; seen {
					continue
				}
				seenDangerousWrites["d:"+path] = struct{}{}
				protectedWrites = append(protectedWrites,
					fmt.Sprintf(`(deny file-write* (subpath %s))`, quote(path)))
			}
		}
	}
	rules = append(rules, protectedWrites...)

	// Do not expose persistent credentials to model-authored shell commands.
	// Protect both a custom METIS_HOME and the default ~/.metis: stale/default
	// credentials remain sensitive even when this runtime uses a custom root.
	for _, root := range metisControlRoots(home, metisHome) {
		// This explicit deny is required even without a broad ~/.metis allow:
		// Metis is commonly launched with the user's home as cwd, whose write
		// grant would otherwise include all persistent Metis state.
		rules = append(rules,
			fmt.Sprintf(`(deny file-write* (subpath %s))`, quote(root)),
			fmt.Sprintf(`(deny file-read* (subpath %s))`, quote(filepath.Join(root, metisCredentialDirectoryName))))
		credentialFiles := []string{
			"auth.json",
			"llm-oauth.json",
			".llm-oauth.lock",
			"mcp-oauth.json",
			".mcp-oauth.lock",
			"mcp.toml",
			"credentials.json",
			"secrets.json",
			"config.toml", // may contain legacy inline api_key values
			"config.local.toml",
		}
		for _, name := range credentialFiles {
			rules = append(rules,
				fmt.Sprintf(`(deny file-read* (literal %s))`, quote(filepath.Join(root, name))))
		}
		llmOAuthPrefix := "^" + regexp.QuoteMeta(root+string(filepath.Separator))
		for _, suffix := range []string{
			`\.auth\.json\.[^/]+$`,
			`\.llm-oauth-refresh-[^/]+\.lock$`,
			`\.llm-oauth-[^/]+\.tmp$`,
			`\.mcp-oauth-refresh-[^/]+\.lock$`,
			`\.mcp-oauth-[^/]+\.tmp$`,
		} {
			rules = append(rules,
				fmt.Sprintf(`(deny file-read* (regex %s))`, regex(llmOAuthPrefix+suffix)))
		}
		rules = append(rules,
			fmt.Sprintf(`(deny file-read* (subpath %s))`, quote(filepath.Join(root, "ide"))))
	}

	if home != "" {
		// Host-wide reads are useful for build tools, but must not expose the
		// user's reusable cloud, SSH, package-registry, or Git credentials.
		// Deny writes too: when Metis starts in $HOME the cwd grant would
		// otherwise allow persistence by replacing those files.
		for _, dir := range sensitiveHomeDirectories(home) {
			for _, path := range policyPathVariants(dir) {
				rules = append(rules,
					fmt.Sprintf(`(deny file-read* (subpath %s))`, quote(path)),
					fmt.Sprintf(`(deny file-write* (subpath %s))`, quote(path)))
			}
		}
		for _, file := range sensitiveHomeFiles(home) {
			for _, path := range policyPathVariants(file) {
				rules = append(rules,
					fmt.Sprintf(`(deny file-read* (literal %s))`, quote(path)),
					fmt.Sprintf(`(deny file-write* (literal %s))`, quote(path)))
			}
		}
	}

	return strings.Join(rules, "\n") + "\n"
}
