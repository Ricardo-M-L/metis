package main

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/exitcode"
)

func TestWorkerTerminalOutcomeDoesNotHideLateFailure(t *testing.T) {
	for _, reason := range []string{"max_iterations", "content_filter"} {
		t.Run(reason, func(t *testing.T) {
			terminal := runTerminalError(reason, "last tool result mentioned 429 rate limit")
			for _, tc := range []struct {
				name string
				err  error
				want int
			}{
				{"checkpoint_storage", errors.New("save headless session checkpoint: backend unavailable"), exitcode.General},
				{"checkpoint_disk_full", fmt.Errorf("save headless session checkpoint: %w", syscall.ENOSPC), exitcode.IO},
				{"checkpoint_bridge", fmt.Errorf("report headless session checkpoint: %w", syscall.EPIPE), exitcode.IO},
			} {
				t.Run(tc.name, func(t *testing.T) {
					// Match cmdRun's deferred checkpoint/bridge error joins.
					joined := errors.Join(terminal, tc.err)
					if got := exitcode.Classify(joined); got != tc.want {
						t.Fatalf("worker exit code = %d, want %d for %v", got, tc.want, joined)
					}
					if !errors.Is(joined, tc.err) {
						t.Fatal("late failure is no longer available to errors.Is")
					}
				})
			}
		})
	}
}
