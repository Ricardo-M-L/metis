package tui

// footer_indicators.go: lightweight, cached side-effects to populate
// the status-bar's pill row — PR status (gh CLI), todo count
// (~/.metis/tasks/<sid>.json), and any future per-frame metadata
// that needs IO. Each indicator implements a 2-5s cache so the View
// refresh (every 50ms) doesn't fork/IO storm.
//
// Reference: claude-code's PrBadge.tsx + BackgroundTaskStatus.tsx
// follow the same pattern (poll every N seconds, render from cache).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/tasks"
	"time"
)

// ============================================================================
// PR badge
// ============================================================================

const prCacheTTL = 30 * time.Second

var (
	prMu       sync.Mutex
	prCached   string
	prCachedAt time.Time
	prRunning  bool
)

// prBadgeText returns the cached PR-badge string ("PR #1234 open")
// for the current branch, or "" if none / gh not installed / etc.
// First call kicks off a refresh in a goroutine; subsequent calls
// within prCacheTTL serve the cache. Designed for per-frame use.
func prBadgeText() string {
	prMu.Lock()
	cached := prCached
	stale := time.Since(prCachedAt) > prCacheTTL
	running := prRunning
	prMu.Unlock()

	if stale && !running {
		go refreshPrBadge()
	}
	return cached
}

func refreshPrBadge() {
	prMu.Lock()
	if prRunning {
		prMu.Unlock()
		return
	}
	prRunning = true
	prMu.Unlock()
	defer func() {
		prMu.Lock()
		prRunning = false
		prCachedAt = time.Now()
		prMu.Unlock()
	}()

	if _, err := exec.LookPath("gh"); err != nil {
		// No gh CLI installed — silently no-op. Don't repeatedly
		// look it up; cache an empty result.
		prMu.Lock()
		prCached = ""
		prMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// `gh pr status --json number,state,url` returns currentBranch.pulls.
	cmd := exec.CommandContext(ctx, "gh", "pr", "status", "--json", "number,state,url")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		prMu.Lock()
		prCached = ""
		prMu.Unlock()
		return
	}
	var resp struct {
		CurrentBranch struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			URL    string `json:"url"`
		} `json:"currentBranch"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return
	}
	if resp.CurrentBranch.Number == 0 {
		prMu.Lock()
		prCached = ""
		prMu.Unlock()
		return
	}
	state := strings.ToLower(resp.CurrentBranch.State)
	label := fmt.Sprintf("PR #%d %s", resp.CurrentBranch.Number, state)
	if resp.CurrentBranch.URL != "" {
		label = osc8Link(label, resp.CurrentBranch.URL)
	}
	prMu.Lock()
	prCached = label
	prMu.Unlock()
}

// ============================================================================
// Background task panel — count of in-flight todos
// ============================================================================

const tasksCacheTTL = 2 * time.Second

var (
	tasksMu       sync.Mutex
	tasksCount    int
	tasksCheckSid string
	tasksAt       time.Time
)

// TaskItem is the wire shape we read from ~/.metis/tasks/<sid>.json.
// Mirrors the TodoWrite tool's persistence schema.
type TaskItem struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Content string `json:"content"`
}

// tasksCurrentSessionIDImpl returns the runtime-set current session id.
// Lives next to tasksFullList so the render-tool snapshot path can
// resolve a sid without each renderer having to thread Model state.
func tasksCurrentSessionIDImpl() string {
	return tasksRuntimeSessionID()
}

// tasksRuntimeSessionID forwards to the runtime-set session id. The
// internal/tasks package holds the canonical value (set by
// setupRuntime via SetCurrentSessionID); this wrapper keeps the import
// graph going one direction (tui → tasks, not the reverse).
var tasksRuntimeSessionID = func() string { return tasks.CurrentSessionID() }

// tasksFullList reads ~/.metis/tasks/<sid>.json and returns every
// todo (any status). Used by the rich task-panel UI; for the cheap
// status-bar count, tasksRunningCount is faster (cached).
func tasksFullList(sessionID string) []TaskItem {
	if sessionID == "" {
		return nil
	}
	home := os.Getenv("METIS_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = filepath.Join(h, ".metis")
	}
	path := filepath.Join(home, "tasks", sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var items []TaskItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil
	}
	return items
}

// tasksRunningCount returns how many todos in this session are
// currently in_progress or pending. Reads ~/.metis/tasks/<sid>.json
// (the same file metis's TodoWrite tool persists). Cached 2s so
// per-frame calls are cheap; refreshes inline (no goroutine — the
// JSON is tiny).
func tasksRunningCount(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	tasksMu.Lock()
	defer tasksMu.Unlock()
	if tasksCheckSid == sessionID && time.Since(tasksAt) < tasksCacheTTL {
		return tasksCount
	}
	tasksCheckSid = sessionID
	tasksAt = time.Now()

	home := os.Getenv("METIS_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			tasksCount = 0
			return 0
		}
		home = filepath.Join(h, ".metis")
	}
	path := filepath.Join(home, "tasks", sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		tasksCount = 0
		return 0
	}
	var todos []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &todos); err != nil {
		tasksCount = 0
		return 0
	}
	n := 0
	for _, t := range todos {
		if t.Status == "in_progress" || t.Status == "pending" {
			n++
		}
	}
	tasksCount = n
	return n
}
