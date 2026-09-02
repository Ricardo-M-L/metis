package tui

// session_switch.go centralizes every in-process top-level session boundary.
// A session is more than its transcript: permissions, tool sidecars, global
// Todo/Task routing, prompt dumps, title, cost and prompt-history caches must
// all move together or the next turn can read/write the previous session.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

const (
	cliDistillationBoundaryGrace = 35 * time.Second
	cliAutoMemoryBoundaryGrace   = 95 * time.Second
)

// persistMemoryBoundary is the shared CLI/TUI durability barrier. The caller
// must stop the foreground turn before entering it. Every successful exchange
// below the normal cadence is flushed to an immutable distillation job, and all
// source-session distillation is joined before the Daily hand-off is written or
// the live history can be replaced. Auto Memory already owns immutable
// snapshots, so session switches may leave it running; process close joins it
// before provider dependencies can be closed.
func persistMemoryBoundary(loop *agent.Loop, sessionID, source, summary string, closing bool) error {
	return persistMemoryBoundaryWithGrace(
		loop,
		sessionID,
		source,
		summary,
		closing,
		cliDistillationBoundaryGrace,
		cliAutoMemoryBoundaryGrace,
	)
}

// persistMemoryBoundaryWithGrace keeps the production policy testable without
// mutating process-global timeout knobs. On an in-process switch, a failed
// distillation join is fail-closed: no Daily note is written and the caller
// leaves the source session active. A clean process close still attempts the
// otherwise-valid final Daily and Auto Memory joins, returning all errors to
// the caller. The wait context is never used to cancel a freshly flushed job.
func persistMemoryBoundaryWithGrace(
	loop *agent.Loop,
	sessionID, source, summary string,
	closing bool,
	distillationGrace, autoMemoryGrace time.Duration,
) error {
	if loop == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	var errs []error

	// Registration happens before waiting, which closes the race between a
	// completed short turn and ResetSession/provider cleanup. Wait only observes
	// the job; unlike CancelAndWaitForDistillation it does not discard residual
	// memory merely because a session boundary was reached.
	loop.FlushPendingDistillation(sessionID)
	distillCtx, cancelDistill := context.WithTimeout(context.Background(), distillationGrace)
	distillErr := loop.WaitForDistillation(distillCtx, sessionID)
	cancelDistill()
	if distillErr != nil {
		distillErr = fmt.Errorf("join memory distillation for %s: %w", sessionID, distillErr)
		if !closing {
			return distillErr
		}
		errs = append(errs, distillErr)
	}

	// Daily is the synchronous, user-visible hand-off record. For a switch it
	// is written only after all source facts are durable. On close it remains
	// useful even if a stubborn provider made the join time out.
	if loop.Memory != nil {
		if err := loop.Memory.SaveDailyNote(sessionID, source, summary); err != nil {
			errs = append(errs, fmt.Errorf("save daily memory for %s (%s): %w", sessionID, source, err))
		}
	}
	if !closing {
		return errors.Join(errs...)
	}

	autoCtx, cancelAuto := context.WithTimeout(context.Background(), autoMemoryGrace)
	if err := loop.WaitAutoMemoryIdle(autoCtx); err != nil {
		errs = append(errs, fmt.Errorf("join auto memory for %s: %w", sessionID, err))
	}
	cancelAuto()

	return errors.Join(errs...)
}

func activationBoundarySource(hdr *session.Header, restorePermissions bool) string {
	if restorePermissions {
		return "cli-resume"
	}
	if hdr != nil && hdr.ForkedFrom != nil {
		return "cli-branch"
	}
	return "cli-new"
}

func (r *REPL) leaveActiveSession(source string, closing bool) error {
	if r == nil || r.sessionBoundaryClosed || r.SessionID == "" {
		return nil
	}
	summary := ""
	if r.Loop != nil {
		summary = r.summarizeHistory()
	}
	if err := persistMemoryBoundary(r.Loop, r.SessionID, source, summary, closing); err != nil {
		return err
	}
	if r.SessionBoundary != nil {
		r.SessionBoundary()
	}
	r.sessionBoundaryClosed = true
	return nil
}

