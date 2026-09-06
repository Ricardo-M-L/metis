//go:build darwin

package sandbox

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestDarwinWrapBuildsArgvAndPreservesFields(t *testing.T) {
	t.Parallel()
	m, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	cwd := t.TempDir()
	stdin := bytes.NewBufferString("stdin")
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	cmd := exec.Command("/bin/sh", "-c", "printf ok")
	cmd.Env = []string{"ONE=1", "TWO=2"}
	cmd.Dir = cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	originalArgs := append([]string(nil), cmd.Args...)
	originalEnv := append([]string(nil), cmd.Env...)

	wrapped, err := m.Wrap(cmd, Request{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped != cmd || cmd.Path != darwinSandboxExecutable {
		t.Fatalf("wrapped command = (%p, %q), want (%p, %q)", wrapped, cmd.Path, cmd, darwinSandboxExecutable)
	}
	if len(cmd.Args) != len(originalArgs)+3 || cmd.Args[0] != "sandbox-exec" || cmd.Args[1] != "-p" {
		t.Fatalf("unexpected sandbox argv: %v", cmd.Args)
	}
	if !reflect.DeepEqual(cmd.Args[3:], originalArgs) {
		t.Fatalf("original argv changed: got %v want %v", cmd.Args[3:], originalArgs)
	}
	if !reflect.DeepEqual(cmd.Env, originalEnv) || cmd.Dir != cwd || cmd.Stdin != stdin || cmd.Stdout != stdout || cmd.Stderr != stderr {
		t.Fatal("Wrap changed env, dir, or stdio")
	}
}

func TestDarwinProfilePolicy(t *testing.T) {
	t.Parallel()
	cwd := filepath.Join(string(filepath.Separator), "work", "repo")
	tempDir := filepath.Join(string(filepath.Separator), "private", "tmp", "metis-sandbox-123")
	home := filepath.Join(string(filepath.Separator), "Users", "test")
	profile := buildDarwinProfile(cwd, tempDir, home, filepath.Join(home, ".metis"), NetworkAllow)

	wants := []string{
		`(deny default)`,
		`(allow file-read*)`,
		`(allow network*)`,
		`(allow file-write* (subpath "/work/repo"))`,
		`(allow file-write* (subpath "/private/tmp/metis-sandbox-123"))`,
		`(deny file-write* (literal "/work/repo/.git/config"))`,
		`(deny file-write* (subpath "/work/repo/.git/hooks"))`,
		`/work/repo/\.metis/config`,
		`(deny file-write* (subpath "/work/repo/.metis/agents"))`,
		`(deny file-write* (subpath "/work/repo/.metis/commands"))`,
		`(deny file-write* (subpath "/work/repo/.metis/skills"))`,
		`(deny file-write* (literal "/work/repo/.gitmodules"))`,
		`(deny file-write* (literal "/work/repo/.mcp.json"))`,
		`(deny file-write* (subpath "/work/repo/.vscode"))`,
		`(deny file-write* (subpath "/work/repo/.idea"))`,
		`(deny file-write* (literal "/Users/test/.zshrc"))`,
		`(deny file-write* (subpath "/Users/test/.metis"))`,
		`(deny file-read* (subpath "/Users/test/.metis/.credentials"))`,
		`(deny file-read* (literal "/Users/test/.metis/auth.json"))`,
		`(deny file-read* (literal "/Users/test/.metis/llm-oauth.json"))`,
		`(deny file-read* (literal "/Users/test/.metis/.llm-oauth.lock"))`,
		`/Users/test/\.metis/\.llm-oauth-refresh-[^/]+\.lock`,
		`/Users/test/\.metis/\.llm-oauth-[^/]+\.tmp`,
		`(deny file-read* (literal "/Users/test/.metis/mcp-oauth.json"))`,
		`(deny file-read* (literal "/Users/test/.metis/mcp.toml"))`,
		`(deny file-read* (literal "/Users/test/.metis/config.local.toml"))`,
		`(deny file-read* (subpath "/Users/test/.ssh"))`,
		`(deny file-write* (subpath "/Users/test/.aws"))`,
		`(deny file-read* (subpath "/Users/test/.config/gcloud"))`,
		`(deny file-read* (subpath "/Users/test/.kube"))`,
		`(deny file-read* (literal "/Users/test/.netrc"))`,
		`(deny file-write* (literal "/Users/test/.git-credentials"))`,
	}
	for _, want := range wants {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, `(allow file-write* (subpath "/Users/test/.metis"))`) {
		t.Fatal("profile still grants broad ~/.metis write access")
	}
	blocked := buildDarwinProfile(cwd, tempDir, home, filepath.Join(home, ".metis"), NetworkBlock)
	if strings.Contains(blocked, "network*") {
		t.Fatalf("network=block profile contains network permission:\n%s", blocked)
	}
}

func TestDarwinProfileProtectsLLMOAuthInCustomAndDefaultHomes(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "test")
	custom := filepath.Join(string(filepath.Separator), "Volumes", "private", "metis-state")
	profile := buildDarwinProfile("/work/repo", "/private/tmp/metis-sandbox", home, custom, NetworkAllow)
	for _, root := range []string{filepath.Join(home, ".metis"), custom} {
		privateDir := filepath.Join(root, metisCredentialDirectoryName)
		wantPrivateDir := `(deny file-read* (subpath "` + privateDir + `"))`
		if !strings.Contains(profile, wantPrivateDir) {
			t.Errorf("profile missing private credential directory rule %q:\n%s", wantPrivateDir, profile)
		}
		for _, path := range []string{
			filepath.Join(root, "llm-oauth.json"),
			filepath.Join(root, ".llm-oauth.lock"),
		} {
			want := `(deny file-read* (literal "` + path + `"))`
			if !strings.Contains(profile, want) {
				t.Errorf("profile missing %q:\n%s", want, profile)
			}
		}
		for _, suffix := range []string{
			`\.llm-oauth-refresh-[^/]+\.lock`,
			`\.llm-oauth-[^/]+\.tmp`,
		} {
			wantPattern := regexp.QuoteMeta(root+string(filepath.Separator)) + suffix
			if !strings.Contains(profile, wantPattern) {
				t.Errorf("profile missing LLM OAuth sidecar rule %q:\n%s", wantPattern, profile)
			}
		}
	}
}

func TestDarwinSandboxE2E(t *testing.T) {
	if !Available() {
		t.Skipf("sandbox-exec unavailable: %v", Doctor().Err)
	}
	// Some CI/container hosts expose /usr/bin/sandbox-exec but forbid applying
	// a nested Seatbelt policy. Treat that as backend unavailability, while
	// keeping all actual policy or command failures below as hard failures.
	probe := exec.Command(darwinSandboxExecutable, "-p", "(version 1)\n(allow default)\n", "/usr/bin/true")
	if output, err := probe.CombinedOutput(); err != nil {
		if strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
			t.Skipf("host forbids applying a nested Seatbelt profile: %s", strings.TrimSpace(string(output)))
		}
		t.Fatalf("sandbox-exec preflight failed: %v: %s", err, output)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	external := t.TempDir()
	m, err := NewManagerWithOptions(Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	protected := []string{
		filepath.Join(cwd, ".git", "config"),
		filepath.Join(cwd, ".git", "hooks", "pre-commit"),
		filepath.Join(cwd, ".metis", "config.local.toml"),
		filepath.Join(cwd, ".metis", "agents", "unsafe.md"),
		filepath.Join(cwd, ".metis", "commands", "unsafe.md"),
		filepath.Join(cwd, ".metis", "skills", "unsafe.md"),
		filepath.Join(cwd, ".gitmodules"),
		filepath.Join(cwd, ".mcp.json"),
		filepath.Join(cwd, ".vscode", "settings.json"),
		filepath.Join(cwd, ".idea", "workspace.xml"),
	}
	for _, path := range protected {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	authPath := filepath.Join(home, ".metis", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	metisCredentialPaths := []string{
		authPath,
		filepath.Join(home, ".metis", "llm-oauth.json"),
		filepath.Join(home, ".metis", ".llm-oauth.lock"),
		filepath.Join(home, ".metis", ".llm-oauth-refresh-fixture.lock"),
		filepath.Join(home, ".metis", ".llm-oauth-fixture.tmp"),
	}
	for _, path := range metisCredentialPaths {
		if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sshKeyPath := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.MkdirAll(filepath.Dir(sshKeyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshKeyPath, []byte("ssh-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(script string, env ...string) error {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), env...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		wrapped, err := m.Wrap(cmd, Request{Cwd: cwd})
		if err != nil {
			return err
		}
		if err := wrapped.Run(); err != nil {
			return fmtRunError(err, stderr.String())
		}
		return nil
	}

	allowed := filepath.Join(cwd, "allowed.txt")
	if err := run(`printf allowed > "$TARGET"`, "TARGET="+allowed); err != nil {
		t.Fatalf("cwd write unexpectedly failed: %v", err)
	}
	if got, err := os.ReadFile(allowed); err != nil || string(got) != "allowed" {
		t.Fatalf("allowed write result = %q, %v", got, err)
	}
	privateTempFile := filepath.Join(m.TempDir(), "scratch.txt")
	if err := run(`printf scratch > "$TARGET"`, "TARGET="+privateTempFile); err != nil {
		t.Fatalf("private temp write unexpectedly failed: %v", err)
	}
	if got, err := os.ReadFile(privateTempFile); err != nil || string(got) != "scratch" {
		t.Fatalf("private temp write result = %q, %v", got, err)
	}

	externalPath := filepath.Join(external, "blocked.txt")
	if err := run(`printf blocked > "$TARGET"`, "TARGET="+externalPath); err == nil {
		t.Fatal("sandbox allowed write outside cwd/private temp")
	}
	if _, err := os.Stat(externalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file exists or stat failed unexpectedly: %v", err)
	}

	for _, path := range protected {
		if err := run(`printf changed > "$TARGET"`, "TARGET="+path); err == nil {
			t.Errorf("sandbox allowed protected write: %s", path)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "original\n" {
			t.Errorf("protected file %s changed: %q, %v", path, got, err)
		}
	}
	for _, path := range metisCredentialPaths {
		if err := run(`cat "$SECRET" >/dev/null`, "SECRET="+path); err == nil {
			t.Errorf("sandbox allowed reading Metis credential %s", path)
		}
	}
	// The command deliberately contains no contiguous `.metis/auth.json`
	// fragment. The OS boundary must still reject the dynamically assembled
	// path; string inspection alone cannot enforce the credential invariant.
	if err := run(`d=.me"tis"; f=auth."json"; cat "$HOME/$d/$f" >/dev/null`); err == nil {
		t.Fatal("sandbox allowed dynamically assembled Metis credential read")
	}
	if err := run(`d=.me"tis"; f=.llm-oauth-fixture."tmp"; cat "$HOME/$d/$f" >/dev/null`); err == nil {
		t.Fatal("sandbox allowed dynamically assembled LLM OAuth temporary-file read")
	}
	if err := run(`cat "$SECRET" >/dev/null`, "SECRET="+sshKeyPath); err == nil {
		t.Fatal("sandbox allowed reading ~/.ssh private key")
	}
	// Home is itself a valid cwd, so the explicit ~/.metis deny must override
	// the broad cwd write grant rather than relying on default-deny.
	homeCmd := exec.Command("/bin/sh", "-c", `printf changed > "$SECRET"`)
	homeCmd.Dir = home
	homeCmd.Env = append(os.Environ(), "SECRET="+authPath)
	wrappedHome, err := m.Wrap(homeCmd, Request{Cwd: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrappedHome.Run(); err == nil {
		t.Fatal("sandbox allowed writing ~/.metis/auth.json")
	}
	if got, err := os.ReadFile(authPath); err != nil || string(got) != "secret" {
		t.Fatalf("auth credential changed: %q, %v", got, err)
	}
	homeSSHCmd := exec.Command("/bin/sh", "-c", `printf changed > "$SECRET"`)
	homeSSHCmd.Dir = home
	homeSSHCmd.Env = append(os.Environ(), "SECRET="+sshKeyPath)
	wrappedHomeSSH, err := m.Wrap(homeSSHCmd, Request{Cwd: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrappedHomeSSH.Run(); err == nil {
		t.Fatal("sandbox allowed writing ~/.ssh private key")
	}
	if got, err := os.ReadFile(sshKeyPath); err != nil || string(got) != "ssh-secret" {
		t.Fatalf("ssh credential changed: %q, %v", got, err)
	}
	homeRC := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(homeRC, []byte("original-rc"), 0o600); err != nil {
		t.Fatal(err)
	}
	homeRCCmd := exec.Command("/bin/sh", "-c", `printf changed > "$TARGET"`)
	homeRCCmd.Dir = home
	homeRCCmd.Env = append(os.Environ(), "TARGET="+homeRC)
	wrappedHomeRC, err := m.Wrap(homeRCCmd, Request{Cwd: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrappedHomeRC.Run(); err == nil {
		t.Fatal("sandbox allowed writing ~/.zshrc when cwd is home")
	}
	if got, err := os.ReadFile(homeRC); err != nil || string(got) != "original-rc" {
		t.Fatalf("shell rc changed: %q, %v", got, err)
	}
}

func TestDarwinSandboxNetworkBlockIsKernelEnforced(t *testing.T) {
	if !Available() {
		t.Skipf("sandbox-exec unavailable: %v", Doctor().Err)
	}
	if _, err := os.Stat("/usr/bin/nc"); err != nil {
		t.Skip("/usr/bin/nc unavailable")
	}
	probe := exec.Command(darwinSandboxExecutable, "-p", "(version 1)\n(allow default)\n", "/usr/bin/true")
	if output, err := probe.CombinedOutput(); err != nil {
		if strings.Contains(string(output), "sandbox_apply: Operation not permitted") {
			t.Skipf("host forbids applying a nested Seatbelt profile: %s", strings.TrimSpace(string(output)))
		}
		t.Fatalf("sandbox-exec preflight failed: %v: %s", err, output)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	manager, err := NewManagerWithOptions(Options{
		Mode: "permissions", Network: NetworkBlock, TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	cmd := exec.Command("/usr/bin/nc", "-z", "-w", "1", host, port)
	cmd.Dir = cwd
	wrapped, err := manager.Wrap(cmd, Request{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Run(); err == nil {
		t.Fatal("network=block allowed a local TCP connection")
	}
}

func fmtRunError(err error, stderr string) error {
	return errors.New(err.Error() + ": " + strings.TrimSpace(stderr))
}
