// Package tasks persists per-session todo lists under ~/.metis/tasks/.
//
// Built-in tools (TodoWrite / TodoRead) and runtime helpers both read
// from this package — splitting it out from internal/runtime breaks an
// otherwise-circular dependency (runtime → tools → builtin → runtime
// would cycle if Todo persistence lived in runtime).
package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// home resolves the metis home dir without importing config (to keep
// internal/tasks a leaf package). Mirror of internal/auth's home() —
// the rule is short and stable enough that copying is fine.
func home() string {
	if h := os.Getenv("METIS_HOME"); h != "" {
		return h
	}
	hd, _ := os.UserHomeDir()
	return filepath.Join(hd, ".metis")
}

// Dir returns the tasks directory.
func Dir() string {
	return filepath.Join(home(), "tasks")
}

// Item is one row in a session's task list.
type Item struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Status    string    `json:"status"`   // pending | in_progress | completed
	Priority  string    `json:"priority"` // low | medium | high
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// List is the on-disk task list for one session.
type List struct {
	SessionID string `json:"session"`
	Items     []Item `json:"items"`
}

var mu sync.Mutex

func path(sessionID string) string {
	if sessionID == "" {
		sessionID = "default"
	}
	return filepath.Join(Dir(), sessionID+".json")
}

// Load returns the list for sessionID. Missing file → empty list, not
// error (the LLM's first TodoWrite call is the implicit creation).
func Load(sessionID string) (*List, error) {
	tl := &List{SessionID: sessionID}
	b, err := os.ReadFile(path(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return tl, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, tl); err != nil {
		return nil, fmt.Errorf("tasks: parse %s: %w", path(sessionID), err)
	}
	if tl.SessionID == "" {
		tl.SessionID = sessionID
	}
	return tl, nil
}

// Save rewrites the task file. 0o644 — task content is already shown
// inline so it's not secret.
func Save(tl *List) error {
	if tl == nil {
		return fmt.Errorf("tasks: nil list")
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tl, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path(tl.SessionID), b, 0o644)
}

// Upsert creates or updates one item. Match priority: explicit ID >
// content string > append new. Status / Priority preserved when caller
// leaves them empty.
func Upsert(sessionID string, in Item) (Item, error) {
	mu.Lock()
	defer mu.Unlock()
	tl, err := Load(sessionID)
	if err != nil {
		return Item{}, err
	}
	now := time.Now()

	idx := -1
	if in.ID != "" {
		for i := range tl.Items {
			if tl.Items[i].ID == in.ID {
				idx = i
				break
			}
		}
	}
	if idx < 0 && in.Content != "" {
		for i := range tl.Items {
			if tl.Items[i].Content == in.Content {
				idx = i
				break
			}
		}
	}

	if idx >= 0 {
		tl.Items[idx].Content = in.Content
		if in.Status != "" {
			tl.Items[idx].Status = in.Status
		}
		if in.Priority != "" {
			tl.Items[idx].Priority = in.Priority
		}
		tl.Items[idx].UpdatedAt = now
		out := tl.Items[idx]
		return out, Save(tl)
	}

	if in.ID == "" {
		in.ID = uuid.NewString()[:8]
	}
	if in.Status == "" {
		in.Status = "pending"
	}
	if in.Priority == "" {
		in.Priority = "medium"
	}
	in.CreatedAt = now
	in.UpdatedAt = now
	tl.Items = append(tl.Items, in)
	tl.SessionID = sessionID
	return in, Save(tl)
}

// CurrentSessionID is set by setupRuntime once a session exists. The
// TodoWrite tool reads it lazily because tools are stateless and built
// before the session id is available.
var (
	currMu  sync.RWMutex
	current string
)

// SetCurrentSessionID is called by setupRuntime once a session id
// exists (either freshly created or resumed). Empty input is allowed
// — defaults to per-session "default" file.
func SetCurrentSessionID(id string) {
	currMu.Lock()
	defer currMu.Unlock()
	current = id
}

// CurrentSessionID returns the runtime-set session id (empty before
// setup completes).
func CurrentSessionID() string {
	currMu.RLock()
	defer currMu.RUnlock()
	return current
}
