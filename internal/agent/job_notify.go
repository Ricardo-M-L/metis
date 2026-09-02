package agent

// job_notify.go — drains the bash job pool's notification channel at
// every Run iteration boundary and synthesizes <job_notification>
// system-reminder user messages so the model sees finished background
// commands without polling.
//
// Why a synthetic user message instead of a hook event: the model
// only "sees" what's in the prompt. Hook events are a side channel
// for tooling. A user-message is the unambiguous "here is something
// the agent should react to right now" — same envelope claude-code
// uses (<task_notification>...</task_notification> wrapping a JSON
// blob). The wrapping tag tells the model "this isn't a real user,
// don't reply conversationally — act on it."

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/security"
)

// injectJobNotifications drains every pending notification on
// l.JobNotify and appends one synthetic user message summarising the
// batch. Multiple jobs that finish within the same window collapse
// into one message to avoid spamming the prompt.
//
// No-op when JobNotify is nil (sub-agents, headless tests) or when
// the channel is empty (the common case — most iterations don't
// have any finished jobs to report).
func (l *Loop) injectJobNotifications(ctx context.Context, out chan<- Event) []jobs.Notification {
	return l.injectJobNotificationsForAwaited(ctx, out, nil)
}

func (l *Loop) injectJobNotificationsForAwaited(
	ctx context.Context,
	out chan<- Event,
	awaited map[string]struct{},
) []jobs.Notification {
	if l.JobNotify == nil {
		return nil
	}
	notifs := drainChan(l.JobNotify)
	if len(notifs) == 0 {
		return nil
	}
	completedAwaited := make(map[string]struct{})
	for _, notification := range notifs {
		if _, ok := awaited[notification.JobID]; ok {
			completedAwaited[notification.JobID] = struct{}{}
		}
	}
	l.injectJobNotificationBatchWithOutputs(ctx, out, notifs, l.readAwaitedJobOutputs(completedAwaited))
	return notifs
}

func (l *Loop) injectJobNotificationBatch(ctx context.Context, out chan<- Event, notifs []jobs.Notification) {
	if len(notifs) == 0 {
		return
	}
	l.injectJobNotificationBatchWithOutputs(ctx, out, notifs, nil)
	return
}

func (l *Loop) injectJobNotificationBatchWithOutputs(
	ctx context.Context,
	out chan<- Event,
	notifs []jobs.Notification,
	outputs map[string]string,
) {
	if len(notifs) == 0 {
		return
	}
	l.appendInjectedMessage(formatJobNotificationsWithOutputs(notifs, outputs))
	// Surface to the TUI too so the user sees the same notification
	// banner the model is reacting to (helps explain why the model
	// suddenly says "I see job bg_xxx finished, ...").
	emit(ctx, out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("[job pool] %d notification(s) injected", len(notifs)),
	})
}

// recordAwaitedBackgroundJobs discovers background jobs that represent an
// intentional wait redirected out of the foreground. Ordinary servers,
// watchers, and long builds omit await_completion, so they never keep a turn
// open after the model has already delivered a useful final response.
func recordAwaitedBackgroundJobs(results []llm.ContentBlock, pending map[string]struct{}) {
	for _, result := range results {
		if result.Type != "tool_result" || result.Presentation == nil {
			continue
		}
		await, _ := result.Presentation["await_completion"].(bool)
		jobID, _ := result.Presentation["job_id"].(string)
		if await && jobID != "" {
			pending[jobID] = struct{}{}
		}
	}
}

func clearCompletedBackgroundJobs(pending map[string]struct{}, notifs []jobs.Notification) {
	for _, notification := range notifs {
		delete(pending, notification.JobID)
	}
}

