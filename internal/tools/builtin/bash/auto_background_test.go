package bash

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestAutoBackgroundPromotionHasSingleWaitOwner(t *testing.T) {
	oldThreshold := AutoBackgroundThreshold
	AutoBackgroundThreshold = 25 * time.Millisecond
	t.Cleanup(func() { AutoBackgroundThreshold = oldThreshold })

	pool := jobs.NewRegistry(t.TempDir())
	b := Bash{gate: permission.New(permission.ModeBypass), Jobs: pool}
	res, err := b.Execute(context.Background(), map[string]any{
		"command":     `printf 'promotion-start\n'; sleep 0.20; printf 'promotion-done\n'`,
		"description": "exercise automatic background promotion",
		"timeout_ms":  float64(2_000),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || res.IsError || !strings.Contains(res.Output, "moved to background") {
		t.Fatalf("promotion result = %+v", res)
	}

	listed := pool.List()
	if len(listed) != 1 {
		t.Fatalf("jobs after promotion = %+v, want one", listed)
	}
	id := listed[0].ID

	deadline := time.Now().Add(3 * time.Second)
	var final jobs.Job
	for time.Now().Before(deadline) {
		var ok bool
		final, ok = pool.Get(id)
		if ok && final.Status != jobs.StatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final.Status != jobs.StatusCompleted || final.ExitCode != 0 {
		t.Fatalf("promoted job terminal state = %s exit=%d; double Wait usually reports failed/-1",
			final.Status, final.ExitCode)
	}
	body, err := jobs.ReadJobOutput(final.OutputPath, 0)
	if err != nil {
		t.Fatalf("ReadJobOutput: %v", err)
	}
	if !strings.Contains(body, "promotion-start") || !strings.Contains(body, "promotion-done") {
		t.Fatalf("promoted output incomplete: %q", body)
	}
}
