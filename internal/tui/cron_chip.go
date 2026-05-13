package tui

// cron_chip.go — status-bar chips for cron-adjacent state the user
// would otherwise have to leave the chat to check:
//
//   wakeupChip  — "next wake: 18m" when an LLM-scheduled wakeup is
//                 pending (kind=at + name prefix "wakeup:"). Mirrors
//                 the way claude-code surfaces a pending ScheduleWakeup
//                 in its REPL chrome.
//
//   silentFiresChip — "[cron silent 3 today]" when one or more silent
//                 cron jobs fired in the last 24h. Without this the
//                 user has no idea their /every-5min health-check
//                 has been running. Hermes pattern via SILENT_MARKER:
//                 visible badge, invisible payload.
//
// Both read ~/.metis/cron/ directly with a 5s cache so per-frame
// status-bar repaints don't IO storm. Cache TTL matches the granularity
// the user cares about (next-wake-in-minutes doesn't need sub-second
// freshness).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const cronChipCacheTTL = 5 * time.Second

var (
	cronChipMu              sync.Mutex
	cronChipCheckedAt       time.Time
	cronChipWakeup          string // "next wake: 18m" or ""
	cronChipSilentFires24h  int    // count of audit-log files in last 24h
)

// wakeupChip returns the formatted pending-wakeup chip text, or ""
// when no wakeup is scheduled. Inspects ~/.metis/cron/*.json once
// per cronChipCacheTTL and renders the soonest-upcoming wakeup.
func wakeupChip() string {
	refreshCronChipCache()
	cronChipMu.Lock()
	defer cronChipMu.Unlock()
	return cronChipWakeup
}

// silentFiresChip returns "[cron silent N today]" when N>0 silent
// cron fires landed in the last 24 hours, or "" otherwise. Helps the
// user notice a misbehaving silent job (e.g. expected 1 fire/hr but
// counter shows 0 — something broke).
func silentFiresChip() string {
	refreshCronChipCache()
	cronChipMu.Lock()
	defer cronChipMu.Unlock()
	if cronChipSilentFires24h == 0 {
		return ""
	}
	return fmt.Sprintf("◐ cron silent %d/24h", cronChipSilentFires24h)
}

// refreshCronChipCache rebuilds wakeup + silent-fires counters when
// the cache is stale. Inline (no goroutine) — the work is one ReadDir
// of ~/.metis/cron + one of ~/.metis/cron/audit/<id>/, each typically
// ≤ 20 files. Costs <1ms; cheaper than the goroutine round-trip.
func refreshCronChipCache() {
	cronChipMu.Lock()
	stale := time.Since(cronChipCheckedAt) > cronChipCacheTTL
	cronChipMu.Unlock()
	if !stale {
		return
	}
	cronDir := metisCronDir()
	if cronDir == "" {
		return
	}
	wakeup := scanNextWakeup(cronDir)
	silent := scanSilentFires24h(cronDir)
	cronChipMu.Lock()
	cronChipWakeup = wakeup
	cronChipSilentFires24h = silent
	cronChipCheckedAt = time.Now()
	cronChipMu.Unlock()
}

// metisCronDir resolves ~/.metis/cron (honoring METIS_HOME) without
// failing the chip render — caller treats empty as "no cron state".
func metisCronDir() string {
	home := os.Getenv("METIS_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".metis")
	}
	return filepath.Join(home, "cron")
}

// cronJobOnDisk mirrors agent.CronJob enough to read NextRun + Name +
// Enabled + Paused for the chip. Defined locally so the tui package
// doesn't import internal/agent (which would create an import cycle —
// agent's loop pulls in tui-adjacent types).
type cronJobOnDisk struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Paused   bool   `json:"paused"`
	Silent   bool   `json:"silent,omitempty"`
	NextRun  time.Time `json:"next_run,omitempty"`
}

// scanNextWakeup walks every cron job and returns "next wake: <dur>"
// for the soonest active wakeup. We identify wakeups by the
// `wakeup:` name prefix that ScheduleWakeup stamps; this avoids
// reading every job's full Schedule.
func scanNextWakeup(cronDir string) string {
	ents, err := os.ReadDir(cronDir)
	if err != nil {
		return ""
	}
	now := time.Now()
	var soonest time.Time
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(cronDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var j cronJobOnDisk
		if err := json.Unmarshal(data, &j); err != nil {
			continue
		}
		if !j.Enabled || j.Paused {
			continue
		}
		if !strings.HasPrefix(j.Name, "wakeup:") {
			continue
		}
		if j.NextRun.IsZero() || j.NextRun.Before(now) {
			continue
		}
		if soonest.IsZero() || j.NextRun.Before(soonest) {
			soonest = j.NextRun
		}
	}
	if soonest.IsZero() {
		return ""
	}
	d := soonest.Sub(now)
	return "↻ wake " + compactDuration(d)
}

// scanSilentFires24h counts every audit-log file under
// <cron>/audit/<id>/<rfc3339>.jsonl whose mtime is within 24h.
// Returns 0 on missing dir (typical case).
func scanSilentFires24h(cronDir string) int {
	auditRoot := filepath.Join(cronDir, "audit")
	jobDirs, err := os.ReadDir(auditRoot)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	total := 0
	for _, jd := range jobDirs {
		if !jd.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(auditRoot, jd.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(cutoff) {
				total++
			}
		}
	}
	return total
}

// compactDuration formats a duration for the status bar: "18m", "2h",
// "1d". Sub-minute rounds to "<1m" to keep width predictable.
func compactDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
