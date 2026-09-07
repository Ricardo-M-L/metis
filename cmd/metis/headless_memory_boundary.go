package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/memory"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// distillationBoundary is the narrow lifecycle contract shared by every
// headless entry point. Keeping the boundary independent from agent.Loop makes
// the ordering, timeout and per-session isolation directly testable.
type distillationBoundary interface {
	FlushPendingDistillation(sessionID string) int
	WaitForDistillation(ctx context.Context, sessionID string) error
}

// Only a timeout of our own bounded join gets this marker. A provider timeout
// is marked by memory.DistillationProviderError; an archival/storage error
// must not become optional merely because it unwraps to DeadlineExceeded.
type headlessDistillationJoinTimeout struct{ error }

func (e *headlessDistillationJoinTimeout) Unwrap() error { return e.error }

// persistHeadlessMemoryBoundary turns residual successful exchanges into
// registered distillation jobs and joins only the owning session before the
// caller may tear down provider/repository dependencies. The wait deliberately
// receives a fresh bounded context: callers invoke this only after their work
// context completed successfully, and a late parent cancellation must not race
// a clean durability hand-off.
func persistHeadlessMemoryBoundary(
	loop distillationBoundary,
	sessionID, source string,
	grace time.Duration,
) error {
	if loop == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if grace <= 0 {
		grace = runtimeDistillationShutdownGrace
	}

	loop.FlushPendingDistillation(sessionID)
	waitCtx, cancel := context.WithTimeout(context.Background(), grace)
	err := loop.WaitForDistillation(waitCtx, sessionID)
	// Loop.WaitForDistillation returns the wait context error directly only
	// when the local join expires; completed job failures are errors.Join-ed.
	if err != nil && err == waitCtx.Err() {
		err = &headlessDistillationJoinTimeout{err}
	} else if waitErr, ok := err.(*agent.DistillationWaitError); ok && waitErr.WaitErr != nil && waitErr.WaitErr == waitCtx.Err() {
		// Preserve every completed job failure; only the explicitly separate
		// local wait cause is optional. A mixed archival failure stays fatal.
		err = errors.Join(&headlessDistillationJoinTimeout{waitErr.WaitErr}, waitErr.CompletedErrors)
	}
	cancel()
	if err == nil {
		return nil
	}
	if strings.TrimSpace(source) == "" {
		source = "headless session"
	}
	return fmt.Errorf("%s: join memory distillation for %s: %w", source, sessionID, err)
}

// persistHeadlessMemoryBoundary is intentionally success-only. Its callers
// invoke it only after the owned work returned nil; therefore it must not
// re-check a parent context that can be cancelled after the successful turn.
// Error and in-turn cancellation paths skip this method and flow directly into
// runtime.Cleanup, whose destructive barrier discards partial residual content.
func (r *runtime) persistHeadlessMemoryBoundary(source string, grace time.Duration) error {
	if r == nil || r.loop == nil || strings.TrimSpace(r.sessionID) == "" {
		return nil
	}
	return completeHeadlessMemoryBoundary(r.loop, r.sessionID, source, grace, os.Stderr)
}

