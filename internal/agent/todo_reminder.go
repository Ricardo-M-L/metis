package agent

// Periodic todo reminder — the mechanism that actually keeps tasks from
// being left stuck mid-status, mirroring Claude Code's
// getTodoReminderAttachments (restored-src/src/utils/attachments.ts).
//
// The full-list TodoWrite schema (internal/tasks.ReplaceAll) lets the
// model restate every task each call, but nothing makes it DO so — a
// model can finish work and simply forget the tracker. Claude Code's
// answer is to re-surface the list: when several turns pass with no
// TodoWrite and incomplete tasks remain, inject a <system-reminder>
// echoing the current list so the model SEES what's still open and
// closes it. Without this, the list silently rots (the 2026-06-14 bug:
// three verify tasks left "in progress" while the model moved on).

import (
	"context"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/tasks"
)

// todoReminderTurns is how many iterations without a TodoWrite must
// pass before the list is re-surfaced — and the minimum gap between
// reminders. Matches Claude Code's TURNS_SINCE_WRITE / TURNS_BETWEEN
// (both 10).
const todoReminderTurns = 10

// injectTodoReminder re-surfaces the current todo list as a
// <system-reminder> when (a) it's been >= todoReminderTurns iterations
// since the last TodoWrite, (b) >= todoReminderTurns since the last
// reminder, and (c) the list has incomplete items. Cheap no-op when the
// list is empty or fully done.
func (l *Loop) injectTodoReminder(ctx context.Context, out chan<- Event) {
	l.mu.RLock()
	cur := l.iterIdx
	lastWrite := l.todoWriteIter
	lastRemind := l.todoReminderIter
	l.mu.RUnlock()

	// Don't nag right after a TodoWrite, and don't double-remind.
	if cur-lastWrite < todoReminderTurns || cur-lastRemind < todoReminderTurns {
		return
	}

	items, err := tasks.PlanningItems(tasks.SessionIDFromContext(ctx))
	if err != nil || len(items) == 0 {
		return
	}
	if !hasIncompleteTodos(items) {
		return // all done → nothing to chase
	}

	l.appendInjectedMessage(formatTodoReminder(items))
	l.mu.Lock()
	l.todoReminderIter = cur
	l.mu.Unlock()
	emit(ctx, out, Event{
		Kind: EventInfo,
		Info: "[todo] re-surfaced incomplete task list (no TodoWrite recently)",
	})
}

// noteTodoWriteActivity records that a TodoWrite ran on iteration cur,
// resetting the reminder countdown. Called from the loop when the
// model's tool batch includes TodoWrite.
func (l *Loop) noteTodoWriteActivity(cur int) {
	l.mu.Lock()
	l.todoWriteIter = cur
	l.mu.Unlock()
}

// incompleteTodos returns the current session's task list if it has any
// non-completed item, else nil. Drives the end-of-turn reconciliation in
// Run (loop.go no_tool_calls branch).
func (l *Loop) incompleteTodos(ctx context.Context) []tasks.Item {
	items, err := tasks.PlanningItems(tasks.SessionIDFromContext(ctx))
	if err != nil || len(items) == 0 {
		return nil
	}
	if !hasIncompleteTodos(items) {
		return nil
	}
	return items
}

// endOfTurnTodoReminder is the nudge injected when the model tries to end a
// turn with open todos. Stronger than the periodic reminder: the model is
// about to STOP, so it must mark finished items completed now (or say what's
// blocked) — otherwise the bottom task strip shows a stale in_progress row
// after the work is already delivered.
func endOfTurnTodoReminder(items []tasks.Item) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	b.WriteString("You're about to end the turn, but the task list still has open items. ")
	b.WriteString("Call TodoWrite now to mark every task you've finished as completed (including any you just described as done). ")
	b.WriteString("If an item genuinely isn't finished or is blocked on the user, leave it and briefly say so — but don't leave a finished task showing in_progress. ")
	b.WriteString("Do NOT mention this reminder to the user.\n\n")
	b.WriteString("Current unified task list:\n")
	for i, it := range items {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, it.Status, it.Content)
	}
	b.WriteString("</system-reminder>")
	return b.String()
}

func hasIncompleteTodos(items []tasks.Item) bool {
	for _, it := range items {
		if it.Status != "completed" {
			return true
		}
	}
	return false
}

// formatTodoReminder builds the system-reminder body echoing the list.
// Wording follows Claude Code's todo_reminder attachment: a gentle
// nudge plus the verbatim current list, with an explicit "finish the
// open ones before claiming done" so the model acts on it.
func formatTodoReminder(items []tasks.Item) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	b.WriteString("The TodoWrite tool hasn't been used in a while and your task list still has open items. ")
	b.WriteString("Update it (mark finished tasks completed; keep one in_progress for serial work, or one per independent owner for parallel work) so it reflects reality — and finish the still-open items below before claiming the work is done. ")
	b.WriteString("Do NOT mention this reminder to the user.\n\n")
	b.WriteString("Current unified task list:\n")
	for i, it := range items {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, it.Status, it.Content)
	}
	b.WriteString("</system-reminder>")
	return b.String()
}
