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
	"os/exec"
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

// TaskItem is the UI projection shared by TodoWrite and structured Task*.
type TaskItem struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Content string `json:"content"`
	Owner   string `json:"owner,omitempty"`
}

func taskItemLabel(item TaskItem) string {
	if owner := strings.TrimSpace(item.Owner); owner != "" {
		return item.Content + "  · @" + owner
	}
	return item.Content
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

// tasksFullList reads the canonical projection shared by TodoWrite and Task*
// so a coordinated team's structured tasks appear in the same CLI panel.
func tasksFullList(sessionID string) []TaskItem {
	if sessionID == "" {
		return nil
	}
	items, err := tasks.PlanningItems(sessionID)
	if err != nil || len(items) == 0 {
		return nil
	}
	out := make([]TaskItem, 0, len(items))
	for _, item := range items {
		out = append(out, TaskItem{ID: item.ID, Status: item.Status, Content: item.Content, Owner: item.Owner})
	}
	return out
}

// currentInProgressTodoContent returns the Content of the first
// in_progress todo for this session, or "" when no in-progress todo
// exists. Mirrors claude-code Spinner.tsx:169 — `currentTodo.activeForm
// ?? currentTodo.subject ?? randomVerb` — so the spinner reads
// "Implementing OAuth refresh…" instead of a static "exploring…" once
// the model has set a task in_progress. The canonical task projection is
// intentionally small; the status-bar count has its own short cache.
func currentInProgressTodoContent(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	for _, t := range tasksFullList(sessionID) {
		if t.Status == "in_progress" {
			return t.Content
		}
	}
	return ""
}

// chooseSpinnerVerb is the metis equivalent of claude-code's
// `currentTodo?.activeForm ?? randomVerb` fallback chain. When a
// TodoWrite todo is in_progress, return its content; otherwise return
// a fresh random gerund from pickSpinnerVerb.
func chooseSpinnerVerb(sessionID string) string {
	if v := currentInProgressTodoContent(sessionID); v != "" {
		return v
	}
	return pickSpinnerVerb()
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

	items, err := tasks.PlanningItems(sessionID)
	if err != nil {
		tasksCount = 0
		return 0
	}
	n := 0
	for _, t := range items {
		if t.Status == "in_progress" || t.Status == "pending" {
			n++
		}
	}
	tasksCount = n
	return n
}
