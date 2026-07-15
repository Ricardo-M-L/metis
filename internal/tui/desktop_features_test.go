package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func TestParseGitDiffTracksLineNumbersAndStatuses(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\nindex 111..222 100644\n--- a/a.go\n+++ b/a.go\n@@ -2,2 +2,3 @@ func x() {\n old\n-gone\n+new\n+extra\n" +
		"diff --git a/new.txt b/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+hello\n"
	files := parseGitDiff(diff)
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
	if files[0].Status != "M" || files[1].Status != "A" {
		t.Fatalf("statuses = %q, %q", files[0].Status, files[1].Status)
	}
	lines := files[0].Hunks[0].Lines
	if lines[0].OldNum != 2 || lines[0].NewNum != 2 {
		t.Fatalf("context nums = %d/%d", lines[0].OldNum, lines[0].NewNum)
	}
	if lines[1].OldNum != 3 || lines[1].NewNum != 0 {
		t.Fatalf("delete nums = %d/%d", lines[1].OldNum, lines[1].NewNum)
	}
	if lines[2].OldNum != 0 || lines[2].NewNum != 3 {
		t.Fatalf("add nums = %d/%d", lines[2].OldNum, lines[2].NewNum)
	}
	if lines[3].NewNum != 4 {
		t.Fatalf("second add num = %d, want 4", lines[3].NewNum)
	}
}

func TestParseGitDiffHunkDefaultsCountToOne(t *testing.T) {
	h, ok := parseDiffHunkHeader("@@ -7 +9 @@ replacement")
	if !ok {
		t.Fatal("header did not parse")
	}
	if h.OldStart != 7 || h.OldCount != 1 || h.NewStart != 9 || h.NewCount != 1 {
		t.Fatalf("unexpected hunk: %+v", h)
	}
}

func TestBuildDiffFilesIncludesTrackedAndUntrackedButNotIgnored(t *testing.T) {
	repo := t.TempDir()
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWD) })

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q")
	write("staged.txt", "old staged\n")
	write("unstaged.txt", "old unstaged\n")
	write(".gitignore", "ignored.txt\n")
	git("add", ".")
	git("-c", "user.name=Metis Test", "-c", "user.email=metis@example.invalid", "commit", "-qm", "base")

	write("staged.txt", "new staged\n")
	git("add", "staged.txt")
	write("unstaged.txt", "new unstaged\n")
	write("folder/new file.txt", "first\nsecond\n")
	write("empty.txt", "")
	write("ignored.txt", "must stay hidden\n")

	files := (&Model{}).buildDiffFiles()
	byPath := make(map[string]screen.DiffFile, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	for _, path := range []string{"staged.txt", "unstaged.txt"} {
		if file, ok := byPath[path]; !ok {
			t.Errorf("tracked change %q missing: %#v", path, files)
		} else if file.Status != "M" {
			t.Errorf("tracked change %q status = %q, want M", path, file.Status)
		}
	}
	for _, path := range []string{"folder/new file.txt", "empty.txt"} {
		if file, ok := byPath[path]; !ok {
			t.Errorf("untracked file %q missing: %#v", path, files)
		} else if file.Status != "A" {
			t.Errorf("untracked file %q status = %q, want A", path, file.Status)
		}
	}
	if _, ok := byPath["ignored.txt"]; ok {
		t.Error("ignored untracked file must not appear in Changes")
	}
	if file := byPath["folder/new file.txt"]; len(file.Hunks) == 0 || len(file.Hunks[0].Lines) != 2 {
		t.Fatalf("untracked text file should carry a readable addition hunk: %+v", file)
	}
}

func TestAgentsViewOpensLocallyDuringActiveTurnAndRefreshes(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.subAgents = []SubAgentInfo{{
		ID: "agent-running", Name: "inspect tests", Status: "running", StartedAt: time.Now(),
	}}
	m.input.SetValue("/agents-view")

	pressEnter(t, m)
	av, ok := m.activeScreen.(*screen.MultiAgentScreen)
	if !ok {
		t.Fatalf("/agents-view mid-turn should open local modal, got %T", m.activeScreen)
	}
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("local command must not be queued/steered: %+v", m.queuedPrompts)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("local command should clear input, got %q", got)
	}
	if view := av.View(); !strings.Contains(view, "inspect tests") || !strings.Contains(view, "1 running") {
		t.Fatalf("active agent missing from modal:\n%s", view)
	}

	// The live source is re-evaluated while the modal remains open.
	m.subAgents = append(m.subAgents, SubAgentInfo{
		ID: "agent-second", Name: "review changes", Status: "running", StartedAt: time.Now(),
	})
	if view := av.View(); !strings.Contains(view, "review changes") || !strings.Contains(view, "2 running") {
		t.Fatalf("open modal did not refresh from current model state:\n%s", view)
	}
}

func TestBuildAgentTasksRetainsCompletedAgentsFromToolTimeline(t *testing.T) {
	started := time.Now().Add(-3 * time.Second)
	m := &Model{toolEvents: []ToolEvent{
		{
			ID: "agent-finished", Kind: "result", ToolName: "Agent",
			Input:     map[string]any{"prompt": "audit persistence behavior"},
			StartTime: started, Duration: 2 * time.Second,
		},
		{ID: "child-read", Kind: "result", ToolName: "sub: Read", SubAgentParentID: "agent-finished"},
		{ID: "child-test", Kind: "result", ToolName: "sub: Bash", SubAgentParentID: "agent-finished"},
	}}

	// subAgents is empty exactly as it is after the 2-second status pill prune.
	tasks := m.buildAgentTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want retained completed task: %+v", len(tasks), tasks)
	}
	task := tasks[0]
	if task.Status != "completed" || task.ToolsCount != 2 || task.LastTool != "Bash" {
		t.Fatalf("retained task metadata = %+v", task)
	}
	if task.FinishedAt.IsZero() || task.FinishedAt.Sub(task.StartedAt) != 2*time.Second {
		t.Fatalf("retained task duration = %v..%v", task.StartedAt, task.FinishedAt)
	}
}

func TestBuildAgentTasksDoesNotInventDurationForResumedAgent(t *testing.T) {
	m := &Model{toolEvents: []ToolEvent{{
		ID: "agent-resumed", Kind: "end", ToolName: "Agent",
		Input: map[string]any{"prompt": "restored task"}, StartTime: time.Now().Add(-time.Hour),
	}}}

	tasks := m.buildAgentTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if !tasks[0].StartedAt.IsZero() || !tasks[0].FinishedAt.IsZero() {
		t.Fatalf("unknown resumed duration should stay hidden, got %+v", tasks[0])
	}
}
