package tui

// Plain-readline slash-signal support. Bubble Tea owns overlays and editor
// state, but most slash commands are backed by the same Loop, session store,
// permission gate, renderers, and disk catalog. Keep that shared work real and
// classify the genuinely TUI-only signals explicitly so a command can never
// disappear just because stdout is redirected or --no-tui was selected.

import (
	"context"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/slash"
)

type plainREPLSignalClass uint8

const (
	plainREPLSignalUnknown plainREPLSignalClass = iota
	plainREPLSignalBackend
	plainREPLSignalTUIOnly
)

// classifyPlainREPLSignal is deliberately exhaustive over the current Signal
// enum. Run consults it before dispatch, and the registry-coverage test
// consults the same function, so adding a command cannot silently recreate the
// old empty-switch behavior.
func classifyPlainREPLSignal(sig slash.Signal) plainREPLSignalClass {
	switch sig {
	case slash.SignalNone:
		return plainREPLSignalBackend
	case slash.SignalBtw,
		slash.SignalDiff,
		slash.SignalLoop,
		slash.SignalVim,
		slash.SignalTheme,
		slash.SignalThinkingDisplay:
		return plainREPLSignalTUIOnly
	case slash.SignalQuit,
		slash.SignalClear,
		slash.SignalCompact,
		slash.SignalReload,
		slash.SignalPlan,
		slash.SignalAcceptEdits,
		slash.SignalBypassPermissions,
		slash.SignalFullAccess,
		slash.SignalDefault,
		slash.SignalDontAsk,
		slash.SignalNew,
		slash.SignalRetry,
		slash.SignalUndo,
		slash.SignalRewind,
		slash.SignalHistory,
		slash.SignalSave,
		slash.SignalBranch,
		slash.SignalStatus,
		slash.SignalTitle,
		slash.SignalTools,
		slash.SignalSessions,
		slash.SignalSession,
		slash.SignalSkills,
		slash.SignalVersion,
		slash.SignalAddDir,
		slash.SignalRemoveDir,
		slash.SignalListDirs,
		slash.SignalBatch,
		slash.SignalCost,
		slash.SignalDoctor,
		slash.SignalStats,
		slash.SignalKeybindings,
		slash.SignalPermissions,
		slash.SignalHooks,
		slash.SignalExport,
		slash.SignalReleaseNotes,
		slash.SignalEffort,
		slash.SignalPRComments,
		slash.SignalUpgrade,
		slash.SignalContext,
		slash.SignalResume,
		slash.SignalRename,
		slash.SignalTag,
		slash.SignalCustomPrompt:
		return plainREPLSignalBackend
	default:
		return plainREPLSignalUnknown
	}
}

func plainREPLTUIOnlyMessage(sig slash.Signal) string {
	feature := "this command"
	suggestion := "start metis in the interactive TUI"
	switch sig {
	case slash.SignalBtw:
		feature = "/btw side-question overlay"
	case slash.SignalDiff:
		feature = "/diff interactive viewer"
		suggestion = "start metis in the interactive TUI, or use /git diff for raw output"
	case slash.SignalLoop:
		feature = "/loop interactive control"
		suggestion = "use /cron in plain REPL, or start the interactive TUI"
	case slash.SignalVim:
		feature = "/vim modal input"
	case slash.SignalTheme:
		feature = "/theme live color switching"
	case slash.SignalThinkingDisplay:
		feature = "/thinking transcript display controls"
	}
	return fmt.Sprintf("%s is unavailable in the plain readline REPL; %s", feature, suggestion)
}

func renderREPLStats(r *REPL) string {
	if r == nil || r.Loop == nil {
		return "stats: no active agent loop"
	}
	history := r.Loop.History()
	toolCalls := 0
	toolErrors := 0
	for _, msg := range history {
		for _, block := range msg.Content {
			switch block.Type {
			case "tool_use":
				toolCalls++
			case "tool_result":
				if block.IsError {
					toolErrors++
				}
			}
		}
	}
	rows := usageActivityRows(r.Session)
	rows = append(rows,
		infoRow{Key: "", Value: ""},
		infoRow{Key: "", Value: "Current Session Stats"},
	)
	rows = append(rows, []infoRow{
		{Key: "session id", Value: r.SessionID},
		{Key: "user turns", Value: fmt.Sprintf("%d", transcript.CountTurns(history))},
		{Key: "tool calls", Value: fmt.Sprintf("%d", toolCalls)},
		{Key: "tool errors", Value: fmt.Sprintf("%d", toolErrors)},
		{Key: "input tokens", Value: fmtThousands(r.totalTokens.Input())},
		{Key: "output tokens", Value: fmtThousands(r.totalTokens.Output())},
		{Key: "loop iterations", Value: fmt.Sprintf("%d", r.Loop.IterIdx()), Hint: "actual iterations completed in this session"},
		{Key: "iteration cap", Value: formatIterationCap(r.Loop.MaxIters), Hint: "maximum per user turn"},
		{Key: "history msgs", Value: fmt.Sprintf("%d", len(history))},
	}...)
	return renderInfoBox("Usage & Activity", rows)
}

