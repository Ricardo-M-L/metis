package memdir

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseBulletBody_LinkForm(t *testing.T) {
	got := parseBulletBody("[User Role](user_role.md) — Backend lead")
	if got.Title != "User Role" || got.File != "user_role.md" || got.Hook != "Backend lead" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseBulletBody_LinkFormHyphen(t *testing.T) {
	got := parseBulletBody("[X](x.md) - hyphen sep")
	if got.Title != "X" || got.File != "x.md" || got.Hook != "hyphen sep" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseBulletBody_BareForm(t *testing.T) {
	got := parseBulletBody("plain.md — hook text")
	if got.File != "plain.md" || got.Hook != "hook text" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseBulletBody_BareFormNoHook(t *testing.T) {
	got := parseBulletBody("just_a_file.md")
	if got.File != "just_a_file.md" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseBulletBody_Garbage(t *testing.T) {
	got := parseBulletBody("not a memory line at all")
	if got.File != "" {
		t.Fatalf("garbage should not parse a File, got %+v", got)
	}
}

func TestReadIndex_NonExistent(t *testing.T) {
	root := t.TempDir()
	got, oversize, err := ReadIndex(root)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if oversize {
		t.Fatalf("oversize should be false for missing index")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d", len(got))
	}
}

func TestReadIndex_Mixed(t *testing.T) {
	root := t.TempDir()
	body := "# heading\n\n- [user role](user.md) — be lead\n- bare.md\n- gibberish line\n"
	if err := os.WriteFile(IndexPath(root), []byte(body), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	got, oversize, err := ReadIndex(root)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if oversize {
		t.Fatalf("oversize false expected")
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(got), got)
	}
}

func TestReadIndex_OversizeFlag(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 250; i++ {
		sb.WriteString("- entry.md\n")
	}
	if err := os.WriteFile(IndexPath(root), []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, oversize, err := ReadIndex(root)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if !oversize {
		t.Fatalf("expected oversize=true on 250-line index")
	}
}

func TestWriteIndex_GroupsByType(t *testing.T) {
	root := t.TempDir()
	files := []MemoryFile{
		{Path: root + "/user_role.md", Name: "user_role", ModTime: time.Now(),
			Frontmatter: Frontmatter{Name: "User Role", Description: "Be lead", Type: TypeUser}},
		{Path: root + "/feedback_chinese.md", Name: "feedback_chinese", ModTime: time.Now(),
			Frontmatter: Frontmatter{Name: "Chinese Replies", Description: "Reply in Chinese", Type: TypeFeedback}},
		{Path: root + "/project_x.md", Name: "project_x", ModTime: time.Now(),
			Frontmatter: Frontmatter{Name: "Proj X", Description: "Some proj", Type: TypeProject}},
		{Path: root + "/garbage.md", Name: "garbage", ParseError: errFakeParse},
	}
	if err := WriteIndex(root, files); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}
	got, err := os.ReadFile(IndexPath(root))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	gotS := string(got)
	for _, want := range []string{"user_role.md", "feedback_chinese.md", "project_x.md"} {
		if !strings.Contains(gotS, want) {
			t.Errorf("index missing %q: %q", want, gotS)
		}
	}
	if strings.Contains(gotS, "garbage.md") {
		t.Errorf("errored file should not appear in index: %q", gotS)
	}
	// Ordering: User group must appear before Feedback group, etc.
	uIdx := strings.Index(gotS, "user_role")
	fIdx := strings.Index(gotS, "feedback_chinese")
	pIdx := strings.Index(gotS, "project_x")
	if !(uIdx < fIdx && fIdx < pIdx) {
		t.Errorf("expected user < feedback < project, got %d, %d, %d", uIdx, fIdx, pIdx)
	}
}

type fakeErr struct{}

func (fakeErr) Error() string { return "parse" }

var errFakeParse = fakeErr{}
