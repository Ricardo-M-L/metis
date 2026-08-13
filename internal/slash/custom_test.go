package slash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadCustomCommands_BasicMarkdown — happy path: a single .md file
// in ~/.metis/commands/ becomes a slash command whose handler returns
// the body as a SignalCustomPrompt.
func TestLoadCustomCommands_BasicMarkdown(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "Run go test ./... and report only failures.\n"
	if err := os.WriteFile(filepath.Join(cmdDir, "precommit.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry()
	loaded := LoadCustomCommands(r, dir)

	if len(loaded) != 1 || loaded[0] != "precommit" {
		t.Fatalf("loaded names mismatch: got %v, want [precommit]", loaded)
	}
	c, ok := r.Get("precommit")
	if !ok {
		t.Fatal("/precommit not registered")
	}
	if c.Description != strings.TrimSpace(body) {
		t.Errorf("description should not duplicate Source metadata; got %q", c.Description)
	}
	if !c.Trusted {
		t.Fatal("user-level custom command must be marked trusted")
	}
	prompt, sig := c.Handler("")
	if sig != SignalCustomPrompt {
		t.Errorf("handler must return SignalCustomPrompt; got %d", sig)
	}
	if prompt != "Run go test ./... and report only failures." {
		t.Errorf("prompt body mismatch: %q", prompt)
	}
}

// TestLoadCustomCommands_FrontMatter — `---\ndescription: …\n---\nbody`
// strips the front matter from the prompt body and uses the description
// in /help.
func TestLoadCustomCommands_FrontMatter(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, "commands")
	_ = os.MkdirAll(cmdDir, 0o755)
	src := "---\ndescription: My custom check\n---\nDo the thing with $ARGUMENTS.\n"
	_ = os.WriteFile(filepath.Join(cmdDir, "check.md"), []byte(src), 0o644)

	r := NewRegistry()
	LoadCustomCommands(r, dir)

	c, ok := r.Get("check")
	if !ok {
		t.Fatal("/check not registered")
	}
	if c.Description != "My custom check" {
		t.Errorf("description from front matter mismatch: %q", c.Description)
	}
	prompt, _ := c.Handler("the urgent stuff")
	if prompt != "Do the thing with the urgent stuff." {
		t.Errorf("$ARGUMENTS substitution failed; got %q", prompt)
	}
	if strings.Contains(prompt, "description:") || strings.Contains(prompt, "---") {
		t.Errorf("front matter leaked into prompt body: %q", prompt)
	}
}

// TestLoadCustomCommands_PositionalArgs — $1 / $2 substitution. Out-of-
// range positional refs collapse to empty (matches bash $@ semantics).
func TestLoadCustomCommands_PositionalArgs(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, "commands")
	_ = os.MkdirAll(cmdDir, 0o755)
	src := "First=$1, Second=$2, Third=$3, All=$ARGUMENTS"
	_ = os.WriteFile(filepath.Join(cmdDir, "args.md"), []byte(src), 0o644)

	r := NewRegistry()
	LoadCustomCommands(r, dir)

	c, _ := r.Get("args")
	prompt, _ := c.Handler("alpha beta")
	want := "First=alpha, Second=beta, Third=, All=alpha beta"
	if prompt != want {
		t.Errorf("positional substitution mismatch:\n got: %q\nwant: %q", prompt, want)
	}
}

// TestLoadCustomCommands_RefusesShadowingBuiltins — a user .md named
// `help.md` must NOT silently shadow the built-in /help. Without this
// guard, a typo'd file could brick a critical command.
func TestLoadCustomCommands_RefusesShadowingBuiltins(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, "commands")
	_ = os.MkdirAll(cmdDir, 0o755)
	_ = os.WriteFile(filepath.Join(cmdDir, "help.md"), []byte("imposter help"), 0o644)

	r := NewRegistry()
	r.Register(Cmd{Name: "help", Description: "REAL help", Handler: func(_ string) (string, Signal) {
		return "real help text", SignalNone
	}})
	loaded := LoadCustomCommands(r, dir)

	if len(loaded) != 0 {
		t.Errorf("help.md must be refused to avoid shadowing built-in; got loaded=%v", loaded)
	}
	c, _ := r.Get("help")
	if !strings.Contains(c.Description, "REAL") {
		t.Errorf("built-in /help was overwritten by user .md; description now %q", c.Description)
	}
}

