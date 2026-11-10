package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/tasks"
)

func TestTaskPresentationMixedPlanDoesNotFlattenExecution(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "tui-grouped-execution"
	plan, err := tasks.BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.Save(&tasks.List{SessionID: sid, PlanID: plan, Items: []tasks.Item{
		{ID: "main-one", Content: "Inspect primary schema", Status: "completed"},
		{ID: "main-two", Content: "Compare primary design", Status: "in_progress"},
		{ID: "main-three", Content: "Review primary proposal", Status: "pending"},
	}}); err != nil {
		t.Fatal(err)
	}
	store := tasks.TaskStoreForSession(sid)
	child, err := store.CreateOwnedInPlan(plan, "Protocol inspection", "", "", "verifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	status := tasks.TaskCompleted
	if _, err := store.Update(child.ID, tasks.TaskPatch{Status: &status}); err != nil {
		t.Fatal(err)
	}
	m := &Model{sessionID: sid, stripSelStart: -1, stripSelEnd: -1}
	strip := stripAnsiForTest(renderStickyTaskStrip(m))
	if strings.Contains(strip, child.Subject) || !strings.Contains(strip, "执行任务 1/1 完成") {
		t.Fatalf("sticky view flattened execution as another main step: %q", strip)
	}
	for _, content := range []string{"Inspect primary schema", "Compare primary design", "Review primary proposal"} {
		if !strings.Contains(strip, content) {
			t.Fatalf("primary step disappeared: %q", strip)
		}
	}
	panel := stripAnsiForTest(renderTaskPanel(m))
	group := strings.Index(panel, "执行任务")
	childAt := strings.Index(panel, child.Subject)
	if group < 0 || childAt < group || !strings.Contains(panel, "@verifier") {
		t.Fatalf("full panel lost grouped execution status/owner: %q", panel)
	}
	if items, err := tasks.PlanningItems(sid); err != nil || len(items) != 4 {
		t.Fatalf("presentation changed runtime completeness: %+v %v", items, err)
	}
}

func TestTaskPresentationGroupedChineseTitlesRemainUTF8(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "tui-grouped-chinese"
	plan, err := tasks.BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	title := "A" + strings.Repeat("中文任务", 12)
	if err := tasks.Save(&tasks.List{SessionID: sid, PlanID: plan, Items: []tasks.Item{{ID: "main", Content: title, Status: "in_progress"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.TaskStoreForSession(sid).CreateOwnedInPlan(plan, "B"+strings.Repeat("中文执行", 12), "", "", "复核员", nil); err != nil {
		t.Fatal(err)
	}
	panel := renderTaskPanel(&Model{sessionID: sid})
	if !utf8.ValidString(panel) {
		t.Fatal("grouped task titles were truncated inside UTF-8 characters")
	}
}
