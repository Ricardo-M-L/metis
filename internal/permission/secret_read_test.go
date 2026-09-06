package permission

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

// Reading a credential file must be gated in modes that otherwise auto-allow
// read-only tools. Interactive modes ASK; bypass silently denies.
func TestGate_SecretReadGatedAcrossModes(t *testing.T) {
	secrets := []string{
		"/Users/x/.ssh/id_ed25519",
		"/Users/x/.ssh/id_rsa",
		"/home/y/.aws/credentials",
		"/root/.kube/config",
		"/Users/x/.gnupg/secring.gpg",
		"/Users/x/.metis/auth.json",
		"/Users/x/.metis/.credentials/auth.json",
		"/Users/x/.metis/.credentials/future-secret.bin",
		"/Users/x/.metis/llm-oauth.json",
		"/Users/x/.metis/.llm-oauth.lock",
		"/Users/x/.metis/.llm-oauth-refresh-0123456789abcdef.lock",
		"/Users/x/.metis/.llm-oauth-session.tmp",
		"/Users/x/.metis/.auth.json.session",
		"/Users/x/.metis/mcp-oauth.json",
		"/Users/x/.metis/.mcp-oauth.lock",
		"/Users/x/.metis/.mcp-oauth-refresh-0123456789abcdef.lock",
		"/Users/x/.metis/.mcp-oauth-session.tmp",
		"/Users/x/.metis/mcp.toml",
		"/Users/x/.metis/config.toml",
		"/work/project/.metis/config.local.toml",
		"/work/project/.env",
		"/work/project/.env.production",
		"/work/project/.env.development.local",
		"/Users/x/.npmrc",
		"/Users/x/.pypirc",
		"/Users/x/.git-credentials",
		"/Users/x/.config/gh/hosts.yml",
	}
	normal := []string{
		"/Users/x/project/main.go",
		"/Users/x/notes/.env.example",
		"/etc/hosts",
	}
	for _, mode := range []Mode{ModeAsk, ModeAcceptEdits, ModeBypass} {
		g := New(mode)
		for _, p := range secrets {
			want := DecisionAsk
			if mode == ModeBypassPermissions {
				want = DecisionDeny
			}
			if d, _ := g.Check(context.Background(), "Read", p); d != want {
				t.Errorf("mode=%v Read(%q): want %v, got %v", mode, p, want, d)
			}
		}
		for _, p := range normal {
			if d, _ := g.Check(context.Background(), "Read", p); d == DecisionAsk {
				t.Errorf("mode=%v Read(%q): normal file should not be gated, got ASK", mode, p)
			}
		}
	}
}

func TestGate_MetisCredentialFilesProtectedFromReadGrepAndBash(t *testing.T) {
	paths := []string{
		"/Users/x/.metis/auth.json",
		"/Users/x/.metis/llm-oauth.json",
		"/Users/x/.metis/.llm-oauth.lock",
		"/Users/x/.metis/.llm-oauth-refresh-0123456789abcdef.lock",
		"/Users/x/.metis/.llm-oauth-session.tmp",
		"/Users/x/.metis/.auth.json.session",
		"/Users/x/.metis/mcp-oauth.json",
		"/Users/x/.metis/.mcp-oauth.lock",
		"/Users/x/.metis/.mcp-oauth-session.tmp",
		"/Users/x/.metis/mcp.toml",
		"/Users/x/.metis/config.toml",
		"/work/project/.metis/config.local.toml",
		`C:\Users\x\.metis\auth.json`,
		`C:\Users\x\.metis\.credentials\auth.json`,
		`C:\Users\x\.metis\.credentials\future-secret.bin`,
		`C:\Users\x\.metis\llm-oauth.json`,
		`C:\Users\x\.metis\.llm-oauth-refresh-0123456789abcdef.lock`,
	}
	for _, mode := range []Mode{ModeAsk, ModeBypassPermissions} {
		want := DecisionAsk
		if mode == ModeBypassPermissions {
			want = DecisionDeny
		}
		g := New(mode)
		for _, path := range paths {
			for _, tc := range []struct {
				tool  string
				input string
			}{
				{tool: "Read", input: path},
				{tool: "Grep", input: path},
				{tool: "Bash", input: `cat "` + path + `"`},
			} {
				decision, source := g.Check(context.Background(), tc.tool, tc.input)
				if decision != want {
					t.Errorf("mode=%v %s(%q) = %v (%s), want %v",
						mode, tc.tool, tc.input, decision, source, want)
				}
				if source != "secret_read:bypass_immune" && source != "safety_check:bypass_immune" {
					t.Errorf("mode=%v %s(%q) protected by unexpected source %q",
						mode, tc.tool, tc.input, source)
				}
			}
		}
	}
}

