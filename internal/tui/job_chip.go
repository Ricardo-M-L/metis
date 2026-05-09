package tui

// job_chip.go — counts running background-bash jobs for the
// "⚙ N jobs" status-bar chip in render_chrome.go.
//
// Reaches into m.loop.Jobs (which the runtime layer wires from
// jobs.Registry on session bootstrap). Returns 0 when no pool is
// attached (sub-agent contexts, headless tests, builds before the
// 2026-05-09 wiring) so the chip simply doesn't render.

import (
	"github.com/Ricardo-M-L/metis/internal/jobs"
)

// bashJobsRunningCount returns how many bash jobs are currently in
// StatusRunning. Read-only; takes no locks of its own — the
// Registry's List does its own snapshot.
func bashJobsRunningCount(m *Model) int {
	if m == nil || m.loop == nil || m.loop.Jobs == nil {
		return 0
	}
	all := m.loop.Jobs.List()
	n := 0
	for _, j := range all {
		if j.Status == jobs.StatusRunning {
			n++
		}
	}
	return n
}
