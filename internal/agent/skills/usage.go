package skills

// usage.go — per-skill usage/lifecycle record. The keystone for skill
// self-evolution: provenance, a multi-event activity signal, and lifecycle
// states all derive from one store, mirroring hermes-agent's
// tools/skill_usage.py.
//
// Why this exists (and replaces the curator's old file-mtime signal):
//   - Provenance (#5): a record with Source=="agent-synth" authoritatively
//     marks a skill as agent-created. The curator no longer has to guess
//     "agent-created == flat .md file".
//   - Multi-event signal (#4): use / view / patch are tracked separately
//     (count + timestamp) instead of collapsing everything into one mtime,
//     so a frequently-VIEWED-but-rarely-invoked skill isn't mistaken for
//     idle, and an invoke doesn't have to mutate the file on disk (the old
//     os.Chtimes-on-read coupling is gone).
//   - Lifecycle states (#3): active / idle / archived are derived from the
//     latest activity timestamp via DeriveState.
//
// Storage: one JSON object {skillName: record} at
// <skillsDir>/.curator-usage.json. The dotfile name keeps it invisible to
// the loader (dirLayer skips dot-prefixed entries). Writes are mutex- +
// whole-file atomic (read-modify-write under the lock); concurrent metis
// processes can still race, but a lost increment is harmless (it only
// nudges a lifecycle clock, never corrupts).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const usageFile = ".curator-usage.json"

// SkillState is a lifecycle state derived from activity recency.
type SkillState string

const (
	StateActive   SkillState = "active"   // used/viewed/patched recently
	StateIdle     SkillState = "idle"     // quiet past the idle window, not yet old enough to retire
	StateArchived SkillState = "archived" // quiet past the archive window — a Sweep will retire it
)

// UsageRecord is the tracked history of one agent-created skill.
type UsageRecord struct {
	CreatedAt     string `json:"created_at"`               // RFC3339, set on synth create
	Source        string `json:"source,omitempty"`         // "agent-synth" = provenance marker
	UseCount      int    `json:"use_count"`                // Skill(invoke) count
	ViewCount     int    `json:"view_count"`               // Skill(get) count
	PatchCount    int    `json:"patch_count"`              // synth UpdateSkill count
	LastUsedAt    string `json:"last_used_at,omitempty"`   // RFC3339
	LastViewedAt  string `json:"last_viewed_at,omitempty"` // RFC3339
	LastPatchedAt string `json:"last_patched_at,omitempty"`
}

// SourceAgentSynth marks a record as authored by the dreaming SkillSynth.
const SourceAgentSynth = "agent-synth"

// LatestActivity returns the newest of last-used / last-viewed /
// last-patched, falling back to created_at so a brand-new never-touched
// skill still has a sane lifecycle anchor. ok is false only when the
// record carries no parseable timestamp at all.
func (r UsageRecord) LatestActivity() (t time.Time, ok bool) {
	for _, ts := range []string{r.LastUsedAt, r.LastViewedAt, r.LastPatchedAt, r.CreatedAt} {
		if ts == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			if parsed.After(t) {
				t = parsed
				ok = true
			}
		}
	}
	return t, ok
}

// ActivityCount is the total observed use + view + patch events.
func (r UsageRecord) ActivityCount() int {
	return r.UseCount + r.ViewCount + r.PatchCount
}

// DeriveState maps activity recency to a lifecycle state. idleAfter and
// archiveAfter are durations from the latest activity.
func DeriveState(r UsageRecord, now time.Time, idleAfter, archiveAfter time.Duration) SkillState {
	act, ok := r.LatestActivity()
	if !ok {
		return StateActive // no timestamps yet — treat as freshly active
	}
	return stateForAge(now.Sub(act), idleAfter, archiveAfter)
}

// stateForAge maps an inactivity duration to a lifecycle state. The single
// source of truth for the idle/archive boundaries, shared by DeriveState
// and the curator's LifecycleStates so the two can't drift.
func stateForAge(age, idleAfter, archiveAfter time.Duration) SkillState {
	switch {
	case age >= archiveAfter:
		return StateArchived
	case age >= idleAfter:
		return StateIdle
	default:
		return StateActive
	}
}

