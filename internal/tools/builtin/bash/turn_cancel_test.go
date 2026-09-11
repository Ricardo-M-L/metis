package bash

import (
	"context"
	"errors"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestBashCancelledContextDoesNotStartBackgroundJobs(t *testing.T) {
	for _, await := range []bool{false, true} {
		name := "persistent"
		if await {
			name = "finite"
		}
		t.Run(name, func(t *testing.T) {
			pool := jobs.NewRegistry(t.TempDir())
			t.Cleanup(func() { pool.ResetAndWait(0) })
			b := Bash{gate: permission.New(permission.ModeBypass), Jobs: pool}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := b.Execute(ctx, map[string]any{
				"command": "printf must-not-start", "description": "cancelled background dispatch",
				"run_in_background": true, "await_completion": await,
			})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Execute error = %v, want context.Canceled", err)
			}
			if got := len(pool.List()); got != 0 {
				t.Fatalf("cancelled turn started %d background jobs", got)
			}
		})
	}
}