func (m *Model) leaveActiveSession(source string, closing bool) error {
	if m == nil || m.sessionBoundaryClosed || m.sessionID == "" {
		return nil
	}
	summary := ""
	if m.loop != nil {
		summary = m.summarizeHistory()
	}
	if err := persistMemoryBoundary(m.loop, m.sessionID, source, summary, closing); err != nil {
		return err
	}
	if m.ext.SessionBoundary != nil {
		m.ext.SessionBoundary()
	}
	m.sessionBoundaryClosed = true
	return nil
}

// stopForegroundTurnForClose handles the rare quit-while-cancelling path after
// Bubble Tea has stopped consuming doneCh. This is deliberately condition-
// based rather than timeout-based: returning while Loop.Run is still mutating
// history would let runtime cleanup observe no background writer, close its
// dependencies, and lose the final transcript/Daily tail. An OS-level force
// kill remains the explicit escape hatch for a provider that never returns.
func (m *Model) stopForegroundTurnForClose() error {
	if m == nil || !m.turnActive {
		return nil
	}
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
	<-m.doneCh
	var persistErr error
	if m.session != nil && m.sessionID != "" && m.loop != nil {
		if err := m.session.AppendHistoryTail(m.sessionID, m.loop.History(), &m.historyCursor); err != nil {
			persistErr = fmt.Errorf("persist foreground turn for session %s: %w", shortID(m.sessionID), err)
		}
	}
	m.turnActive = false
	m.spinnerActive = false
	return persistErr
}

// cleanupUnactivatedSession removes a fresh/fork destination whose activation
// failed before it ever became live. Resume targets are never passed here: they
// pre-existed the attempt and must remain available for retry.
func cleanupUnactivatedSession(store *session.Store, id string, activationErr error) error {
	if activationErr == nil || store == nil || strings.TrimSpace(id) == "" {
		return activationErr
	}
	if err := store.Delete(id); err != nil {
		return errors.Join(activationErr, fmt.Errorf("discard unactivated session %s: %w", shortID(id), err))
	}
	return activationErr
}

func (m *Model) activateCreatedSession(id string, hdr *session.Header, messages []llm.Message) error {
	if err := m.activateSession(id, hdr, messages, false); err != nil {
		return cleanupUnactivatedSession(m.session, id, err)
	}
	return nil
}

// freshPermissionMode returns the invocation-level posture for a newly
// created/forked session. Production supplies the fully-resolved value (CLI,
// agent profile and dangerous-skip override included) via ExternalHooks. Tests
// and embedders fall back to config, then to ask rather than inheriting a
// possibly-bypass mode from the session being left.
func (m *Model) freshPermissionMode() permission.Mode {
	if m.ext.FreshPermissionMode != "" {
		return m.ext.FreshPermissionMode
	}
	if m.cfg != nil && m.cfg.Permission.Mode != "" {
		return permission.Mode(m.cfg.Permission.Mode)
	}
	return permission.ModeAsk
}

// persistActiveSessionState snapshots mutable permission state before leaving
// a session. Only interactive/session rules belong in the session header;
// config, persistent, CLI and managed-policy rules are rebuilt/preserved at
// process scope and copying them would both duplicate and elevate stale state.
func (m *Model) persistActiveSessionState() error {
	if m == nil || m.session == nil || m.gate == nil || m.loop == nil || m.sessionID == "" {
		return nil
	}
	return persistSessionState(m.session, m.sessionID, m.gate, m.loop, m.providerName, m.model, m.loop.System)
}

func persistPermissionState(store *session.Store, sessionID string, gate *permission.Gate) error {
	return persistSessionState(store, sessionID, gate, nil, "", "", "")
}

