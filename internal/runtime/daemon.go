// Package runtime — KAIROS-style long-running daemon mode.
//
// claude-code's KAIROS feature flag (per the leaked sourcemap) gates a
// 24/7 mode: agent stays alive, processes queued tasks, and uses idle
// time to organize memory ("AutoDream" auto-distillation). metis
// daemon is the minimum-viable version of that:
//
//   - Watches ~/.metis/inbox/*.txt for new task files (one prompt per
//     file). Drops processed files into ~/.metis/outbox/.
//   - On idle (no inbox files for IdleTimeout), runs a one-shot
//     distillation prompt that condenses recent memory into longer-
//     lived summary blocks.
//   - Heartbeats to ~/.metis/daemon.pid + ~/.metis/daemon.status.json
//     so other processes can tell if a daemon is up and what it's
//     doing.
//
// This is not the full CC pattern (which includes scheduling,
// notifications, multi-process coordination). It IS a working
// always-on agent loop with a real task queue + auto-distillation,
// which covers the user-facing 80% of KAIROS behavior.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DaemonConfig controls the daemon loop. Defaults are sensible for
// "background helper that processes a few tasks per hour".
type DaemonConfig struct {
	InboxDir     string        // default ~/.metis/inbox
	OutboxDir    string        // default ~/.metis/outbox
	PidFile      string        // default ~/.metis/daemon.pid
	StatusFile   string        // default ~/.metis/daemon.status.json
	PollInterval time.Duration // default 5s — how often to scan inbox
	IdleTimeout  time.Duration // default 30m — distillation cadence
}

// DefaultDaemonConfig fills in the standard paths under ~/.metis.
func DefaultDaemonConfig() DaemonConfig {
	home := Home()
	return DaemonConfig{
		InboxDir:     filepath.Join(home, "inbox"),
		OutboxDir:    filepath.Join(home, "outbox"),
		PidFile:      filepath.Join(home, "daemon.pid"),
		StatusFile:   filepath.Join(home, "daemon.status.json"),
		PollInterval: 5 * time.Second,
		IdleTimeout:  30 * time.Minute,
	}
}

// Home returns the metis home directory, honoring METIS_HOME override.
// Mirrors internal/config.Home() but copied here to avoid a runtime →
// config import cycle.
func Home() string {
	if h := os.Getenv("METIS_HOME"); h != "" {
		return h
	}
	hd, _ := os.UserHomeDir()
	return filepath.Join(hd, ".metis")
}

// DaemonStatus is the JSON written to StatusFile each tick.
type DaemonStatus struct {
	StartedAt        time.Time `json:"started_at"`
	LastTickAt       time.Time `json:"last_tick_at"`
	TasksProcessed   int       `json:"tasks_processed"`
	LastTask         string    `json:"last_task"`
	LastError        string    `json:"last_error,omitempty"`
	IdleSince        time.Time `json:"idle_since,omitempty"`
	DistillationRuns int       `json:"distillation_runs"`
}

// TaskHandler is the per-task callback. The daemon takes care of file
// I/O + bookkeeping; the handler runs the actual agent loop.
//
// Returns the (possibly multi-line) result string the daemon will
// write to <OutboxDir>/<taskID>.txt. An error is logged + recorded in
// status but does NOT stop the daemon — one bad task file shouldn't
// kill the long-running process.
type TaskHandler func(ctx context.Context, prompt string) (string, error)

// DistillHandler runs the idle-time auto-distillation pass. The daemon
// only invokes this once per IdleTimeout window — back-to-back tasks
// reset the idle timer. Pass nil to disable.
type DistillHandler func(ctx context.Context) error

// RunDaemon is the daemon loop. Blocks until ctx is canceled. Safe to
// run from main()'s `metis daemon` subcommand.
func RunDaemon(ctx context.Context, cfg DaemonConfig, run TaskHandler, distill DistillHandler) error {
	for _, dir := range []string{cfg.InboxDir, cfg.OutboxDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("daemon: create %s: %w", dir, err)
		}
	}
	if err := writePidFile(cfg.PidFile); err != nil {
		return fmt.Errorf("daemon: pid file: %w", err)
	}
	defer os.Remove(cfg.PidFile)

	status := DaemonStatus{StartedAt: time.Now()}
	flushStatus := func() { _ = writeJSON(cfg.StatusFile, status) }
	flushStatus()
	defer os.Remove(cfg.StatusFile)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	idleSince := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-ticker.C:
			status.LastTickAt = t
			tasks, err := scanInbox(cfg.InboxDir)
			if err != nil {
				status.LastError = err.Error()
				flushStatus()
				continue
			}
			if len(tasks) == 0 {
				// Idle path: run distillation once per IdleTimeout window.
				if distill != nil && time.Since(idleSince) > cfg.IdleTimeout {
					if err := distill(ctx); err != nil {
						status.LastError = "distill: " + err.Error()
					} else {
						status.DistillationRuns++
						status.LastError = ""
					}
					idleSince = time.Now() // reset so we don't re-fire next tick
					flushStatus()
				} else {
					status.IdleSince = idleSince
					flushStatus()
				}
				continue
			}
			// Reset idle timer — there's work to do.
			idleSince = time.Now()
			for _, taskPath := range tasks {
				if err := processOne(ctx, taskPath, cfg.OutboxDir, run); err != nil {
					status.LastError = err.Error()
				} else {
					status.TasksProcessed++
					status.LastTask = filepath.Base(taskPath)
					status.LastError = ""
				}
				flushStatus()
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
		}
	}
}

// scanInbox returns paths of *.txt files under dir, sorted by mtime
// (oldest first) so FIFO ordering holds across daemon restarts.
func scanInbox(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type ent struct {
		path string
		mod  time.Time
	}
	var ee []ent
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".txt") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		ee = append(ee, ent{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
	}
	sort.Slice(ee, func(i, j int) bool { return ee[i].mod.Before(ee[j].mod) })
	out := make([]string, len(ee))
	for i, e := range ee {
		out[i] = e.path
	}
	return out, nil
}

// processOne reads the task file, runs the handler, writes the result
// to outbox, then DELETES the inbox file. A crash mid-handler leaves
// the inbox file in place so the task gets retried on next start —
// at-least-once semantics.
func processOne(ctx context.Context, taskPath, outboxDir string, run TaskHandler) error {
	body, err := os.ReadFile(taskPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", taskPath, err)
	}
	prompt := strings.TrimSpace(string(body))
	if prompt == "" {
		_ = os.Remove(taskPath) // empty task = drop silently
		return nil
	}
	result, runErr := run(ctx, prompt)
	id := strings.TrimSuffix(filepath.Base(taskPath), filepath.Ext(taskPath))
	outPath := filepath.Join(outboxDir, id+".txt")
	out := result
	if runErr != nil {
		out = "ERROR: " + runErr.Error() + "\n\n" + result
	}
	if err := os.WriteFile(outPath, []byte(out), 0o644); err != nil {
		return fmt.Errorf("write outbox %s: %w", outPath, err)
	}
	if err := os.Remove(taskPath); err != nil {
		return fmt.Errorf("remove inbox %s: %w", taskPath, err)
	}
	return runErr
}

func writePidFile(path string) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
