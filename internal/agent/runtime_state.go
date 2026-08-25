package agent

import (
	"fmt"
	"strings"
)

// RuntimeStateSnapshot is the comparable, provider-neutral view of mutable
// execution state that the model must see on every request. Keep fields as
// values (rather than a pre-rendered blob) so Loop can distinguish a semantic
// state change from a callback that merely rebuilt the same data.
type RuntimeStateSnapshot struct {
	PermissionMode   string
	WorkingDirectory string
	SessionID        string
	CurrentPlan      string
	Provider         string
	Model            string
	PlanMode         bool
}

// Render returns a byte-stable representation. Field order is deliberately
// fixed: prompt caches key on exact prefixes, so map/JSON rendering would make
// an unchanged snapshot vulnerable to ordering drift.
func (s RuntimeStateSnapshot) Render() string {
	var body strings.Builder
	body.WriteString("<runtime_state>\n")
	writeRuntimeStateField(&body, "permission_mode", s.PermissionMode)
	writeRuntimeStateField(&body, "working_directory", s.WorkingDirectory)
	writeRuntimeStateField(&body, "session_id", s.SessionID)
	writeRuntimeStateField(&body, "provider", s.Provider)
	writeRuntimeStateField(&body, "model", s.Model)
	fmt.Fprintf(&body, "plan_mode: %t\n", s.PlanMode)
	if plan := strings.TrimSpace(s.CurrentPlan); plan != "" {
		body.WriteString("<current_plan>\n")
		body.WriteString(plan)
		body.WriteString("\n</current_plan>\n")
	}
	body.WriteString("</runtime_state>")
	return body.String()
}

func writeRuntimeStateField(body *strings.Builder, name, value string) {
	if value != "" {
		fmt.Fprintf(body, "%s: %s\n", name, value)
	}
}

// currentRuntimeStateSectionLocked snapshots the live callback, fills fields
// owned directly by Loop, and only re-renders when the comparable value
// changes. Caller must hold l.mu for writing because the cached baseline and
// revision are session state.
func (l *Loop) currentRuntimeStateSectionLocked() (llmSection runtimeStateSection, ok bool) {
	if l.CurrentStateSnapshot == nil {
		return runtimeStateSection{}, false
	}
	next := l.CurrentStateSnapshot()
	next.PlanMode = l.planMode
	if next.Model == "" {
		next.Model = l.Model
	}
	if l.Provider != nil {
		if next.Provider == "" {
			next.Provider = l.Provider.Name()
		}
		if next.Model == "" {
			next.Model = l.Provider.ModelID()
		}
	}
	if !l.runtimeStateReady || next != l.runtimeStateSnapshot {
		l.runtimeStateSnapshot = next
		l.runtimeStateBody = next.Render()
		l.runtimeStateReady = true
		l.runtimeStateRevision++
	}
	return runtimeStateSection{body: l.runtimeStateBody}, l.runtimeStateBody != ""
}

// runtimeStateSection is intentionally private and wire-neutral; loop.go turns
// it into llm.SystemSection at the request boundary.
type runtimeStateSection struct {
	body string
}

func (l *Loop) invalidateRuntimeStateLocked() {
	l.runtimeStateReady = false
}

// InvalidateRuntimeState forces the next request (or fork snapshot) to rebuild
// a complete runtime-state section. Session restore and compaction call this
// because their history prefix changed even if the semantic fields did not.
func (l *Loop) InvalidateRuntimeState() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.invalidateRuntimeStateLocked()
	l.mu.Unlock()
}