// UsageStore is the on-disk usage map for one skills root.
type UsageStore struct {
	mu   sync.Mutex
	path string
}

// NewUsageStore binds a store to <skillsDir>/.curator-usage.json. A nil
// return is never produced; an empty skillsDir yields a no-op store
// (every method is a safe noop / empty result) so callers needn't guard.
func NewUsageStore(skillsDir string) *UsageStore {
	if skillsDir == "" {
		return &UsageStore{}
	}
	return &UsageStore{path: filepath.Join(skillsDir, usageFile)}
}

// RecordCreate stamps a new skill's provenance + creation time. Idempotent
// on re-create (refreshes CreatedAt only if previously empty).
func (u *UsageStore) RecordCreate(name, source string) {
	u.mutate(name, func(r *UsageRecord) {
		if r.CreatedAt == "" {
			r.CreatedAt = nowRFC()
		}
		if source != "" {
			r.Source = source
		}
	})
}

// RecordUse bumps the invoke counter + last-used clock.
func (u *UsageStore) RecordUse(name string) {
	u.mutate(name, func(r *UsageRecord) { r.UseCount++; r.LastUsedAt = nowRFC() })
}

// RecordView bumps the get/view counter + last-viewed clock.
func (u *UsageStore) RecordView(name string) {
	u.mutate(name, func(r *UsageRecord) { r.ViewCount++; r.LastViewedAt = nowRFC() })
}

// RecordPatch bumps the patch counter + last-patched clock.
func (u *UsageStore) RecordPatch(name string) {
	u.mutate(name, func(r *UsageRecord) { r.PatchCount++; r.LastPatchedAt = nowRFC() })
}

// Get returns the record for name (ok=false when none exists).
func (u *UsageStore) Get(name string) (UsageRecord, bool) {
	if u.path == "" {
		return UsageRecord{}, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	m := u.loadLocked()
	r, ok := m[name]
	return r, ok
}

// IsAgentCreated reports whether name has an agent-synth provenance record.
// Authoritative provenance (vs the curator's flat-file shape heuristic).
func (u *UsageStore) IsAgentCreated(name string) bool {
	r, ok := u.Get(name)
	return ok && r.Source == SourceAgentSynth
}

// Forget drops a skill's record — called when a skill is permanently gone
// (uninstall). Archiving does NOT forget; a restored skill keeps its history.
func (u *UsageStore) Forget(name string) {
	if u.path == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	m := u.loadLocked()
	if _, ok := m[name]; !ok {
		return
	}
	delete(m, name)
	_ = u.saveLocked(m)
}

func (u *UsageStore) mutate(name string, fn func(*UsageRecord)) {
	if u.path == "" || name == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	m := u.loadLocked()
	r := m[name]
	fn(&r)
	m[name] = r
	_ = u.saveLocked(m)
}

func (u *UsageStore) loadLocked() map[string]UsageRecord {
	m := map[string]UsageRecord{}
	data, err := os.ReadFile(u.path)
	if err != nil {
		return m
	}
	// A corrupt usage file is non-fatal: usage data is advisory (it only
	// tunes lifecycle clocks), so start fresh rather than erroring. Unlike
	// pins (a safety gate), losing usage history never archives a skill the
	// user wanted kept — the flat-file mtime fallback still protects it.
	_ = json.Unmarshal(data, &m)
	if m == nil {
		m = map[string]UsageRecord{}
	}
	return m
}

func (u *UsageStore) saveLocked(m map[string]UsageRecord) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(u.path, data)
}

// atomicWriteFile writes data to path via a same-dir temp file + rename, so
// a concurrent reader (the curator, another metis process) never observes a
// half-written file. Shared by the package's two JSON state stores
// (.curator-usage.json and .curator-pins.json) — a plain WriteFile
// truncates-in-place, and a torn read of either resets it (usage history
// lost, or worse, every pin dropped right before a Sweep). The temp file is
// always cleaned unless the rename consumed it, so a persistently failing
// rename can't orphan a dotfile per call.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }
