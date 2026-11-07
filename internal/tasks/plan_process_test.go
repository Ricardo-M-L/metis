package tasks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

func TestPlanStorageProcessHelper(t *testing.T) {
	if os.Getenv("METIS_PLAN_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	ctx := WithPlanID(WithSessionID(context.Background(), "shared-plan-process"), os.Getenv("METIS_PLAN_TEST_SCOPE"))
	ctx = FreezePlanContext(ctx)
	for i := 0; i < 40; i++ {
		if _, err := UpsertForContext(ctx, Item{Content: fmt.Sprintf("%s item %d", os.Getenv("METIS_PLAN_TEST_ROLE"), i), Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentProcessesPreserveCurrentAndArchivedPlans(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "shared-plan-process"
	old, err := ReplaceAll(sid, []Item{{Content: "old initial", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := ReplaceAll(sid, []Item{{Content: "current initial", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commands := []*exec.Cmd{}
	for _, child := range []struct{ id, role string }{{old.PlanID, "archived-child"}, {current.PlanID, "current-child"}} {
		cmd := exec.Command(exe, "-test.run=^TestPlanStorageProcessHelper$")
		cmd.Env = append(os.Environ(), "METIS_PLAN_TEST_CHILD=1", "METIS_PLAN_TEST_SCOPE="+child.id, "METIS_PLAN_TEST_ROLE="+child.role)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	items, err := PlanningItems(sid)
	if err != nil || len(items) != 41 {
		t.Fatalf("current plan lost concurrent writes: count=%d err=%v", len(items), err)
	}
	history, err := PlanHistory(sid)
	if err != nil || len(history) != 1 || len(history[0].Items) != 41 {
		t.Fatalf("archive lost concurrent writes: %+v %v", history, err)
	}
	if id, _ := CurrentPlanID(sid); id != current.PlanID {
		t.Fatal("archived child reset active plan")
	}
}
