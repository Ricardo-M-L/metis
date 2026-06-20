package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tasks"
)

func setupTodoReminderEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "todo-rem-" + t.Name()
	tasks.SetCurrentSessionID(sid)
	t.Cleanup(func() { tasks.SetCurrentSessionID("") })
	return sid
}

func TestInjectTodoReminder_FiresAfterLullWithIncomplete(t *testing.T) {
	sid := setupTodoReminderEnv(t)
	tasks.ReplaceAll(sid, []tasks.Item{
		{Content: "task A", Status: "completed"},
		{Content: "task B", Status: "in_progress"},
		{Content: "task C", Status: "pending"},
	})

	l := &Loop{}
	l.iterIdx = todoReminderTurns + 1 // enough turns since write(0) + reminder(0)
	out := make(chan Event, 8)
	l.injectTodoReminder(context.Background(), out)

	hist := l.History()
	if len(hist) != 1 {
		t.Fatalf("expected 1 injected reminder message, got %d", len(hist))
	}
	body := hist[0].Content[0].Text
	for _, want := range []string{"system-reminder", "task B", "task C", "[in_progress]", "[pending]"} {
		if !strings.Contains(body, want) {
			t.Errorf("reminder missing %q:\n%s", want, body)
		}
	}
	// Second call within the gap must NOT double-remind.
	l.injectTodoReminder(context.Background(), out)
	if n := len(l.History()); n != 1 {
		t.Errorf("reminder double-fired within the gap: %d messages", n)
	}
}

func TestInjectTodoReminder_QuietWhenAllDone(t *testing.T) {
	sid := setupTodoReminderEnv(t)
	tasks.ReplaceAll(sid, []tasks.Item{
		{Content: "task A", Status: "completed"},
		{Content: "task B", Status: "completed"},
	})
	l := &Loop{}
	l.iterIdx = todoReminderTurns + 5
	out := make(chan Event, 8)
	l.injectTodoReminder(context.Background(), out)
	if n := len(l.History()); n != 0 {
		t.Errorf("no reminder expected when all tasks done; got %d", n)
	}
}

func TestInjectTodoReminder_QuietWithinLull(t *testing.T) {
	sid := setupTodoReminderEnv(t)
	tasks.ReplaceAll(sid, []tasks.Item{{Content: "task A", Status: "pending"}})
	l := &Loop{}
	l.iterIdx = todoReminderTurns - 1 // not enough turns yet
	out := make(chan Event, 8)
	l.injectTodoReminder(context.Background(), out)
	if n := len(l.History()); n != 0 {
		t.Errorf("reminder fired too early (within the lull window); got %d", n)
	}
}

func TestInjectTodoReminder_QuietWhenEmpty(t *testing.T) {
	setupTodoReminderEnv(t)
	l := &Loop{}
	l.iterIdx = todoReminderTurns + 5
	out := make(chan Event, 8)
	l.injectTodoReminder(context.Background(), out)
	if n := len(l.History()); n != 0 {
		t.Errorf("no reminder expected for empty list; got %d", n)
	}
}

// A TodoWrite resets the countdown — after noteTodoWriteActivity the
// reminder must stay quiet until another full lull passes.
func TestNoteTodoWriteActivity_ResetsCountdown(t *testing.T) {
	sid := setupTodoReminderEnv(t)
	tasks.ReplaceAll(sid, []tasks.Item{{Content: "task A", Status: "pending"}})
	l := &Loop{}
	l.iterIdx = todoReminderTurns + 1
	l.noteTodoWriteActivity(l.iterIdx) // model just touched the tracker
	out := make(chan Event, 8)
	l.injectTodoReminder(context.Background(), out)
	if n := len(l.History()); n != 0 {
		t.Errorf("reminder should be quiet right after a TodoWrite; got %d", n)
	}
}