// waitForAwaitedJobNotifications is the event-driven half of foreground wait
// redirection. Once the model says it is waiting, keep this Run alive until
// the specific redirected jobs settle, then inject their notification and let
// the model produce the real final answer. The notification channel is only a
// bounded wake-up path: Registry deliberately drops sends when that buffer is
// full, so terminal Registry state is reconciled before every blocking receive.
// No polling loop is used; cancellation and caller-supplied deadlines remain
// immediately responsive. A non-nil error is always the waiting context's
// classified terminal cause (context.Canceled or context.DeadlineExceeded).
func (l *Loop) waitForAwaitedJobNotifications(
	ctx context.Context,
	out chan<- Event,
	pending map[string]struct{},
) (bool, error) {
	if len(pending) == 0 {
		return false, nil
	}
	awaited := make(map[string]struct{}, len(pending))
	for jobID := range pending {
		awaited[jobID] = struct{}{}
	}
	collected := make([]jobs.Notification, 0, len(pending))
	completedAwaited := make(map[string]struct{}, len(pending))
	markCompletedAwaited := func(notifications []jobs.Notification) {
		for _, notification := range notifications {
			if notification.Status == jobs.StatusRunning {
				continue
			}
			if _, ok := awaited[notification.JobID]; ok {
				completedAwaited[notification.JobID] = struct{}{}
			}
		}
	}
	flushCollected := func(flushCtx context.Context) {
		if len(collected) == 0 {
			return
		}
		l.injectJobNotificationBatchWithOutputs(
			flushCtx,
			out,
			collected,
			l.readAwaitedJobOutputs(completedAwaited),
		)
		collected = nil
	}
	flushAfterContextEnd := func() {
		// History injection is durable and happens before the cosmetic EventInfo.
		// Give that event a bounded detached window instead of either dropping it
		// immediately on cancellation or blocking cancellation forever.
		flushCtx, cancelFlush := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
		defer cancelFlush()
		flushCollected(flushCtx)
	}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			// These envelopes have already been removed from JobNotify. Preserve
			// them even though the waiting context can no longer emit normally.
			flushAfterContextEnd()
			return false, context.Cause(ctx)
		}
		// Prefer real channel envelopes when they are already available. Besides
		// preserving unrelated job notifications, this avoids synthesizing an
		// awaited envelope whose actual send is sitting in the buffer.
		if l.JobNotify != nil {
			notifications := drainChan(l.JobNotify)
			collected = append(collected, notifications...)
			markCompletedAwaited(notifications)
			clearCompletedBackgroundJobs(pending, notifications)
		}
		// A full notify buffer intentionally drops completion sends. Job status is
		// the durable source of truth, so recover any such missing awaited edges
		// before blocking for another channel event.
		recovered := l.recoverTerminalAwaitedJobs(pending)
		collected = append(collected, recovered...)
		markCompletedAwaited(recovered)
		if len(pending) == 0 {
			break
		}
		if l.JobNotify == nil {
			flushCollected(ctx)
			return false, nil
		}
		select {
		case <-ctx.Done():
			flushAfterContextEnd()
			return false, context.Cause(ctx)
		case notification, ok := <-l.JobNotify:
			if !ok {
				flushCollected(ctx)
				return false, nil
			}
			collected = append(collected, notification)
			markCompletedAwaited([]jobs.Notification{notification})
			delete(pending, notification.JobID)
		}
	}
	// A wait that was automatically redirected out of foreground execution is
	// semantically different from an ordinary server/watch job: the command's
	// output is the value the model was waiting for. Include that bounded,
	// redacted output with the completion event so the continuation does not
	// have to guess whether BashOutput is available or issue a second tool call.
	// Ordinary background-job notifications stay compact.
	if ctx.Err() != nil {
		flushAfterContextEnd()
	} else {
		flushCollected(ctx)
	}
	return true, nil
}

