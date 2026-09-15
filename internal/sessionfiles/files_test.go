package sessionfiles

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

type fixtureLoader struct {
	headers  map[string]*session.Header
	messages map[string][]llm.Message
}

func (f fixtureLoader) Load(id string) (*session.Header, []llm.Message, error) {
	if h := f.headers[id]; h != nil {
		return h, f.messages[id], nil
	}
	return nil, nil, os.ErrNotExist
}

func fileFixture(t *testing.T) (*Service, fixtureLoader, string) {
	t.Helper()
	root := t.TempDir()
	f := fixtureLoader{headers: map[string]*session.Header{"a": {ID: "a", WorkDir: root}, "b": {ID: "b", WorkDir: root}}, messages: map[string][]llm.Message{}}
	return New(f), f, root
}

func putFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func toolMessages(id, name string, input map[string]any, failed bool) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: id, ToolName: name, ToolInput: input}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: id, IsError: failed, ToolResult: "done"}}},
	}
}

func assistantText(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func TestDiscoverSuccessfulWritesLinksAndDedupe(t *testing.T) {
	s, f, root := fileFixture(t)
	for _, name := range []string{"中文 报告.md", "edited.txt", "patched.md", "moved.md", "failed.md", "pending.md", "user.md", "code.md", "read.md"} {
		putFile(t, filepath.Join(root, name), "hello")
	}
	f.messages["a"] = append(f.messages["a"], toolMessages("w", "Write", map[string]any{"file_path": "中文 报告.md"}, false)...)
	f.messages["a"] = append(f.messages["a"], toolMessages("e", "Edit", map[string]any{"path": "edited.txt"}, false)...)
	f.messages["a"] = append(f.messages["a"], toolMessages("p", "apply_patch", map[string]any{"patch": "*** Begin Patch\n*** Add File: patched.md\n+# Patch\n*** Update File: old.md\n*** Move to: moved.md\n@@\n-old\n+new\n*** Delete File: deleted.md\n*** End Patch"}, false)...)
	f.messages["a"] = append(f.messages["a"], toolMessages("bad", "Write", map[string]any{"path": "failed.md"}, true)...)
	f.messages["a"] = append(f.messages["a"], toolMessages("read", "Read", map[string]any{"path": "read.md"}, false)...)
	f.messages["a"] = append(f.messages["a"], toolMessages("pending", "Write", map[string]any{"path": "pending.md"}, false)[0])
	f.messages["a"] = append(f.messages["a"], assistantText("[报告](<中文 报告.md>) and [again](%E4%B8%AD%E6%96%87%20%E6%8A%A5%E5%91%8A.md)\n```md\n[example](code.md)\n```\n[remote](https://example.com/remote.md)"))
	f.messages["a"] = append(f.messages["a"], llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "[user](user.md)"}}})
	files, err := s.List("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("files = %+v; want successful Write/Edit/patch files only", files)
	}
	for _, file := range files {
		if file.ID == "" || file.Path == "" || file.Source != "tool" {
			t.Fatalf("incomplete metadata: %+v", file)
		}
		preview, err := s.Read("a", file.ID)
		if err != nil || preview.Content != "hello" {
			t.Fatalf("read = %+v, %v", preview, err)
		}
	}
}

func TestDiscoverToolResultWithRuntimeTextAndCodeLink(t *testing.T) {
	s, f, root := fileFixture(t)
	putFile(t, filepath.Join(root, "report.md"), "report")
	putFile(t, filepath.Join(root, "example.py"), "print('hello')")
	f.messages["a"] = toolMessages("w", "Write", map[string]any{"path": "report.md"}, false)
	f.messages["a"][1].Content = append(f.messages["a"][1].Content, llm.ContentBlock{Type: "text", Text: "<runtime_state>runtime metadata</runtime_state>", Synthetic: true})
	f.messages["a"] = append(f.messages["a"], assistantText("[code](example.py)"))
	files, err := s.List("a")
	if err != nil || len(files) != 2 {
		t.Fatalf("mixed tool result and code link: %+v %v", files, err)
	}
}

