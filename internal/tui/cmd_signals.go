package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
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
	rows := make([]infoRow, 0, len(tools))
	for _, t := range tools {
		rows = append(rows, infoRow{Key: t.Name(), Value: t.Description()})
	}
	return renderInfoBox(fmt.Sprintf("Tools · %d registered", len(tools)), rows)
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
	cwd, _ := os.Getwd()
	entries, err := store.ListResumable(session.ResumeListOptions{Limit: limit, WorkDir: cwd})
	if err != nil {
		return "sessions: " + err.Error()
	}
	if len(entries) == 0 {
		return renderInfoBox("Sessions", []infoRow{{Key: "", Value: "no sessions yet"}})
	}
	rows := make([]infoRow, 0, len(entries))
	for _, e := range entries {
		ts := sessionEntryTime(e).Local().Format("2006-01-02 15:04")
		label := e.Title
		if label == "" {
			label = e.Model
		}
		rows = append(rows, infoRow{
			Key:   shortID(e.ID),
			Value: label,
			Hint:  ts,
		})
	}
	return renderInfoBox(fmt.Sprintf("Recent Sessions · %d", len(entries)), rows)
}

// renderCurrentSession describes the currently-active session using live loop
// state rather than only the persisted header. It is the shared implementation
// behind /status in both the Bubble Tea and readline surfaces.
func renderCurrentSession(store *session.Store, sessionID string, loop *agent.Loop, model string, mode string) string {
	if sessionID == "" {
		return "(no active session)"
	}
	rows := []infoRow{{Key: "id", Value: sessionID}}
	workDir := ""
	if store != nil {
		if hdr, _, err := store.LoadHeader(sessionID); err == nil && hdr != nil {
			if hdr.Title != "" {
				rows = append(rows, infoRow{Key: "title", Value: hdr.Title})
			}
			if !hdr.CreatedAt.IsZero() {
				rows = append(rows, infoRow{Key: "since", Value: hdr.CreatedAt.Local().Format(time.RFC3339)})
			}
			workDir = hdr.WorkDir
			if model == "" {
				model = hdr.Model
			}
			if mode == "" {
				mode = hdr.Mode
			}
		}
		rows = append(rows, infoRow{
			Key:   "transcript",
			Value: filepath.Join(store.Dir, filepath.Base(sessionID)+".jsonl"),
		})
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	if workDir != "" {
		rows = append(rows, infoRow{Key: "working dir", Value: workDir})
	}
	if model != "" {
		rows = append(rows, infoRow{Key: "model", Value: model})
	}
	if mode != "" {
		rows = append(rows, infoRow{Key: "mode", Value: mode})
	}
	if loop != nil {
		hist := loop.History()
		if loop.Provider != nil {
			rows = append(rows, infoRow{Key: "provider", Value: loop.Provider.Name()})
		}
		effort := string(loop.EffortValue())
		if effort == "" {
			effort = "provider default"
		}
		rows = append(rows, infoRow{Key: "effort", Value: effort})
		rows = append(rows, infoRow{Key: "quick output", Value: fmt.Sprintf("%t", loop.FastEnabled())})
		rows = append(rows, infoRow{Key: "plan mode", Value: fmt.Sprintf("%t", loop.IsPlanMode())})

		used := loop.EstimateContextTokens()
		limit := loop.ContextWindow
		if limit <= 0 && loop.Provider != nil {
			limit = loop.Provider.MaxContextTokens()
		}
		contextValue := fmt.Sprintf("~%s tokens", fmtThousands(used))
		contextHint := "window unknown"
		if limit > 0 {
			contextValue = fmt.Sprintf("~%s / %s tokens", fmtThousands(used), fmtThousands(limit))
			contextHint = fmt.Sprintf("%.1f%% used", float64(used)*100/float64(limit))
		}
		rows = append(rows, infoRow{Key: "context", Value: contextValue, Hint: contextHint})

		iterCap := "unlimited"
		if loop.MaxIters > 0 {
			iterCap = fmt.Sprintf("%d per turn", loop.MaxIters)
		}
		rows = append(rows, infoRow{Key: "loop iterations", Value: fmt.Sprintf("%d", loop.IterIdx()), Hint: "cap " + iterCap})
		rows = append(rows, infoRow{
			Key:   "turns",
			Value: fmt.Sprintf("%d", transcript.CountTurns(hist)),
			Hint:  fmt.Sprintf("%d messages", len(hist)),
		})
	}
	return renderInfoBox("Session", rows)
}

// renderSkillsList aggregates every layer of the skill loader (bundled +
// user + project) and prints name + description. After the multi-source
// loader landed, "skills" is a union not just an on-disk store.
func renderSkillsList(loop *agent.Loop, skillDir string) string {
	list, err := loadSkillCatalog(loop, skillDir)
	if err != nil {
		return "skills: " + err.Error()
	}
	if len(list) == 0 {
		return "(no skills available — bundled set should always be present; check the binary)"
	}
	rows := make([]infoRow, 0, len(list))
	for _, sk := range list {
		rows = append(rows, infoRow{Key: sk.Name, Value: sk.Description})
	}
	return renderInfoBox(fmt.Sprintf("Skills · %d available", len(list)), rows)
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
