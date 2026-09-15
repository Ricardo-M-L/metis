package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles"
)

func TestSessionFilesCommandListAndShow(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: root}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "中文 报告.md"), []byte("# Report\npassword=test-cli-secret\n\x1b]52;c;evil\x07"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("a", llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "[报告](<中文 报告.md>)"}}}); err != nil {
		t.Fatal(err)
	}
	service := sessionfiles.New(store)
	var out bytes.Buffer
	if err := runSessionFilesCommand([]string{"list", "--session", "a", "--json"}, &out, service); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Files []sessionfiles.File `json:"files"`
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil || len(listed.Files) != 1 {
		t.Fatalf("list=%s %v", out.String(), err)
	}
	for _, jsonFlag := range []bool{false, true} {
		out.Reset()
		args := []string{"show", listed.Files[0].ID, "--session=a"}
		if jsonFlag {
			args = append(args, "--json")
		}
		if err := runSessionFilesCommand(args, &out, service); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "# Report") || strings.Contains(out.String(), "test-cli-secret") || strings.Contains(out.String(), "evil") || strings.Contains(out.String(), "\x1b") {
			t.Fatalf("unsafe CLI preview=%q", out.String())
		}
		if jsonFlag {
			var preview sessionfiles.Preview
			if err := json.Unmarshal(out.Bytes(), &preview); err != nil || preview.File.ID != listed.Files[0].ID {
				t.Fatalf("JSON=%s %v", out.String(), err)
			}
		}
	}
	out.Reset()
	if err := runSessionFilesCommand([]string{"list", "--session", "a"}, &out, service); err != nil || !strings.Contains(out.String(), "中文 报告.md") {
		t.Fatalf("text list=%q %v", out.String(), err)
	}
	if err := runSessionFilesCommand([]string{"show", listed.Files[0].ID, "--session", "b"}, &out, service); err == nil {
		t.Fatal("cross-session CLI read succeeded")
	}
}

func TestSessionFilesCommandRejectsUnknownOrIncompleteArguments(t *testing.T) {
	for _, args := range [][]string{
		{"list"}, {"list", "--session"}, {"list", "--session", "--json"},
		{"list", "--session", "a", "extra"}, {"show", "--session", "a"},
		{"show", "file", "extra", "--session", "a"}, {"delete", "file", "--session", "a"},
		{"list", "--session", "a", "--path", "/etc/passwd"}, {"list", "--sessions-dir"},
	} {
		var out bytes.Buffer
		if err := runSessionFilesCommand(args, &out, nil); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	var out bytes.Buffer
	if err := runSessionFilesCommand([]string{"--help"}, &out, nil); err != nil || !strings.Contains(out.String(), "metis files show") {
		t.Fatalf("help=%q %v", out.String(), err)
	}
}

func TestSessionFilesCLIStoreSelectionIsReadOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("METIS_HOME", filepath.Join(root, "nonexistent-home"))
	opts, err := parseSessionFilesCommand([]string{"list", "--session", "a"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionFilesCommandService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.List("a"); err == nil {
		t.Fatal("missing store should fail without creating it")
	}
	if _, err := os.Stat(filepath.Join(root, "nonexistent-home")); !os.IsNotExist(err) {
		t.Fatalf("command initialized user home: %v", err)
	}
	dir := filepath.Join(root, "custom sessions")
	fixture, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.WriteHeaderFull(session.Header{ID: "a", WorkDir: root}); err != nil {
		t.Fatal(err)
	}
	opts, err = parseSessionFilesCommand([]string{"list", "--session", "a", "--sessions-dir", dir})
	if err != nil {
		t.Fatal(err)
	}
	store, err = sessionFilesCommandService(opts)
	if err != nil {
		t.Fatal(err)
	}
	if files, err := store.List("a"); err != nil || len(files) != 0 {
		t.Fatalf("custom store=%+v %v", files, err)
	}
}

// Opt in to rebuilding and exercising the real dispatcher, with all session
// state and output confined to temporary directories and no provider required.
func TestSessionFilesBinaryIntegration(t *testing.T) {
	if os.Getenv("METIS_TEST_SESSION_FILES_CLI") != "1" {
		t.Skip("set METIS_TEST_SESSION_FILES_CLI=1 to build and exercise the real CLI")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "metis")
	if goruntime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/metis")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real CLI: %v\n%s", err, out)
	}
	home := filepath.Join(root, "metis-home")
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"preview-a", "preview-b"} {
		if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: root}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "二进制 CLI 报告.md")
	const marker = "REAL_CLI_PREVIEW_OK"
	const secret = "cli-fixture-secret-do-not-emit"
	if err := os.WriteFile(path, []byte("# "+marker+"\npassword="+secret+"\n\x1b]52;c;Y2xpcGJvYXJk\x07visible\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("preview-a", llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "[报告](<" + path + ">)"}}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(home, "sessions", "preview-a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "METIS_HOME=") {
			env = append(env, entry)
		}
	}
	env = append(env, "METIS_HOME="+home)
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(binary, args...)
		cmd.Dir = root
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	list, stderr, err := run("files", "list", "--session", "preview-a", "--json")
	if err != nil {
		t.Fatalf("CLI list: %v %s", err, stderr)
	}
	var listed struct {
		Files []sessionfiles.File `json:"files"`
	}
	if err := json.Unmarshal([]byte(list), &listed); err != nil || len(listed.Files) != 1 {
		t.Fatalf("CLI JSON list: %v %s", err, list)
	}
	for _, jsonFlag := range []bool{false, true} {
		args := []string{"files", "show", listed.Files[0].ID, "--session", "preview-a"}
		if jsonFlag {
			args = append(args, "--json")
		}
		stdout, stderr, err := run(args...)
		if err != nil {
			t.Fatalf("CLI show: %v %s", err, stderr)
		}
		if !strings.Contains(stdout, marker) || strings.Contains(stdout, secret) || strings.Contains(stdout, "\x1b") || strings.Contains(stdout, "Y2xpcGJvYXJk") {
			t.Fatalf("unsafe binary preview: %q", stdout)
		}
		if jsonFlag {
			var preview sessionfiles.Preview
			if err := json.Unmarshal([]byte(stdout), &preview); err != nil || preview.File.ID != listed.Files[0].ID {
				t.Fatalf("CLI JSON preview: %v %s", err, stdout)
			}
		}
	}
	if stdout, _, err := run("files", "show", listed.Files[0].ID, "--session", "preview-b"); err == nil || strings.Contains(stdout, marker) {
		t.Fatalf("CLI cross-session read: err=%v stdout=%q", err, stdout)
	}
	after, err := os.ReadFile(filepath.Join(home, "sessions", "preview-a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("CLI preview changed session transcript")
	}
	t.Log("real binary: 1 listed file; plain and JSON previews contain expected marker; credentials and ESC/OSC payload absent; cross-session denied; transcript unchanged")
}