func TestReadRevalidatesSessionMembershipAndIDs(t *testing.T) {
	s, f, root := fileFixture(t)
	putFile(t, filepath.Join(root, "report.md"), "private report")
	f.messages["a"] = []llm.Message{assistantText("[report](report.md)")}
	files, err := s.List("a")
	if err != nil || len(files) != 1 {
		t.Fatalf("list = %+v %v", files, err)
	}
	for _, id := range []string{files[0].ID, filepath.Join(root, "report.md"), "../../report.md"} {
		if _, err := s.Read("b", id); err == nil {
			t.Fatalf("cross-session or arbitrary path accepted: %s", id)
		}
	}
	f.messages["b"] = f.messages["a"]
	other, _ := s.List("b")
	if other[0].ID == files[0].ID {
		t.Fatal("IDs must include session identity")
	}
	f.messages["a"] = nil
	if _, err := s.Read("a", files[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed membership error = %v", err)
	}
	if _, err := s.List("../a"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("traversal session = %v", err)
	}
}

func TestLinkRootsSymlinksAndCredentialDenial(t *testing.T) {
	s, f, root := fileFixture(t)
	outside := t.TempDir()
	putFile(t, filepath.Join(outside, "outside.md"), "outside")
	putFile(t, filepath.Join(root, ".ssh", "secret.md"), "never preview")
	putFile(t, filepath.Join(root, ".env.md"), "never preview")
	putFile(t, filepath.Join(root, ".codex", "auth.json"), "never preview")
	putFile(t, filepath.Join(root, ".credentials", "private.md"), "never preview")
	putFile(t, filepath.Join(root, "ordinary.md"), "ordinary")
	if err := os.Symlink(filepath.Join(outside, "outside.md"), filepath.Join(root, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, ".ssh", "secret.md"), filepath.Join(root, "credential.md")); err != nil {
		t.Fatal(err)
	}
	f.messages["a"] = []llm.Message{assistantText("[escape](escape.md) [credential](credential.md) [dot env](.env.md) [credential dir](.credentials/private.md) [good](ordinary.md)")}
	f.messages["a"] = append(f.messages["a"], toolMessages("secret", "Write", map[string]any{"path": filepath.Join(root, ".ssh", "secret.md")}, false)...)
	f.messages["a"] = append(f.messages["a"], toolMessages("auth", "Write", map[string]any{"path": filepath.Join(root, ".codex", "auth.json")}, false)...)
	files, err := s.List("a")
	if err != nil || len(files) != 1 || files[0].Name != "ordinary.md" {
		t.Fatalf("unsafe candidates = %+v %v", files, err)
	}
	if err := os.Remove(filepath.Join(root, "ordinary.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.md"), filepath.Join(root, "ordinary.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read("a", files[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("changed symlink read = %v", err)
	}
}

func TestCanonicalTemporaryRootAlias(t *testing.T) {
	s, f, root := fileFixture(t)
	out := t.TempDir()
	alias := filepath.Join(root, "temporary-alias")
	if err := os.Symlink(out, alias); err != nil {
		t.Fatal(err)
	}
	s.tempRoots = []string{alias}
	path := filepath.Join(out, "temp.md")
	putFile(t, path, "temporary")
	f.messages["a"] = []llm.Message{assistantText("[tmp](<" + path + ">)")}
	files, err := s.List("a")
	if err != nil || len(files) != 1 {
		t.Fatalf("canonical temp alias=%+v %v", files, err)
	}
}

func TestExplicitLinksAndOutsideWrites(t *testing.T) {
	s, f, root := fileFixture(t)
	// A sibling outside the configured temp root exercises the workspace-only
	// link rule without probing any real user files.
	outside := t.TempDir()
	s.tempRoots = []string{root}
	path := filepath.Join(outside, "outside.md")
	putFile(t, path, "authorized write")
	f.messages["a"] = []llm.Message{assistantText("[outside](<" + path + ">)")}
	if files, err := s.List("a"); err != nil || len(files) != 0 {
		t.Fatalf("outside link = %+v %v", files, err)
	}
	f.messages["a"] = append(f.messages["a"], toolMessages("write", "Write", map[string]any{"path": path}, false)...)
	files, err := s.List("a")
	if err != nil || len(files) != 1 {
		t.Fatalf("outside recorded write = %+v %v", files, err)
	}
	if preview, err := s.Read("a", files[0].ID); err != nil || preview.Content != "authorized write" {
		t.Fatalf("outside read = %+v %v", preview, err)
	}
}

func TestReadMissingBinaryLargeAndRedactedText(t *testing.T) {
	s, f, root := fileFixture(t)
	for _, tt := range []struct {
		name, content     string
		binary, truncated bool
	}{
		{"empty.md", "", false, false},
		{"binary.md", "hello\x00world", true, false},
		{"invalid.md", "\xff\xfe", true, false},
		{"large.md", strings.Repeat("中", MaxPreviewBytes), false, true},
		{"secret.md", "# Report\napi_key=sk-not-a-real-test-secret\npassword:\n  multiline-test-secret\n\x1b]52;c;dGVzdA==\x07visible\x1b[31m red\x1b[0m\r\x08\u202e", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			putFile(t, filepath.Join(root, tt.name), tt.content)
			f.messages["a"] = []llm.Message{assistantText("[file](" + tt.name + ")")}
			files, err := s.List("a")
			if err != nil || len(files) != 1 {
				t.Fatalf("list = %+v %v", files, err)
			}
			preview, err := s.Read("a", files[0].ID)
			if tt.binary {
				if !errors.Is(err, ErrNotText) {
					t.Fatalf("binary = %+v %v", preview, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if preview.Truncated != tt.truncated || len(preview.Content) > MaxPreviewBytes || !utf8.ValidString(preview.Content) {
				t.Fatalf("bad preview bound: %d %v", len(preview.Content), preview.Truncated)
			}
			for _, forbidden := range []string{"sk-not-a-real-test-secret", "multiline-test-secret", "\x1b", "dGVzdA==", "\x08", "\r", "\u202e"} {
				if strings.Contains(preview.Content, forbidden) {
					t.Fatalf("preview leaked %q: %q", forbidden, preview.Content)
				}
			}
			if err := os.Remove(filepath.Join(root, tt.name)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Read("a", files[0].ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing read = %v", err)
			}
		})
	}
}

func TestRepeatedToolIDsDoNotAuthorizeFailedOrOrphanCalls(t *testing.T) {
	s, f, root := fileFixture(t)
	putFile(t, filepath.Join(root, "good.md"), "good")
	putFile(t, filepath.Join(root, "bad.md"), "bad")
	f.messages["a"] = toolMessages("reused", "Write", map[string]any{"path": "good.md"}, false)
	f.messages["a"] = append(f.messages["a"], toolMessages("reused", "Write", map[string]any{"path": "bad.md"}, true)...)
	f.messages["a"] = append(f.messages["a"], toolMessages("reused", "Write", nil, false)[1])
	files, err := s.List("a")
	if err != nil || len(files) != 1 || files[0].Name != "good.md" {
		t.Fatalf("reused result = %+v %v", files, err)
	}
}