func renderREPLContext(r *REPL) string {
	if r == nil || r.Loop == nil {
		return "context: no active agent loop"
	}
	return renderContext(&Model{
		loop:        r.Loop,
		model:       r.model,
		skillDir:    r.skillDir,
		totalTokens: r.totalTokens,
	})
}

func renderREPLKeybindings() string {
	return renderInfoBox("Readline REPL Keybindings", []infoRow{
		{Key: "Ctrl-C", Value: "clear input; on an empty line, exit"},
		{Key: "Ctrl-D", Value: "exit"},
		{Key: "Up / Down", Value: "navigate input history"},
		{Key: "Ctrl-R", Value: "search input history"},
		{Key: "Tab", Value: "complete slash commands"},
		{Key: "<<< ... >>>", Value: "enter a multi-line prompt"},
	})
}

func (r *REPL) reloadDiskCatalog() string {
	if r == nil {
		return "reload: REPL unavailable"
	}
	skillCount := 0
	skillErr := error(nil)
	if loaded, err := loadSkillCatalog(r.Loop, r.skillDir); err != nil {
		skillErr = err
	} else {
		skillCount = len(loaded)
	}
	customCount := 0
	if r.Slash != nil {
		r.Slash.RemoveCustom()
		customCount = len(slash.LoadCustomCommandsWithSandbox(r.Slash, config.Home(), r.sandbox))
	}
	if skillErr != nil {
		return fmt.Sprintf("reload: custom commands=%d; skills: %v", customCount, skillErr)
	}
	return fmt.Sprintf("reload: %d skills · %d custom commands", skillCount, customCount)
}

func (r *REPL) retryLastResponse(ctx context.Context) {
	if r == nil || r.Loop == nil {
		fmt.Fprintln(r.out, "retry: no active agent loop")
		return
	}
	lastUser, ok := r.Loop.UndoLastTurnWithPrefill()
	if !ok || strings.TrimSpace(lastUser) == "" {
		fmt.Fprintln(r.out, r.Styles.Hint.Render("(retry: no prior user prompt found in history)"))
		return
	}
	if err := r.replaceHistory(r.Loop.History()); err != nil {
		fmt.Fprintln(r.out, r.Styles.Err.Render("retry: failed to persist rollback: "+err.Error()))
		return
	}
	fmt.Fprintln(r.out, r.Styles.Hint.Render("(retrying last response)"))
	r.Loop.AppendUser(lastUser)
	r.persistTail()
	_ = runtime.AppendHistory(runtime.HistoryEntry{
		SessionID: r.SessionID,
		Input:     lastUser,
		Source:    "repl-retry",
	})
	if err := r.runTurn(ctx); err != nil {
		fmt.Fprintln(r.out, r.Styles.Err.Render("retry: "+err.Error()))
	}
}

func (r *REPL) rewindLastEditTurn() string {
	if r == nil || r.Loop == nil {
		return "rewind: no active agent loop"
	}
	res, ok := r.Loop.Rewind()
	if !ok {
		return "(nothing to rewind — no file snapshots yet, or checkpointing is off)"
	}
	if err := r.replaceHistory(r.Loop.History()); err != nil {
		return "rewind applied but failed to persist conversation: " + err.Error()
	}
	return fmt.Sprintf("(rewound: restored files + undid %d turn(s) — %s)", res.TurnsUndone, res.Label)
}

func (r *REPL) setPermissionMode(mode permission.Mode, label string) {
	if r == nil || r.Gate == nil {
		fmt.Fprintln(r.out, "mode: permission gate unavailable")
		return
	}
	if err := applyREPLPermissionMode(r, mode); err != nil {
		fmt.Fprintln(r.out, "mode unchanged: "+err.Error())
		return
	}
	fmt.Fprintln(r.out, "(mode set: "+label+")")
}