func TestLoadCustomCommands_RefusesNamesReservedByTUI(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, "commands")
	_ = os.MkdirAll(cmdDir, 0o755)
	_ = os.WriteFile(filepath.Join(cmdDir, "fast.md"), []byte("shadow fast"), 0o644)

	r := NewRegistry()
	r.Reserve("fast")
	if loaded := LoadCustomCommands(r, dir); len(loaded) != 0 {
		t.Fatalf("reserved TUI command loaded as custom: %v", loaded)
	}
	if _, ok := r.Get("fast"); ok {
		t.Fatal("reserved name should not create a callable slash entry")
	}
}

func TestRemoveCustomReflectsDeletionWithoutDroppingBuiltins(t *testing.T) {
	r := NewRegistry()
	r.Register(Cmd{Name: "help", Handler: func(string) (string, Signal) { return "help", SignalNone }})
	r.Register(Cmd{Name: "check", Custom: true, Handler: func(string) (string, Signal) { return "old", SignalCustomPrompt }})

	r.RemoveCustom()
	if _, ok := r.Get("check"); ok {
		t.Fatal("RemoveCustom retained stale custom command")
	}
	if _, ok := r.Get("help"); !ok {
		t.Fatal("RemoveCustom dropped built-in command")
	}
}

// TestLoadCustomCommands_SkipsHiddenAndJunk — `.gitkeep`, `_template.md`,
// and `.DS_Store` should NOT register as commands.
func TestLoadCustomCommands_SkipsHiddenAndJunk(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, "commands")
	_ = os.MkdirAll(cmdDir, 0o755)
	for _, junk := range []string{".gitkeep.md", "_template.md", ".DS_Store"} {
		_ = os.WriteFile(filepath.Join(cmdDir, junk), []byte("junk"), 0o644)
	}
	_ = os.WriteFile(filepath.Join(cmdDir, "real.md"), []byte("real prompt"), 0o644)

	r := NewRegistry()
	loaded := LoadCustomCommands(r, dir)

	if len(loaded) != 1 || loaded[0] != "real" {
		t.Errorf("only `real` should load; got %v", loaded)
	}
}

// TestLoadCustomCommands_MissingDir — no commands/ dir means no error,
// no commands. Must not crash on first run.
func TestLoadCustomCommands_MissingDir(t *testing.T) {
	r := NewRegistry()
	loaded := LoadCustomCommands(r, "/nonexistent/path/xxx")
	if len(loaded) != 0 {
		t.Errorf("missing dir should silently load 0; got %v", loaded)
	}
}

// TestParseFrontMatter_NoYAML — `description:` is a plain key/value line,
// not full YAML. Other keys (model:, tools:, etc.) get preserved into
// the body so future fields don't silently disappear.
func TestParseFrontMatter_RecognizedKeys(t *testing.T) {
	src := "---\ndescription: short\nmodel: claude-haiku\nargument-hint: [pr]\nallowed-tools: Bash, Read\nunknown: ignored\n---\nbody text\n"
	meta, body := parseFrontMatter(src)
	if meta.description != "short" {
		t.Errorf("description = %q, want \"short\"", meta.description)
	}
	if meta.model != "claude-haiku" {
		t.Errorf("model = %q, want \"claude-haiku\"", meta.model)
	}
	if meta.argumentHint != "[pr]" {
		t.Errorf("argumentHint = %q, want \"[pr]\"", meta.argumentHint)
	}
	if len(meta.allowedTools) != 2 || meta.allowedTools[0] != "Bash" || meta.allowedTools[1] != "Read" {
		t.Errorf("allowedTools = %v, want [Bash Read]", meta.allowedTools)
	}
	if body != "body text\n" {
		t.Errorf("body = %q, want \"body text\\n\" (front matter must be stripped fully)", body)
	}
}

