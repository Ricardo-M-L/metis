package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestApplyDesktopWorkerMemoryPolicy(t *testing.T) {
	newLoop := func() *agent.Loop {
		return agent.NewLoop(nil, nil, nil, nil, "", 1)
	}
	worker := newLoop()
	applyDesktopWorkerMemoryPolicy(worker, func(string) string { return "1" })
	if worker.DistillEvery != 0 {
		t.Fatalf("worker distillation cadence = %d, want disabled", worker.DistillEvery)
	}
	normal := newLoop()
	want := normal.DistillEvery
	applyDesktopWorkerMemoryPolicy(normal, func(string) string { return "" })
	if normal.DistillEvery != want {
		t.Fatalf("normal cadence = %d, want %d", normal.DistillEvery, want)
	}
}