func TestGate_PrivateCredentialDirectoryIsProtectedAsNamespace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("METIS_HOME", root)
	credentialDir := filepath.Join(root, ".credentials")
	g := New(ModeBypassPermissions)
	for _, tc := range []struct {
		tool  string
		input string
	}{
		{tool: "Read", input: filepath.Join(credentialDir, "auth.json")},
		{tool: "Read", input: filepath.Join(credentialDir, "future-random-name.bin")},
		{tool: "Grep", input: credentialDir},
		{tool: "RunCode", input: `print(open("` + filepath.Join(credentialDir, "future-random-name.bin") + `").read())`},
		{tool: "Bash", input: `cat "$METIS_HOME/.credentials/llm-oauth.json"`},
		{tool: "Bash", input: `cat "$METIS_HOME/.credentials/.mcp-oauth-session.tmp"`},
	} {
		decision, source := g.Check(context.Background(), tc.tool, tc.input)
		if decision != DecisionDeny || source != "secret_read:bypass_immune" {
			t.Errorf("%s(%q) = %v (%s), want private-directory secret deny", tc.tool, tc.input, decision, source)
		}
	}
}

func TestGate_LLMOAuthSidecarMatchingIsNarrow(t *testing.T) {
	g := New(ModeBypassPermissions)
	for _, path := range []string{
		"/Users/x/.metis/.llm-oauth.lock",
		"/Users/x/.metis/.llm-oauth-refresh-0123456789abcdef.lock",
		"/Users/x/.metis/.llm-oauth-session.tmp",
	} {
		if decision, source := g.Check(context.Background(), "Read", path); decision != DecisionDeny || source != "secret_read:bypass_immune" {
			t.Errorf("Read(%q) = %v (%s), want silent credential deny", path, decision, source)
		}
	}
	for _, path := range []string{
		"/Users/x/project/.llm-oauth-session.tmp",
		"/Users/x/.metis/.llm-oauth-notes.md",
		"/Users/x/.metis/.llm-oauth-refresh-status.txt",
	} {
		if decision, source := g.Check(context.Background(), "Read", path); decision == DecisionDeny || source == "secret_read:bypass_immune" {
			t.Errorf("ordinary Read(%q) unexpectedly treated as an OAuth credential: %v (%s)", path, decision, source)
		}
	}
}

func TestGate_RunCodeCredentialReadUsesFullSnippet(t *testing.T) {
	g := New(ModeBypassPermissions)
	for _, code := range []string{
		`print(open("/Users/x/.metis/auth.json").read())`,
		strings.Repeat("# padding\n", 20) + `print(open("/Users/x/.metis/mcp-oauth.json").read())`,
		`print(open(r"C:\Users\x\.metis\config.toml").read())`,
		`print(open("/work/project/.env.development.local").read())`,
	} {
		decision, source := g.Check(context.Background(), "RunCode", code)
		if decision != DecisionDeny || source != "secret_read:bypass_immune" {
			t.Fatalf("RunCode credential read = %v (%s), want silent secret-read deny", decision, source)
		}
	}
	if decision, source := g.Check(context.Background(), "RunCode", `print("hello")`); decision != DecisionAllow {
		t.Fatalf("ordinary RunCode = %v (%s), want allow", decision, source)
	}
}