func persistSessionState(store *session.Store, sessionID string, gate *permission.Gate, controller rtpkg.PermissionPlanController, providerName, model, system string) error {
	if store == nil || gate == nil || sessionID == "" {
		return nil
	}
	permissionState, err := rtpkg.CapturePermissionModeState(gate, controller)
	if err != nil {
		return fmt.Errorf("persist session %s permission state: %w", sessionID, err)
	}
	rules := make([]session.SavedRule, 0)
	for _, rule := range gate.Snapshot() {
		if rule.Source != "interactive" && !strings.HasPrefix(rule.Source, "session:") {
			continue
		}
		rules = append(rules, session.SavedRule{
			Tool: rule.Tool, Match: rule.Match, Verb: int(rule.Verb), Source: rule.Source,
		})
	}
	if err := store.WriteHeaderFull(session.Header{
		ID:               sessionID,
		Provider:         providerName,
		Model:            model,
		System:           system,
		Mode:             string(permissionState.Mode),
		PrePlanMode:      permissionState.PrePlanMode,
		AlwaysAllow:      rules,
		ClearAlwaysAllow: len(rules) == 0,
	}); err != nil {
		return fmt.Errorf("persist session %s state: %w", sessionID, err)
	}
	return nil
}

func systemPromptKindForSections(sections []llm.SystemSection) string {
	for _, section := range sections {
		if section.Name == "identity" {
			return session.SystemPromptKindDefault
		}
	}
	return session.SystemPromptKindCustom
}

func (r *REPL) freshPermissionMode() permission.Mode {
	if r != nil && r.FreshPermissionMode != "" {
		return r.FreshPermissionMode
	}
	return permission.ModeAsk
}

func (r *REPL) rebindFreshSession(id string) {
	r.SessionID = id
	if r.historyCursor == nil {
		cursor := session.NewHistoryCursor(r.Loop.History())
		r.historyCursor = &cursor
	} else {
		r.historyCursor.Mark(r.Loop.History())
	}
	rtpkg.RebindLoopRuntime(r.Loop, r.Loop.Provider, r.model, r.Loop.System, id)
	mode := r.freshPermissionMode()
	prePlanMode := ""
	if mode == permission.ModePlan {
		prePlanMode = string(permission.ModeDefault)
	}
	r.Gate.ResetSessionStateAndWait(mode, nil, func() {
		r.Loop.SetPrePlanMode(prePlanMode)
	}, func() {
		rtpkg.SynchronizeRestoredPermissionState(r.Gate, r.Loop, prePlanMode)
	})
	r.Loop.TimingSink = r.Session.NewTimingRecorder(id).Record
	r.totalTokens.Reset()
	if r.SessionSwitch != nil {
		r.SessionSwitch(id)
	}
	r.sessionBoundaryClosed = false
}

func (r *REPL) startFreshSession() (string, error) {
	if r == nil || r.Session == nil || r.Loop == nil || r.Gate == nil {
		return "", fmt.Errorf("session runtime unavailable")
	}
	if err := persistSessionState(r.Session, r.SessionID, r.Gate, r.Loop, r.providerName, r.model, r.Loop.System); err != nil {
		return "", err
	}
	id := r.Session.NewSessionID()
	system := r.baseSystem
	cwd, _ := os.Getwd()
	if err := r.Session.WriteHeaderFull(session.Header{
		ID: id, Provider: r.providerName, Model: r.model, System: system,
		SystemPromptKind: systemPromptKindForSections(r.baseSystemSections), WorkDir: cwd,
		Mode: string(r.freshPermissionMode()),
	}); err != nil {
		return "", err
	}
	if err := r.leaveActiveSession("cli-new", false); err != nil {
		return "", cleanupUnactivatedSession(r.Session, id, err)
	}
	r.Loop.System = system
	r.Loop.SystemSections = append([]llm.SystemSection(nil), r.baseSystemSections...)
	r.Loop.ResetSession(nil)
	r.rebindFreshSession(id)
	return id, nil
}

func (r *REPL) branchSession() (string, error) {
	if r == nil || r.Session == nil || r.Loop == nil || r.Gate == nil || r.SessionID == "" {
		return "", fmt.Errorf("session runtime unavailable")
	}
	if err := persistSessionState(r.Session, r.SessionID, r.Gate, r.Loop, r.providerName, r.model, r.Loop.System); err != nil {
		return "", err
	}
	id, err := r.Session.Branch(r.SessionID, r.Loop.History())
	if err != nil {
		return "", err
	}
	cwd, _ := os.Getwd()
	if err := r.Session.WriteHeaderFull(session.Header{
		ID: id, WorkDir: cwd, Mode: string(r.freshPermissionMode()),
		// Branch reads the source header from disk, which may predate the
		// dynamic Plan overlay migration even though this live Loop was cleaned
		// at resume. Persist the live cleaned system so the child does not inherit
		// an obsolete static "do not implement" instruction.
		System: rtpkg.RemoveLegacyPlanOverlay(r.Loop.System),
	}); err != nil {
		return "", cleanupUnactivatedSession(r.Session, id, err)
	}
	if err := r.leaveActiveSession("cli-branch", false); err != nil {
		return "", cleanupUnactivatedSession(r.Session, id, err)
	}
	r.Loop.ResetSession(r.Loop.History())
	r.rebindFreshSession(id)
	return id, nil
}

func (m *Model) createFreshSession() (string, *session.Header, error) {
	if m == nil || m.session == nil || m.loop == nil {
		return "", nil, fmt.Errorf("session runtime unavailable")
	}
	id := m.session.NewSessionID()
	system := m.baseSystem
	cwd, _ := os.Getwd()
	hdr := &session.Header{
		ID: id, Provider: m.providerName, Model: m.model, System: system,
		SystemPromptKind: systemPromptKindForSections(m.baseSystemSections), WorkDir: cwd,
		Mode: string(m.freshPermissionMode()),
	}
	if err := m.session.WriteHeaderFull(*hdr); err != nil {
		return "", nil, err
	}
	return id, hdr, nil
}

// forkSession copies the selected transcript/model/system lineage but starts a
// new permission lifetime. In particular, an always-allow/bypass grant from
// either the currently-active session or the selected parent is not inherited.
func (m *Model) forkSession(parentID string, messages []llm.Message) (string, *session.Header, error) {
	if m == nil || m.session == nil {
		return "", nil, fmt.Errorf("session runtime unavailable")
	}
	id, err := m.session.Branch(parentID, messages)
	if err != nil {
		return "", nil, err
	}
	cwd, _ := os.Getwd()
	if err := m.session.WriteHeaderFull(session.Header{
		ID: id, WorkDir: cwd, Mode: string(m.freshPermissionMode()),
	}); err != nil {
		return "", nil, cleanupUnactivatedSession(m.session, id, err)
	}
	hdr, _, err := m.session.LoadHeader(id)
	if err != nil {
		return "", nil, cleanupUnactivatedSession(m.session, id, err)
	}
	cleanedSystem := rtpkg.RemoveLegacyPlanOverlay(hdr.System)
	if cleanedSystem != hdr.System {
		// The fork's initial header was copied from the selected parent. Append a
		// corrected header immediately so the new branch is clean even if it is
		// never activated in this process.
		if cleanedSystem != "" {
			if err := m.session.WriteHeaderFull(session.Header{ID: id, System: cleanedSystem}); err != nil {
				return "", nil, cleanupUnactivatedSession(m.session, id, fmt.Errorf("clean legacy plan overlay from fork %s: %w", id, err))
			}
		}
		hdr.System = cleanedSystem
	}
	return id, hdr, nil
}

// activateSession commits a fully-loaded destination session to the live TUI.
// restorePermissions controls whether header Mode/AlwaysAllow belong to the
// destination (resume) or the destination starts at invocation defaults
// (fresh/fork). Provider/model selection is the only fallible preflight and
// runs before any gate, prompt, transcript, ID, sidecar or UI state changes.
// A preflight error therefore leaves the source session completely usable.
func (m *Model) activateSession(id string, hdr *session.Header, messages []llm.Message, restorePermissions bool) error {
	if m == nil || m.loop == nil || m.gate == nil || m.session == nil {
		return fmt.Errorf("session runtime unavailable")
	}
	if id == "" {
		return fmt.Errorf("empty session id")
	}
	sourceRuntime := m.loop.ProviderRuntimeState()
	sourceModel := m.model
	sourceProviderName := m.providerName
	sourceBaseSystem := m.baseSystem
	sourceBaseSections := append([]llm.SystemSection(nil), m.baseSystemSections...)
	sourceSessionID := m.sessionID
	restoreSourceRuntime := func() {
		m.loop.RebindProviderRuntime(
			sourceRuntime.Provider,
			sourceRuntime.Model,
			sourceRuntime.MaxOutputTokens,
			sourceRuntime.System,
			sourceRuntime.SystemSections,
		)
		m.model = sourceModel
		m.providerName = sourceProviderName
		m.baseSystem = sourceBaseSystem
		m.baseSystemSections = sourceBaseSections
		rtpkg.RebindLoopRuntime(m.loop, sourceRuntime.Provider, sourceRuntime.Model, sourceRuntime.System, sourceSessionID)
	}

	// Provider/model preflight must happen before any other live state changes.
	// switchModel itself is atomic on BuildProvider failure. Session activation
	// is stricter than the standalone /model test fallback: without config we
	// cannot prove that a different model/profile is backed by a matching live
	// transport, so abort instead of doing a string-only swap.
	if hdr != nil {
		targetModel := hdr.Model
		if targetModel == "" {
			targetModel = m.model
		}
		targetProvider := hdr.Provider
		if targetProvider == "" {
			targetProvider = m.providerName
		}
		if targetProvider == "" && m.cfg != nil {
			targetProvider = m.cfg.Provider.Default
		}
		if targetModel != m.model || targetProvider != m.providerName {
			if m.cfg == nil || targetProvider == "" {
				return fmt.Errorf("provider/model preflight for %s: provider configuration unavailable", shortID(id))
			}
			if err := m.switchModel(targetModel, targetProvider); err != nil {
				return fmt.Errorf("provider/model preflight for %s: %w", shortID(id), err)
			}
		}
	}

	// Metis <=0.2.8 persisted Plan instructions in Header.System. Startup
	// resume cleans them in cmd/metis, but the in-process session picker bypasses
	// that boot path. Clean and persist the destination before committing the
	// switch so leaving plan mode actually removes the old static restriction.
	destinationSystem := ""
	if hdr != nil {
		if hdr.SystemPromptKind == session.SystemPromptKindDefault {
			destinationSystem = m.baseSystem
		} else {
			destinationSystem = rtpkg.RemoveLegacyPlanOverlay(hdr.System)
		}
		if destinationSystem != hdr.System && destinationSystem != "" {
			if err := m.session.WriteHeaderFull(session.Header{ID: id, System: destinationSystem}); err != nil {
				restoreSourceRuntime()
				return fmt.Errorf("clean legacy plan overlay from session %s: %w", shortID(id), err)
			}
		}
	}

	mode := m.freshPermissionMode()
	prePlanMode := ""
	if mode == permission.ModePlan {
		prePlanMode = string(permission.ModeDefault)
	}
	var resumedRules []permission.Rule
	if restorePermissions && hdr != nil {
		validatedMode, validatedPrePlan, hasMode, err := rtpkg.ValidateRestoredPermissionStateFromSource(
			hdr.Mode, hdr.PrePlanMode, mode, m.ext.TrustSessionPermissions,
		)
		if err != nil {
			restoreSourceRuntime()
			return fmt.Errorf("restore permission state for %s: %w", shortID(id), err)
		}
		if hasMode {
			mode = validatedMode
			prePlanMode = validatedPrePlan
		}
		if m.ext.TrustSessionPermissions {
			resumedRules = make([]permission.Rule, 0, len(hdr.AlwaysAllow))
			for _, rule := range hdr.AlwaysAllow {
				resumedRules = append(resumedRules, permission.Rule{
					Tool: rule.Tool, Match: rule.Match,
					Verb:   permission.Decision(rule.Verb),
					Source: permission.ResumedSessionSource(rule.Source),
				})
			}
		}
	}
	if err := rtpkg.PreflightRestoredPermissionState(m.ext.Sandbox, mode, prePlanMode); err != nil {
		restoreSourceRuntime()
		return fmt.Errorf("restore permission boundary for %s: %w", shortID(id), err)
	}

	// The destination preflight has passed. Flush/join source distillation and
	// save its Daily note before the source ID/history can be replaced. Auto
	// Memory owns immutable snapshots and is joined only at destructive close.
	if err := m.leaveActiveSession(activationBoundarySource(hdr, restorePermissions), false); err != nil {
		// switchModel applies a successfully-built target transport during
		// preflight. A later durability failure must still leave the source
		// completely usable, so restore that coherent runtime snapshot before
		// reporting the failed switch.
		restoreSourceRuntime()
		return fmt.Errorf("persist source session memory: %w", err)
	}
	// Ephemeral cron jobs belong to the session being left. Keeping them in
	// the shared service would make an old reminder fire into the destination
	// transcript after /new, /branch or /resume.
	if m.cronSvc != nil {
		m.cronSvc.ClearEphemeral()
	}
	m.gate.ResetSessionStateAndWait(mode, resumedRules, func() {
		m.loop.SetPrePlanMode(prePlanMode)
	}, func() {
		rtpkg.SynchronizeRestoredPermissionState(m.gate, m.loop, prePlanMode)
	})

	if hdr != nil && (hdr.System != "" || hdr.SystemPromptKind == session.SystemPromptKindDefault) {
		m.loop.System = destinationSystem
		if destinationSystem == m.baseSystem {
			m.loop.SystemSections = append([]llm.SystemSection(nil), m.baseSystemSections...)
		} else {
			// A persisted free-form system prompt has no typed-section
			// representation. Clear the source session's sections so providers
			// actually use this destination prompt.
			m.loop.SystemSections = nil
		}
	} else {
		// Empty is meaningful: an invocation may intentionally have no base
		// prompt. Always assign both fields so neither a free-form prompt nor
		// typed sections from the source session can leak into the destination.
		m.loop.System = m.baseSystem
		m.loop.SystemSections = append([]llm.SystemSection(nil), m.baseSystemSections...)
	}

	m.sessionID = id
	rtpkg.RebindLoopRuntime(m.loop, m.loop.Provider, m.model, m.loop.System, id)
	m.sessionTitle = ""
	if hdr != nil {
		m.sessionTitle = strings.TrimSpace(hdr.Title)
	}
	m.loop.ResetSession(messages)
	m.historyCursor.Mark(messages)
	m.loop.TimingSink = m.session.NewTimingRecorder(id).Record
	if m.ext.SessionSwitch != nil {
		m.ext.SessionSwitch(id)
	}

	// Clear every UI-side cache derived from the old transcript/session.
	m.messages = nil
	m.toolEvents = nil
	m.turnToolEventStart = 0
	m.subAgents = nil
	m.thinkingText = ""
	m.streamingText = ""
	m.queuedPrompts = nil
	m.totalTokens.Reset()
	m.histAll = nil
	m.histMatched = nil
	m.histFilter = ""
	m.histDirectIdx = -1
	m.histDirectDraft = ""
	if m.renderCache != nil {
		m.renderCache.InvalidateAll()
	}
	if cost, ok, _ := m.session.ReadCost(id); ok {
		m.totalTokens.Restore(cost.InputTokens, cost.OutputTokens, cost.CacheCreateTokens, cost.CacheReadTokens)
		if m.loop.Budget != nil {
			m.loop.Budget.AddUsage(cost.InputTokens, cost.OutputTokens, cost.CacheReadTokens, cost.CacheCreateTokens)
		}
	}
	if len(messages) > 0 {
		m.hydrateFromLoopHistory()
	}
	m.sessionBoundaryClosed = false
	return nil
}

func (m *Model) sessionActivationWarning(action string, err error) string {
	if err == nil {
		return ""
	}
	if m == nil || m.sessionID == "" {
		return action + " failed before changing session: " + err.Error()
	}
	return fmt.Sprintf("%s failed; session %s remains active: %v", action, shortID(m.sessionID), err)
}

func (m *Model) sessionWorkDirWarning(hdr *session.Header) string {
	if hdr == nil || hdr.WorkDir == "" {
		return ""
	}
	cwd, _ := os.Getwd()
	if cwd == "" || cwd == hdr.WorkDir {
		return ""
	}
	return fmt.Sprintf("session was created in %q; current working directory remains %q", hdr.WorkDir, cwd)
}
