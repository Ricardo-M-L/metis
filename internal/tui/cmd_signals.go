package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/version"
)

// cmd_signals.go holds the rendering logic for slash signals that just
// need to print live runtime data (tools, sessions, version, etc.).
// Both the bubbletea Model and the readline REPL share these helpers so
// the two surfaces stay visually consistent.
//
// Each helper returns a plain string. The caller decides how to surface it
// (info-row append in bubbletea, fmt.Fprintln in REPL).

// renderToolsList builds a human-readable listing of every registered tool
// on the live agent.Loop. Output is column-aligned for readability.
func renderToolsList(loop *agent.Loop) string {
	if loop == nil || loop.Registry == nil {
		return "(tools: registry not available)"
	}
	tools := loop.Registry.All()
	if len(tools) == 0 {
		return "(no tools registered)"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d tools registered:\n", len(tools)))
	maxName := 0
	for _, t := range tools {
		if l := len(t.Name()); l > maxName {
			maxName = l
		}
	}
	for _, t := range tools {
		fmt.Fprintf(&b, "  %-*s  %s\n", maxName, t.Name(), t.Description())
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderSessionsList lists recent sessions with title (when set) — title-first
// when present so the human label dominates. Falls back to id when no title.
func renderSessionsList(store *session.Store, limit int) string {
	if store == nil {
		return "(sessions: no store available)"
	}
	if limit <= 0 {
		limit = 20
	}
	entries, err := store.List(limit)
	if err != nil {
		return "sessions: " + err.Error()
	}
	if len(entries) == 0 {
		return "(no sessions yet)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "recent sessions (%d):\n", len(entries))
	for _, e := range entries {
		ts := e.CreatedAt.Local().Format("2006-01-02 15:04")
		if e.Title != "" {
			fmt.Fprintf(&b, "  %s  %s  %s — %s\n", ts, shortID(e.ID), e.Model, e.Title)
		} else {
			fmt.Fprintf(&b, "  %s  %s  %s\n", ts, shortID(e.ID), e.Model)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderCurrentSession describes the currently-active session: id, title,
// model, mode, message+turn count. Useful as a "where am I" panel.
func renderCurrentSession(store *session.Store, sessionID string, loop *agent.Loop, model string, mode string) string {
	if sessionID == "" {
		return "(no active session)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "id:    %s\n", sessionID)
	if store != nil {
		if hdr, _, err := store.LoadHeader(sessionID); err == nil && hdr != nil {
			if hdr.Title != "" {
				fmt.Fprintf(&b, "title: %s\n", hdr.Title)
			}
			if !hdr.CreatedAt.IsZero() {
				fmt.Fprintf(&b, "since: %s\n", hdr.CreatedAt.Local().Format(time.RFC3339))
			}
		}
	}
	if model != "" {
		fmt.Fprintf(&b, "model: %s\n", model)
	}
	if mode != "" {
		fmt.Fprintf(&b, "mode:  %s\n", mode)
	}
	if loop != nil {
		hist := loop.History()
		fmt.Fprintf(&b, "turns: %d  (messages: %d)\n",
			transcript.CountTurns(hist), len(hist))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderSkillsList aggregates every layer of the skill loader (bundled +
// user + project) and prints name + description. After the multi-source
// loader landed, "skills" is a union not just an on-disk store.
func renderSkillsList(skillDir string) string {
	loader := skills.NewLoader(skillDir, "", nil)
	list, err := loader.List()
	if err != nil {
		return "skills: " + err.Error()
	}
	if len(list) == 0 {
		return "(no skills available — bundled set should always be present; check the binary)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d skills available:\n", len(list))
	for _, sk := range list {
		fmt.Fprintf(&b, "  %-22s  %s\n", sk.Name, sk.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderVersion is one-line: matches `metis version` default form so muscle
// memory carries between the CLI and the chat surface.
func renderVersion() string {
	v := version.Version + " (Metis)"
	if version.Commit != "" && version.Commit != "unknown" {
		v += " · " + version.Commit
	}
	return v
}

// shortID truncates a session UUID to its first 8 chars for compact listing.
// Falls back to the original string for non-UUID-shaped ids.
func shortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}
