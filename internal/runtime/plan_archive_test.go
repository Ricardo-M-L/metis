package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestArchivePlan_WritesFileWithSession(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	plan := ArchivedPlan{
		SessionID: "session-x",
		ToolCalls: []agent.ToolCall{
			{ID: "c1", Name: "Read", Input: map[string]any{"path": "/tmp/x"}},
		},
	}
	if err := ArchivePlan(plan); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(PlansDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 plan file, got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "session-x_") {
		t.Errorf("filename should start with session id; got %q", entries[0].Name())
	}
	if !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Errorf("expected .json extension; got %q", entries[0].Name())
	}
}

func TestArchivePlan_AutoTimestamp(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	before := time.Now()
	_ = ArchivePlan(ArchivedPlan{
		SessionID: "s",
		ToolCalls: []agent.ToolCall{{Name: "Read"}},
	})
	plans, err := ListPlans(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("got %d plans", len(plans))
	}
	if plans[0].Timestamp.Before(before) {
		t.Errorf("timestamp should be auto-set after `before`")
	}
}

func TestArchivePlan_EmptyToolCallsSkipped(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := ArchivePlan(ArchivedPlan{SessionID: "s"}); err != nil {
		t.Errorf("empty plan should be no-op, not error; got %v", err)
	}
	if _, err := os.Stat(PlansDir()); err == nil {
		t.Error("plans/ shouldn't be created for empty plan")
	}
}

func TestListPlans_NewestFirst(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	now := time.Now()
	for i := 0; i < 3; i++ {
		_ = ArchivePlan(ArchivedPlan{
			SessionID: "s", Timestamp: now.Add(time.Duration(i) * time.Second),
			ToolCalls: []agent.ToolCall{{Name: "Read"}},
		})
	}
	plans, err := ListPlans(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 {
		t.Fatalf("got %d plans", len(plans))
	}
	if !plans[0].Timestamp.After(plans[1].Timestamp) ||
		!plans[1].Timestamp.After(plans[2].Timestamp) {
		t.Errorf("plans should be newest-first; got timestamps %v %v %v",
			plans[0].Timestamp, plans[1].Timestamp, plans[2].Timestamp)
	}
}

func TestListPlans_LimitRespected(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	for i := 0; i < 5; i++ {
		_ = ArchivePlan(ArchivedPlan{
			SessionID: "s", Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			ToolCalls: []agent.ToolCall{{Name: "Read"}},
		})
	}
	plans, _ := ListPlans(2)
	if len(plans) != 2 {
		t.Errorf("limit=2 ignored; got %d plans", len(plans))
	}
}

func TestListPlans_MissingDirReturnsNil(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	plans, err := ListPlans(0)
	if err != nil {
		t.Errorf("missing plans dir should not error; got %v", err)
	}
	if plans != nil {
		t.Errorf("got %v plans for missing dir; want nil", plans)
	}
}

func TestPlansDir_Path(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	want := filepath.Join(dir, "plans")
	if got := PlansDir(); got != want {
		t.Errorf("PlansDir = %q, want %q", got, want)
	}
}
