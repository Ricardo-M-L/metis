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
	if !strings.HasPrefix(c.Description, "[user] ") {
		t.Errorf("description must be prefixed with [user]; got %q", c.Description)
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
	if c.Description != "[user] My custom check" {
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
func TestParseFrontMatter_OtherKeysPassThrough(t *testing.T) {
	src := "---\ndescription: short\nmodel: claude-haiku\n---\nbody text\n"
	desc, body := parseFrontMatter(src)
	if desc != "short" {
		t.Errorf("description = %q, want \"short\"", desc)
	}
	if body != "body text\n" {
		t.Errorf("body = %q, want \"body text\\n\" (front matter must be stripped fully)", body)
	}
}