func TestGate_CustomMetisHomeAndSymlinkAliasRemainSecret(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := t.TempDir()
	t.Setenv("METIS_HOME", root)
	secret := filepath.Join(root, "auth.json")
	if err := os.WriteFile(secret, []byte(`{"provider":"opaque"}`), 0o600); err != nil {
		t.Fatalf("write credential fixture: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "innocent.json")
	if err := os.Symlink(secret, alias); err != nil {
		t.Fatalf("create credential symlink: %v", err)
	}

	g := New(ModeBypassPermissions)
	decision, source := g.CheckPath(context.Background(), "Read", alias, alias)
	if decision != DecisionDeny || source != "secret_read:bypass_immune" {
		t.Fatalf("Read symlink to custom METIS_HOME = %v (%s), want silent secret-read deny", decision, source)
	}
}

func TestGate_CustomMetisHomeSymlinkParentProtectsMissingLeaf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := t.TempDir()
	t.Setenv("METIS_HOME", root)
	aliasDir := filepath.Join(t.TempDir(), "settings")
	if err := os.Symlink(root, aliasDir); err != nil {
		t.Fatalf("create METIS_HOME directory symlink: %v", err)
	}
	// The leaf deliberately does not exist. CheckPath still has to resolve the
	// existing symlinked parent before applying the credential filename rule.
	alias := filepath.Join(aliasDir, "config.local.toml")

	g := New(ModeBypassPermissions)
	decision, source := g.CheckPath(context.Background(), "Read", alias, alias)
	if decision != DecisionDeny || source != "secret_read:bypass_immune" {
		t.Fatalf("Read missing credential through symlinked parent = %v (%s), want silent deny", decision, source)
	}
}

func TestGate_CustomMetisHomeGrepRootIsSecret(t *testing.T) {
	root := t.TempDir()
	t.Setenv("METIS_HOME", root)
	g := New(ModeBypassPermissions)
	input := strings.TrimRight(root, "/") + "/\ntoken"
	decision, source := g.CheckPath(context.Background(), "Grep", input, root)
	if decision != DecisionDeny || source != "secret_read:bypass_immune" {
		t.Fatalf("Grep relocated METIS_HOME = %v (%s), want silent deny", decision, source)
	}
}

func TestGate_CustomMetisHomeShellVariablePathsRemainSecret(t *testing.T) {
	root := t.TempDir()
	t.Setenv("METIS_HOME", root)
	g := New(ModeBypassPermissions)
	for _, command := range []string{
		`cat "$METIS_HOME/auth.json"`,
		`cat "$METIS_HOME/llm-oauth.json"`,
		`cat "$METIS_HOME/.llm-oauth.lock"`,
		`cat "$METIS_HOME/.llm-oauth-refresh-0123456789abcdef.lock"`,
		`cat "$METIS_HOME/.llm-oauth-session.tmp"`,
		`Get-Content "$env:METIS_HOME\\llm-oauth.json"`,
		`Get-Content "%METIS_HOME%\\.llm-oauth-session.tmp"`,
		`cat "${METIS_HOME}/mcp-oauth.json"`,
		`cat $METIS_HOME/config.toml`,
		`cat "$env:METIS_HOME\\mcp.toml"`,
		`cat "%METIS_HOME%\\config.local.toml"`,
		`Get-Content "$Env:metis_home\\AUTH.JSON"`,
		`type "%metis_home%\\sub\\..\\mcp-oauth.json"`,
		`cat "$metis_home/sub/../auth.json"`,
		`cat "${METIS_HOME}"/auth.json`,
		`cat "$METIS_HOME"'/mcp-oauth.json'`,
		`cat "` + root + `"/config.toml`,
		`cat '` + root + `'"/config.local.toml"`,
	} {
		decision, source := g.Check(context.Background(), "Bash", command)
		if decision != DecisionDeny || (source != "secret_read:bypass_immune" && source != "safety_check:bypass_immune") {
			t.Errorf("Bash(%q) = %v (%s), want silent credential-path deny", command, decision, source)
		}
	}
}

func TestGate_DarwinPrivateAliasProtectsLLMOAuthSidecars(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("/private path aliases are specific to macOS")
	}
	t.Setenv("METIS_HOME", "/private/var/tmp/metis-private-state")
	g := New(ModeBypassPermissions)
	for _, path := range []string{
		"/var/tmp/metis-private-state/llm-oauth.json",
		"/var/tmp/metis-private-state/.llm-oauth-refresh-fixture.lock",
		"/var/tmp/metis-private-state/.llm-oauth-fixture.tmp",
	} {
		decision, source := g.Check(context.Background(), "Read", path)
		if decision != DecisionDeny || source != "secret_read:bypass_immune" {
			t.Errorf("Read(%q) = %v (%s), want macOS alias credential deny", path, decision, source)
		}
	}
}

