package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
)

// PlansDir returns the directory where archived plans live.
//
// Each plan is one JSON file under ~/.metis/plans/. We don't bundle
// them into a single JSONL like history because plans are
// human-readable artefacts users may want to grep or git-add
// individually.
func PlansDir() string {
	return filepath.Join(config.Home(), "plans")
}

// ArchivedPlan is the on-disk shape of a captured plan-mode run.
type ArchivedPlan struct {
	SessionID  string           `json:"session"`
	Timestamp  time.Time        `json:"ts"`
	UserPrompt string           `json:"user_prompt,omitempty"`
	ToolCalls  []agent.ToolCall `json:"tool_calls"`
}

// plansMu prevents concurrent writes to the plans dir from racing on
// the directory entry table — overkill on most filesystems but cheap
// insurance.
var plansMu sync.Mutex

// ArchivePlan persists a plan-mode result to ~/.metis/plans/. Filename
// pattern: `<session>_<unix-millis>.json` so listings sort
// chronologically and never collide across sessions.
//
// Errors are returned but callers (TUI / REPL handle EventPlan paths)
// fire-and-forget — disk hiccups must not break the chat surface.
func ArchivePlan(plan ArchivedPlan) error {
	if len(plan.ToolCalls) == 0 {
		// An empty plan is meaningless; don't create a file just to
		// document "the LLM proposed nothing."
		return nil
	}
	if plan.Timestamp.IsZero() {
		plan.Timestamp = time.Now()
	}
	dir := PlansDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("plans: mkdir %s: %w", dir, err)
	}
	plansMu.Lock()
	defer plansMu.Unlock()

	sessionTag := plan.SessionID
	if sessionTag == "" {
		sessionTag = "unknown"
	}
	name := fmt.Sprintf("%s_%d.json", sessionTag, plan.Timestamp.UnixMilli())
	path := filepath.Join(dir, name)
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("plans: marshal: %w", err)
	}
	return os.WriteFile(path, b, 0o644)
}

// ListPlans returns the most recent N archived plans, newest first.
// Reads the metadata only — caller can re-load full content via
// ReadPlan when needed. Used by future `metis plans list` / `/plans`
// commands.
func ListPlans(limit int) ([]ArchivedPlan, error) {
	if limit <= 0 {
		limit = 20
	}
	dir := PlansDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var plans []ArchivedPlan
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var p ArchivedPlan
		if json.Unmarshal(data, &p) != nil {
			continue
		}
		plans = append(plans, p)
	}
	// Sort newest first by Timestamp; ReadDir order isn't reliable.
	for i := 1; i < len(plans); i++ {
		for j := i; j > 0 && plans[j-1].Timestamp.Before(plans[j].Timestamp); j-- {
			plans[j-1], plans[j] = plans[j], plans[j-1]
		}
	}
	if len(plans) > limit {
		plans = plans[:limit]
	}
	return plans, nil
}
