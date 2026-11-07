package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tasks"
)

func TestTaskPlanHumanBoundary(t *testing.T) {
	for _, status := range []string{"completed", "pending"} {
		t.Run(status, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			const sid = "human-plan-boundary"
			old, err := tasks.ReplaceAll(sid, []tasks.Item{{Content: "earlier work", Status: status}})
			if err != nil {
				t.Fatal(err)
			}
			l := &Loop{}
			l.AppendUser("next user request")
			ctx, err := l.prepareTaskPlan(tasks.WithSessionID(context.Background(), sid))
			if err != nil {
				t.Fatal(err)
			}
			items, err := tasks.PlanningItemsForContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if status == "completed" {
				if tasks.PlanIDFromContext(ctx) == old.PlanID || len(items) != 0 {
					t.Fatalf("completed plan leaked into new work: %+v", items)
				}
				history, err := tasks.PlanHistory(sid)
				if err != nil || len(history) != 1 || len(history[0].Items) != 1 {
					t.Fatalf("prior work not preserved: %+v %v", history, err)
				}
			} else if tasks.PlanIDFromContext(ctx) != old.PlanID || len(items) != 1 {
				t.Fatalf("unfinished follow-up lost its plan: %+v", items)
			}
		})
	}
}

func TestTaskPlanSyntheticResumeKeepsArchivedOwner(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "scheduled-plan-owner"
	base := tasks.WithSessionID(context.Background(), sid)
	old, err := tasks.ReplaceAll(sid, []tasks.Item{{Content: "waiting work", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := tasks.ReplaceAll(sid, []tasks.Item{{Content: "unrelated new work", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{}
	l.AppendContextForTaskPlan("old work is ready", old.PlanID)
	ctx, err := l.prepareTaskPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	if tasks.PlanIDFromContext(ctx) != old.PlanID {
		t.Fatal("scheduled context rebound to a different plan")
	}
	if _, err := tasks.UpsertForContext(ctx, tasks.Item{Content: "waiting work", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	items, err := tasks.PlanningItems(sid)
	if err != nil || len(items) != 1 || items[0].Content != "unrelated new work" || items[0].Status != "pending" {
		t.Fatalf("scheduled result polluted current work: %+v %v", items, err)
	}
	id, err := tasks.CurrentPlanID(sid)
	if err != nil || id != current.PlanID {
		t.Fatal("scheduled completion activated an archived plan")
	}
}

func TestTaskPlanFrozenChildDoesNotBeginHumanWork(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "child-plan-owner"
	old, err := tasks.ReplaceAll(sid, []tasks.Item{{Content: "shared work", Status: "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tasks.FreezePlanContext(tasks.WithPlanID(tasks.WithSessionID(context.Background(), sid), old.PlanID))
	l := &Loop{}
	l.AppendUser("internally assigned child prompt")
	bound, err := l.prepareTaskPlan(ctx)
	if err != nil || tasks.PlanIDFromContext(bound) != old.PlanID {
		t.Fatalf("child changed task scope: %v", err)
	}
	id, _ := tasks.CurrentPlanID(sid)
	if id != old.PlanID {
		t.Fatal("child retired its parent's completed plan")
	}
}

func TestTaskPlanNoticeSupersedesLegacyReminderOnce(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	ctx := tasks.WithPlanID(context.Background(), "new-plan")
	l := &Loop{}
	l.AppendContext("<system-reminder>\nCurrent unified task list:\n1. [pending] archived task\n</system-reminder>")
	before := len(l.History())
	l.syncTaskPlanNotice(ctx)
	l.syncTaskPlanNotice(ctx)
	history := l.History()
	if len(history) != before+1 {
		t.Fatalf("scope transition injected %d notices", len(history)-before)
	}
	b := history[len(history)-1].Content[0]
	if !b.Synthetic || b.ProviderHint[TaskPlanHint] != "new-plan" || !strings.Contains(b.Text, "historical") {
		t.Fatal("missing host-owned task scope correction")
	}
}