func TestGate_CustomMetisHomeWritesRemainBypassImmune(t *testing.T) {
	root := t.TempDir()
	t.Setenv("METIS_HOME", root)
	g := New(ModeBypassPermissions)
	for _, tc := range []struct {
		tool  string
		input string
	}{
		{tool: "Bash", input: `printf compromised > "$METIS_HOME//auth.json"`},
		{tool: "Bash", input: `printf compromised > "$METIS_HOME/.llm-oauth-session.tmp"`},
		{tool: "Write", input: filepath.Join(root, "llm-oauth.json")},
		{tool: "Edit", input: filepath.Join(root, ".llm-oauth-refresh-0123456789abcdef.lock")},
		{tool: "Write", input: filepath.Join(root, "mcp.toml")},
		{tool: "Edit", input: filepath.Join(root, "config.local.toml")},
	} {
		decision, source := g.Check(context.Background(), tc.tool, tc.input)
		if decision != DecisionDeny || source != "safety_check:bypass_immune" {
			t.Errorf("%s(%q) = %v (%s), want silent safety deny", tc.tool, tc.input, decision, source)
		}
	}
}

func TestGate_RelativeMetisHomeVariableCredentialReadDenied(t *testing.T) {
	t.Setenv("METIS_HOME", ".secrets")

	gate := New(ModeBypassPermissions)
	decision, source := gate.Check(context.Background(), "Bash", `cat "$METIS_HOME/auth.json"`)
	if decision != DecisionDeny || source != "secret_read:bypass_immune" {
		t.Fatalf("relative METIS_HOME credential read = %v (%s), want silent deny", decision, source)
	}
}

func TestGate_RelativeMetisHomeLiteralCredentialReadDenied(t *testing.T) {
	t.Setenv("METIS_HOME", ".secrets")
	gate := New(ModeBypassPermissions)
	decision, source := gate.Check(context.Background(), "Bash", `cat .secrets/auth.json`)
	if decision != DecisionDeny || source != "secret_read:bypass_immune" {
		t.Fatalf("relative literal credential read = %v (%s), want silent deny", decision, source)
	}
}

func TestGate_BackslashEscapedCredentialBashReadDenied(t *testing.T) {
	root := t.TempDir()
	t.Setenv("METIS_HOME", root)
	gate := New(ModeBypassPermissions)
	command := `cat ` + filepath.Join(root, "au") + `\th.json`
	decision, source := gate.Check(context.Background(), "Bash", command)
	if decision != DecisionDeny || source != "secret_read:bypass_immune" {
		t.Fatalf("backslash-escaped credential read %q = %v (%s), want silent deny", command, decision, source)
	}
}

func TestGate_SymlinkedMetisHomeCanonicalTargetDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "metis-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", linkRoot)
	secret := filepath.Join(realRoot, "auth.json")
	gate := New(ModeBypassPermissions)
	for _, tc := range []struct {
		tool, input string
	}{
		{tool: "Read", input: secret},
		{tool: "Grep", input: secret},
		{tool: "Bash", input: `cat "` + secret + `"`},
	} {
		decision, source := gate.Check(context.Background(), tc.tool, tc.input)
		if decision != DecisionDeny || source != "secret_read:bypass_immune" {
			t.Fatalf("%s canonical credential target = %v (%s), want silent deny; matched=%v paths=%q", tc.tool, decision, source, matchesSecretReadPath(secret), metisSecretReadPaths())
		}
	}
}

func TestGate_RetargetedMetisHomeStillProtectsFrozenCredentialRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	if err := os.MkdirAll(first, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "current")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("METIS_HOME", link)
	if err := auth.Set("openai", "credential-must-remain-hidden"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	resolvedFirst, err := filepath.EvalSymlinks(first)
	if err != nil {
		t.Fatal(err)
	}
	oldCredentialPath := filepath.Join(resolvedFirst, auth.CredentialDirectoryName, "auth.json")
	decision, source := New(ModeBypassPermissions).Check(context.Background(), "Read", oldCredentialPath)
	if decision != DecisionDeny || source != "secret_read:bypass_immune" {
		t.Fatalf("Read(frozen credential root) = %v (%s), want silent deny", decision, source)
	}
}