// completeHeadlessMemoryBoundary preserves successful task status when only
// optional provider enrichment failed. The low-level lifecycle join remains
// strict, and every unknown, mixed or storage error remains fatal. This is not
// proof of session durability: caller-owned checkpoint defers must still run
// and retain their errors. Cleanup still cancels and joins remaining workers.
func completeHeadlessMemoryBoundary(loop distillationBoundary, sessionID, source string, grace time.Duration, warnings io.Writer) error {
	err := persistHeadlessMemoryBoundary(loop, sessionID, source, grace)
	if err == nil {
		return err
	}
	reason := "provider_error"
	var joinTimeout *headlessDistillationJoinTimeout
	if errors.As(err, &joinTimeout) {
		reason = "join_timeout"
		// Long-lived cron/daemon runtimes do not Cleanup after each task.
		// Stop this session's enrichment work now; do not retry or extend the
		// completed task's budget. Final Cleanup keeps its bounded join.
		if canceler, ok := loop.(interface{ CancelDistillation(string) }); ok {
			canceler.CancelDistillation(sessionID)
		}
	}
	if !optionalHeadlessMemoryError(err) {
		return err
	}
	// Do not serialize the provider error, task text, session ID, or arbitrary
	// caller labels: they may contain credentials or private conversation data.
	warning := struct {
		Kind         string `json:"kind"`
		Source       string `json:"source"`
		TaskStatus   string `json:"task_status"`
		MemoryStatus string `json:"memory_status"`
		Reason       string `json:"reason"`
		Message      string `json:"message"`
	}{
		Kind: "memory_enrichment_warning", Source: headlessMemorySource(source),
		TaskStatus: "completed", MemoryStatus: "incomplete", Reason: reason,
		Message: "Task completed; optional memory enrichment did not complete. Distilled memory is not confirmed saved.",
	}
	if warnings == nil {
		warnings = os.Stderr
	}
	if err := json.NewEncoder(warnings).Encode(warning); err != nil {
		return fmt.Errorf("write memory enrichment warning: %w", err)
	}
	return nil
}

func optionalHeadlessMemoryError(err error) bool {
	switch err := err.(type) {
	case *memory.DistillationProviderError, *headlessDistillationJoinTimeout:
		return true
	case interface{ Unwrap() []error }:
		children := err.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !optionalHeadlessMemoryError(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return optionalHeadlessMemoryError(err.Unwrap())
	default:
		return false
	}
}

func headlessMemorySource(source string) string {
	switch {
	case source == "metis run":
		return "run"
	case source == "metis mcp-serve run_task":
		return "mcp"
	case strings.HasPrefix(source, "cron job "):
		return "cron"
	case strings.HasPrefix(source, "metis daemon "):
		return "daemon"
	case strings.HasPrefix(source, "metis coordinator "):
		return "coordinator"
	default:
		return "headless"
	}
}

// collectHeadlessEvents owns the producer channel lifecycle. Loop.Run does not
// close caller-owned channels; the wrapper must close after Run returns on
// success, error or cancellation so range consumers cannot wait forever.
func collectHeadlessEvents(run func(events chan<- agent.Event) error) (string, error) {
	if run == nil {
		return "", nil
	}
	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		defer close(events)
		done <- run(events)
	}()

	var text strings.Builder
	var eventErr error
	var incompleteReason string
	for event := range events {
		switch event.Kind {
		case agent.EventTextDelta:
			text.WriteString(event.TextDelta)
		case agent.EventError:
			if eventErr == nil {
				eventErr = event.Err
			}
		case agent.EventLoopDone:
			if agent.IsIncompleteStopReason(event.StopReason) {
				incompleteReason = event.StopReason
			}
		}
	}
	if err := <-done; err != nil {
		return text.String(), err
	}
	if eventErr == nil && incompleteReason != "" {
		eventErr = fmt.Errorf("task incomplete: %s", incompleteReason)
	}
	return text.String(), eventErr
}

// runHeadlessOneShot is shared by the daemon and coordinator worker. A single
// implementation keeps their cancellation/channel-close behavior identical
// and establishes exactly one success durability boundary per task.
func runHeadlessOneShot(ctx context.Context, r *runtime, prompt, source string) (string, error) {
	if r == nil || r.loop == nil {
		return "", fmt.Errorf("%s: runtime loop is unavailable", source)
	}
	r.loop.AppendUser(prompt)
	text, runErr := collectHeadlessEvents(func(events chan<- agent.Event) error {
		return rtpkg.RunWithTraceTurn(ctx, r.sessionID, func(turnCtx context.Context) error {
			return r.loop.Run(turnCtx, events)
		})
	})
	if runErr != nil {
		return text, runErr
	}
	if err := r.persistHeadlessMemoryBoundary(source, runtimeDistillationShutdownGrace); err != nil {
		return text, err
	}
	return text, nil
}
