package main

import (
	"context"
	"errors"
	"os/exec"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
)

func TestDesktopWorkerWaitsForBackgroundJobNotification(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	registry := jobs.NewRegistry(t.TempDir())
	defer registry.ResetAndWait(time.Second)
	cmd := exec.Command("sh", "-c", "sleep 0.08; printf done")
	if _, err := registry.Spawn(jobs.SpawnArgs{Command: "sleep 0.08; printf done", Cmd: cmd}); err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{Jobs: registry, JobNotify: registry.Notify()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	resume, err := awaitDesktopWorkerBackground(ctx, loop, nil)
	if err != nil || !resume {
		t.Fatalf("background completion was lost: resume=%v err=%v", resume, err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("worker resumed before background completion")
	}
	select {
	case notification := <-registry.Notify():
		if notification.Status != jobs.StatusCompleted {
			t.Fatalf("wrong notification: %+v", notification)
		}
	default:
		t.Fatal("worker consumed the notification intended for Loop")
	}
	resume, err = awaitDesktopWorkerBackground(ctx, loop, nil)
	if err != nil || resume {
		t.Fatalf("idle worker continued without work: resume=%v err=%v", resume, err)
	}
}

func TestDesktopWorkerBackgroundWaitCancelsWithoutDrainingJob(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	registry := jobs.NewRegistry(t.TempDir())
	defer registry.ResetAndWait(time.Second)
	cmd := exec.Command("sh", "-c", "sleep 1")
	if _, err := registry.Spawn(jobs.SpawnArgs{Command: "sleep 1", Cmd: cmd}); err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{Jobs: registry, JobNotify: registry.Notify()}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start := time.Now()
	resume, err := awaitDesktopWorkerBackground(ctx, loop, nil)
	if resume || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel failed: resume=%v err=%v", resume, err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("cancel waited for background process")
	}
}
