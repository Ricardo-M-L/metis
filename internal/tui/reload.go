package tui

// reload.go — single state-reset hub for /clear, /new, /reset, and
// any future "rebuild the session in place" signal. Mirrors
// kimi-cli's Reload exception pattern: instead of every command's
// handler duplicating a 4-line reset triplet, they all call Reload
// with the right policy flags.
//
// Why a struct of flags instead of distinct methods: the next time
// we add a "wipe X but not Y" reset (e.g. /undo prefill keeps the
// input populated; /branch saves daily note first; /fork preserves
// banner) we can extend ReloadOpts without adding a new method or
// duplicating reset logic. claude-code's `Reload({reason, prefill})`
// shape served the same goal.

import (
	"fmt"
	"time"
)

// ReloadOpts controls what state Model.Reload should clear or keep.
// Zero value means "clear everything that's reset-worthy" (the
// /clear default). Use OptKeepBanner / OptSaveDailyNote / OptPrefill
// to customise.
type ReloadOpts struct {
	// SaveDailyNote — if true and a memory manager is wired, capture
	// the current history into a daily-note before resetting. Only
	// /new uses this; /clear is a "discard, don't archive" command.
	SaveDailyNote bool
	// ResetReason — recorded in the daily note title so the
	// archive distinguishes /new from auto-cleared sessions.
	ResetReason string
	// ShowBanner — when true, restore the welcome banner state so
	// the TUI redraws it on next render. /new sets this; /clear
	// (which the user types repeatedly) doesn't, to avoid banner
	// flash mid-conversation.
	ShowBanner bool
	// Prefill — text to drop into the input box after reset. Used
	// by /undo to pre-populate with the popped user text. Empty =
	// leave input untouched.
	Prefill string
	// PreserveInput — when true, don't clear the textarea. /undo
	// uses Prefill (which sets new content); other resets implicitly
	// clear input.
	PreserveInput bool
}

// Reload performs the consolidated reset. It returns a persistence error and
// leaves the live conversation intact when the clearing snapshot cannot be
// written; callers surface that error in the current UI.
//
// Order matters: save-daily-note happens BEFORE loop.Reset() because
// summarizeHistory reads from the loop's history.
func (m *Model) Reload(opts ReloadOpts) error {
	var noteErr error
	if opts.SaveDailyNote && m.loop != nil && m.loop.Memory != nil {
		summary := m.summarizeHistory()
		reason := opts.ResetReason
		if reason == "" {
			reason = "reload"
		}
		// 2026-05-22: surface SaveDailyNote errors instead of
		// silently dropping. The note IS the user's audit trail
		// for "what happened in the session I just cleared" — a
		// write failure here used to vanish; now it shows in
		// the chat as a warning row so the user can at least
		// retry from a snapshot.
		noteErr = m.loop.Memory.SaveDailyNote(m.sessionID, reason, summary)
	}
	// Persist the empty target history before mutating the live loop. If the
	// snapshot append fails, leave the conversation intact so /clear can be
	// retried instead of reporting success while resume would resurrect it.
	if m.loop != nil && m.session != nil && m.sessionID != "" {
		if err := m.session.ReplaceHistoryAndMark(m.sessionID, nil, &m.historyCursor); err != nil {
			return fmt.Errorf("persist cleared history: %w", err)
		}
	} else {
		m.historyCursor.Mark(nil)
	}
	if m.loop != nil {
		m.loop.Reset()
	}
	m.messages = nil
	m.toolEvents = nil
	m.turnToolEventStart = 0
	m.totalTokens.Reset()
	if !opts.PreserveInput {
		m.input.Reset()
	}
	// /clear is intercepted by handleKey before the ordinary submit path gets
	// a chance to dismiss slash completion. Reset the complete palette cache
	// here so clearing a conversation cannot leave a stale `/clear` row under
	// the now-empty editor.
	m.dismissPalette()
	if opts.Prefill != "" {
		m.input.SetValue(opts.Prefill)
	}
	if opts.ShowBanner {
		m.firstRender = true
		m.showBanner = true
	}
	if noteErr != nil {
		m.messages = append(m.messages, Message{
			Role:      "warning",
			Content:   fmt.Sprintf("(reload: failed to save daily note: %v — session history is cleared but no audit trail was written)", noteErr),
			Timestamp: time.Now(),
		})
	}
	return nil
}

// ReloadEcho appends a one-line confirmation to the message stream.
// Helper kept here so the same banner format covers every reset path.
func (m *Model) ReloadEcho(msg string) {
	m.messages = append(m.messages, Message{Role: "success", Content: msg, Timestamp: time.Now()})
}
