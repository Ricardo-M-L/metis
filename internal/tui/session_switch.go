package tui

// session_switch.go centralizes every in-process top-level session boundary.
// A session is more than its transcript: permissions, tool sidecars, global
// Todo/Task routing, prompt dumps, title, cost and prompt-history caches must
// all move together or the next turn can read/write the previous session.

import (
	"fmt"
	"os"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

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
	return persistSessionState(m.session, m.sessionID, m.gate, m.providerName, m.model, m.loop.System)
}

func persistPermissionState(store *session.Store, sessionID string, gate *permission.Gate) error {
	return persistSessionState(store, sessionID, gate, "", "", "")
}

func persistSessionState(store *session.Store, sessionID string, gate *permission.Gate, providerName, model, system string) error {
	if store == nil || gate == nil || sessionID == "" {
		return nil
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
		Mode:             string(gate.Mode()),
		AlwaysAllow:      rules,
		ClearAlwaysAllow: len(rules) == 0,
	}); err != nil {
		return fmt.Errorf("persist session %s state: %w", sessionID, err)
	}
	return nil
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
	r.Gate.ResetSessionState(r.freshPermissionMode(), nil)
	r.Loop.SetPrePlanMode("")
	r.Loop.TimingSink = r.Session.NewTimingRecorder(id).Record
	r.totalTokens.Reset()
	if r.SessionSwitch != nil {
		r.SessionSwitch(id)
	}
}

func (r *REPL) startFreshSession() (string, error) {
	if r == nil || r.Session == nil || r.Loop == nil || r.Gate == nil {
		return "", fmt.Errorf("session runtime unavailable")
	}
	if err := persistSessionState(r.Session, r.SessionID, r.Gate, r.providerName, r.model, r.Loop.System); err != nil {
		return "", err
	}
	id := r.Session.NewSessionID()
	system := r.baseSystem
	cwd, _ := os.Getwd()
	if err := r.Session.WriteHeaderFull(session.Header{
		ID: id, Provider: r.providerName, Model: r.model, System: system, WorkDir: cwd,
		Mode: string(r.freshPermissionMode()),
	}); err != nil {
		return "", err
	}
	if r.SessionBoundary != nil {
		r.SessionBoundary()
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
	if err := persistSessionState(r.Session, r.SessionID, r.Gate, r.providerName, r.model, r.Loop.System); err != nil {
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
		return "", err
	}
	if r.SessionBoundary != nil {
		r.SessionBoundary()
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
		ID: id, Provider: m.providerName, Model: m.model, System: system, WorkDir: cwd,
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
		return "", nil, err
	}
	hdr, _, err := m.session.LoadHeader(id)
	if err != nil {
		return "", nil, err
	}
	cleanedSystem := rtpkg.RemoveLegacyPlanOverlay(hdr.System)
	if cleanedSystem != hdr.System {
		// The fork's initial header was copied from the selected parent. Append a
		// corrected header immediately so the new branch is clean even if it is
		// never activated in this process.
		if cleanedSystem != "" {
			if err := m.session.WriteHeaderFull(session.Header{ID: id, System: cleanedSystem}); err != nil {
				return "", nil, fmt.Errorf("clean legacy plan overlay from fork %s: %w", id, err)
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
		destinationSystem = rtpkg.RemoveLegacyPlanOverlay(hdr.System)
		if destinationSystem != hdr.System && destinationSystem != "" {
			if err := m.session.WriteHeaderFull(session.Header{ID: id, System: destinationSystem}); err != nil {
				return fmt.Errorf("clean legacy plan overlay from session %s: %w", shortID(id), err)
			}
		}
	}

	mode := m.freshPermissionMode()
	var resumedRules []permission.Rule
	if restorePermissions && hdr != nil {
		if hdr.Mode != "" {
			mode = permission.Mode(hdr.Mode)
		}
		resumedRules = make([]permission.Rule, 0, len(hdr.AlwaysAllow))
		for _, rule := range hdr.AlwaysAllow {
			resumedRules = append(resumedRules, permission.Rule{
				Tool: rule.Tool, Match: rule.Match,
				Verb:   permission.Decision(rule.Verb),
				Source: permission.ResumedSessionSource(rule.Source),
			})
		}
	}

	// The fallible preflight has passed. Commit the remaining session-scoped
	// state as one non-failing boundary.
	if m.ext.SessionBoundary != nil {
		m.ext.SessionBoundary()
	}
	// Ephemeral cron jobs belong to the session being left. Keeping them in
	// the shared service would make an old reminder fire into the destination
	// transcript after /new, /branch or /resume.
	if m.cronSvc != nil {
		m.cronSvc.ClearEphemeral()
	}
	m.gate.ResetSessionState(mode, resumedRules)
	m.loop.SetPrePlanMode("")

	if hdr != nil && hdr.System != "" {
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