func TestRenderTemplate_BashInjection(t *testing.T) {
	out := renderTemplate("before !`echo hello` after", "", true)
	if out != "before hello after" {
		t.Errorf("bash injection: got %q, want \"before hello after\"", out)
	}
	// Failure is surfaced inline, not dropped.
	got := renderTemplate("x !`exit 3` y", "", true)
	if !strings.Contains(got, "exited") && !strings.Contains(got, "failed") {
		t.Errorf("failed command should surface an error note; got %q", got)
	}
}

func TestRenderTemplate_UntrustedSkipsInjection(t *testing.T) {
	// Project-local (untrusted) commands must NOT execute !`cmd` or read
	// @file — the injections stay as inert literal text.
	out := renderTemplate("run !`echo SHOULD_NOT_RUN` here", "", false)
	if strings.Contains(out, "SHOULD_NOT_RUN\n") || !strings.Contains(out, "!`echo SHOULD_NOT_RUN`") {
		t.Errorf("untrusted bash injection must stay literal; got %q", out)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "secret.txt")
	_ = os.WriteFile(f, []byte("TOPSECRET"), 0o644)
	out2 := renderTemplate("leak @"+f, "", false)
	if strings.Contains(out2, "TOPSECRET") {
		t.Errorf("untrusted file injection must not read the file; got %q", out2)
	}
}

func TestRenderTemplate_FileInjection(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(f, []byte("FILE BODY"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := renderTemplate("see @"+f+" end", "", true)
	if !strings.Contains(out, "FILE BODY") {
		t.Errorf("file injection failed; got %q", out)
	}
	// A non-file @token (email-shaped) is left untouched.
	out2 := renderTemplate("ping user@example.com now", "", true)
	if !strings.Contains(out2, "user@example.com") {
		t.Errorf("non-file @token should be preserved; got %q", out2)
	}
}

func TestLoadCustomCommands_FrontMatterOverrides(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, "commands")
	_ = os.MkdirAll(cmdDir, 0o755)
	src := "---\ndescription: review a PR\nargument-hint: [pr-number]\nallowed-tools: Bash, Read\nmodel: claude-opus-4-8\n---\nReview PR $1.\n"
	_ = os.WriteFile(filepath.Join(cmdDir, "rev.md"), []byte(src), 0o644)

	r := NewRegistry()
	LoadCustomCommands(r, dir)
	c, ok := r.Get("rev")
	if !ok {
		t.Fatal("/rev not registered")
	}
	if c.ArgumentHint != "[pr-number]" {
		t.Errorf("ArgumentHint = %q, want [pr-number]", c.ArgumentHint)
	}
	if c.Source != "user" || c.Category != "custom" {
		t.Errorf("custom command metadata source/category = %q/%q", c.Source, c.Category)
	}
	if c.Model != "claude-opus-4-8" {
		t.Errorf("Model override = %q", c.Model)
	}
	if len(c.AllowedTools) != 2 {
		t.Errorf("AllowedTools = %v", c.AllowedTools)
	}
}

func TestLoadCustomCommands_ProjectCommandIsNotTrustedForOverrides(t *testing.T) {
	project := t.TempDir()
	commandDir := filepath.Join(project, ".metis", "commands")
	if err := os.MkdirAll(commandDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "---\nallowed-tools: Bash(git status:*), Write\nmodel: other-model\n---\nInspect the project.\n"
	if err := os.WriteFile(filepath.Join(commandDir, "inspect.md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldCwd) }()

	r := NewRegistry()
	LoadCustomCommands(r, t.TempDir())
	cmd, ok := r.Get("inspect")
	if !ok {
		t.Fatal("project command not loaded")
	}
	if cmd.Trusted {
		t.Fatal("project-level command must not be trusted for permission/model overrides")
	}
	if len(cmd.AllowedTools) != 2 || cmd.Model != "other-model" {
		t.Fatalf("metadata should remain available for an explicit ignored-warning: %+v", cmd)
	}
}