// recoverTerminalAwaitedJobs reconstructs notifications whose best-effort
// channel delivery was dropped. IDs are sorted so a simultaneous multi-job
// recovery produces deterministic prompt text.
func (l *Loop) recoverTerminalAwaitedJobs(pending map[string]struct{}) []jobs.Notification {
	if l.Jobs == nil || len(pending) == 0 {
		return nil
	}
	ids := make([]string, 0, len(pending))
	for jobID := range pending {
		ids = append(ids, jobID)
	}
	sort.Strings(ids)

	recovered := make([]jobs.Notification, 0, len(ids))
	for _, jobID := range ids {
		job, ok := l.Jobs.Get(jobID)
		if !ok || job.Status == jobs.StatusRunning {
			continue
		}
		elapsed := job.EndTime.Sub(job.StartTime)
		if elapsed < 0 {
			elapsed = 0
		}
		recovered = append(recovered, jobs.Notification{
			JobID:    job.ID,
			Status:   job.Status,
			ExitCode: job.ExitCode,
			Elapsed:  elapsed,
			Command:  security.RedactSubprocessText(job.Command),
		})
		delete(pending, jobID)
	}
	return recovered
}

const (
	awaitedJobOutputPerJobBytes = 16 * 1024
	awaitedJobOutputTotalBytes  = 32 * 1024
)

func (l *Loop) readAwaitedJobOutputs(awaited map[string]struct{}) map[string]string {
	if l.Jobs == nil || len(awaited) == 0 {
		return nil
	}
	remaining := awaitedJobOutputTotalBytes
	outputs := make(map[string]string, len(awaited))
	for jobID := range awaited {
		if remaining <= 0 {
			break
		}
		job, ok := l.Jobs.Get(jobID)
		if !ok || job.OutputPath == "" {
			continue
		}
		limit := awaitedJobOutputPerJobBytes
		if remaining < limit {
			limit = remaining
		}
		body, err := jobs.ReadJobOutput(job.OutputPath, limit)
		if err != nil {
			continue
		}
		body = security.RedactSubprocessText(strings.ReplaceAll(body, "\r\n", "\n"))
		body = strings.TrimSpace(body)
		if body == "" {
			body = "(no captured output)"
		}
		outputs[jobID] = body
		remaining -= len(body)
	}
	return outputs
}

// formatJobNotifications builds the synthetic user message body. The
// outer <job_notification> tags are recognized by the model as a
// system-reminder, mirroring claude-code's <task_notification>
// envelope. The body is human-readable lines plus a hint to use
// BashOutput / BashList for follow-up.
func formatJobNotifications(notifs []jobs.Notification) string {
	return formatJobNotificationsWithOutputs(notifs, nil)
}

func formatJobNotificationsWithOutputs(notifs []jobs.Notification, outputs map[string]string) string {
	var b strings.Builder
	b.WriteString("<job_notification>\n")
	if len(notifs) == 1 {
		b.WriteString("A background bash job finished:\n")
	} else {
		fmt.Fprintf(&b, "%d background bash jobs finished:\n", len(notifs))
	}
	for _, n := range notifs {
		command := security.RedactSubprocessText(n.Command)
		fmt.Fprintf(&b, "  • %s — %s — exit=%d — elapsed=%s — `%s`\n",
			n.JobID,
			n.Status,
			n.ExitCode,
			n.Elapsed.Truncate(time.Second),
			truncateOneLine(command, 60),
		)
		if output, ok := outputs[n.JobID]; ok {
			output = security.RedactSubprocessText(output)
			fmt.Fprintf(&b, "<job_output job_id=%q>\n%s\n</job_output>\n", n.JobID, output)
		}
	}
	if len(outputs) > 0 {
		b.WriteString("Captured output for awaited jobs is included above; use it as the completion result and do not call BashOutput for those jobs unless more context is explicitly needed.\n")
	}
	b.WriteString("Use BashOutput only when captured output is not included or more context is needed; use BashList to see all jobs.\n")
	b.WriteString("</job_notification>")
	return b.String()
}

// truncateOneLine is a tiny helper that collapses newlines and caps
// length so a multi-line heredoc job command renders predictably in
// the notification.
func truncateOneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
