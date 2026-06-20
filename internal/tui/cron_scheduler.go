package tui

// cron_scheduler.go — the in-session cron scheduler, claude-code's
// useScheduledTasks (src/hooks/useScheduledTasks.ts). It mounts on the
// per-session CronService and fires session-only (ephemeral) jobs that
// the model scheduled via the CronCreate tool, by feeding their prompt
// into this live chat — exactly like CC enqueues a fired prompt into the
// REPL's command queue and drains it between turns.
//
// Durable jobs are NOT fired here; those persist to disk and run via the
// standalone `metis cron start` daemon. The split (ephemeral→in-session,
// durable→daemon) means the two firing paths never touch the same job, so
// there's no double-fire even with a daemon running alongside the chat.

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/notify"
	"github.com/Ricardo-M-L/metis/internal/runtime"
)

// cronTickInterval is how often the chat polls for due session-only jobs.
// 2s is well under any reasonable schedule granularity (the daemon polls
// per-job; here a small fixed tick keeps the chat loop trivial) and cheap:
// FireDueEphemeral is an in-memory scan of a tiny job map.
const cronTickInterval = 2 * time.Second

// cronFireTickMsg is the recurring in-session scheduler tick.
type cronFireTickMsg time.Time

func cronTickCmd() tea.Cmd {
	return tea.Tick(cronTickInterval, func(t time.Time) tea.Msg {
		return cronFireTickMsg(t)
	})
}

// handleCronTick fires every due session-only job into the live chat and
// re-arms the tick. Idle → start a turn now; mid-turn → queue at Next so it
// drains when the current turn finishes (claude-code's between-turns
// semantics). When several fire at once, the first starts a turn and the
// rest queue behind it (only one turn runs at a time).
func (m *Model) handleCronTick(now time.Time) (tea.Model, tea.Cmd) {
	if m.cronSvc == nil {
		return m, nil
	}
	due := m.cronSvc.FireDueEphemeral(now)
	cmds := []tea.Cmd{cronTickCmd()} // always re-arm, even if nothing fired
	for _, job := range due {
		// Make the fire visible — the user must see the chat didn't type
		// this itself. notify too, in case they've tabbed away.
		label := cronJobLabel(job)
		m.messages = append(m.messages, Message{
			Role:      "info",
			Content:   "⏰ scheduled task fired: " + label,
			Timestamp: now,
		})
		notify.SendNotification("metis", "scheduled task fired: "+label)

		if m.turnActive {
			m.enqueueQueuedItem(job.Prompt, QueuePriorityNext)
		} else {
			cmds = append(cmds, m.beginTurn(job.Prompt)) // sets turnActive=true
		}
	}
	return m, tea.Batch(cmds...)
}

// cronJobLabel is a short human tag for a fired job — its name if set,
// else a prompt snippet. Uses the package's rune-aware truncate so a
// multibyte (e.g. Chinese) prompt isn't sliced mid-codepoint.
func cronJobLabel(j *agent.CronJob) string {
	if j.Name != "" {
		return j.Name
	}
	return truncate(j.Prompt, 50)
}

// beginTurn starts a fresh agent turn from programmatically-supplied text
// (a fired cron prompt), mirroring the plain-text tail of handleSubmit.
// Kept separate because cron prompts are always plain text — no @-file /
// image-paste handling — so this is the minimal, safe subset of the submit
// path. MUST run on the bubbletea update goroutine (it writes m.turnCancel
// and spawns runTurnAsync), which it does: handleCronTick is an Update case.
func (m *Model) beginTurn(text string) tea.Cmd {
	// Idempotency guard: never start a second turn over a live one. Callers
	// (handleCronTick) already check m.turnActive, but this makes beginTurn
	// safe even if a future caller forgets — two runTurnAsync goroutines
	// would share doneCh (buffer 1) and clobber m.turnCancel (leaking the
	// first turn's cancel, making it uncancelable).
	if m.turnActive {
		return nil
	}
	m.loop.AppendUser(text)
	if m.session != nil && m.sessionID != "" {
		_ = m.session.AppendMessage(m.sessionID, lastUserMessage(m.loop.History()))
	}
	_ = runtime.AppendHistory(runtime.HistoryEntry{
		SessionID: m.sessionID, Input: text, Source: "cron",
	})
	m.messages = append(m.messages, Message{Role: "user", Content: text, Timestamp: time.Now()})

	m.streamingText = ""
	m.turnActive = true
	m.spinnerActive = true
	m.spinnerFrame = 0
	m.spinnerStartedAt = time.Now()
	notify.SendProgress(notify.ProgressIndeterminate, 0)
	m.firstStreamAt = time.Time{}
	m.spinnerVerb = chooseSpinnerVerb(m.sessionID)
	m.spinnerSub = ""
	m.spinnerPhase = "requesting"
	m.showBanner = false

	turnCtx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel
	go runTurnAsync(turnCtx, cancel, m.loop, m.eventCh, m.doneCh)
	return tickCmd
}
