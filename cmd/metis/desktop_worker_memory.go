package main

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

const desktopIsolatedWorkerEnv = "METIS_DESKTOP_ISOLATED_WORKER"

// applyDesktopWorkerMemoryPolicy keeps foreground worker latency independent
// from optional LLM fact-distillation. Normal Desktop and CLI processes retain
// their configured cadence; an isolated worker still persists the session and
// synchronous recall record, and execution failures retain their dedicated
// recovery memory.
func applyDesktopWorkerMemoryPolicy(loop *agent.Loop, getenv func(string) string) {
	if loop == nil || getenv == nil {
		return
	}
	if strings.TrimSpace(getenv(desktopIsolatedWorkerEnv)) == "1" {
		loop.DistillEvery = 0
	}
}
