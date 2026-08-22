package builtin

// goal.go — Goal system tools. Builds on top of the task store in
// internal/tasks/ to add long-term goal tracking with priority,
// status, and outcome management. Mirrors harness's tool-goal
// package concept.
//
// A Goal is a higher-level wrapper around one or more Tasks:
//   - A Goal can span multiple tasks across multiple sessions
//   - Goals have a priority and status lifecycle
//   - Goals persist to disk as JSON

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/google/uuid"
)

// ---- Data model -----------------------------------------------------------

// GoalStatus represents the lifecycle state of a goal.
type GoalStatus string

const (
	GoalPending    GoalStatus = "pending"
	GoalInProgress GoalStatus = "in_progress"
	GoalCompleted  GoalStatus = "completed"
	GoalCancelled  GoalStatus = "cancelled"
)

// GoalPriority represents the importance level.
type GoalPriority string

const (
	GoalHigh   GoalPriority = "high"
	GoalMedium GoalPriority = "medium"
	GoalLow    GoalPriority = "low"
)

// Goal is a high-level objective that can span multiple tasks.
type Goal struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Status      GoalStatus   `json:"status"`
	Priority    GoalPriority `json:"priority"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	CompletedAt *time.Time   `json:"completed_at,omitempty"`
	Tags        []string     `json:"tags,omitempty"`
	Tasks       []string     `json:"tasks,omitempty"` // linked task IDs
}

// ---- GoalStore ------------------------------------------------------------

// GoalStore persists goals to a JSON file. Thread-safe.
type GoalStore struct {
	mu    sync.RWMutex
	path  string
	goals map[string]*Goal
}

// goalStoreSingleton reuses one store instance per goals.json path: the
// registry rebuilds on session switches, and two instances would race on
// the shared file (each has its own mutex).
var goalStoreSingleton struct {
	sync.Mutex
	byPath map[string]*GoalStore
}

// NewGoalStore creates or loads a GoalStore from the given directory.
// Callers share one instance per directory.
func NewGoalStore(dir string) *GoalStore {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("goal store: create dir: %v", err)
	}
	path := filepath.Join(dir, "goals.json")
	goalStoreSingleton.Lock()
	defer goalStoreSingleton.Unlock()
	if goalStoreSingleton.byPath == nil {
		goalStoreSingleton.byPath = make(map[string]*GoalStore)
	}
	if existing, ok := goalStoreSingleton.byPath[path]; ok {
		return existing
	}
	s := &GoalStore{
		path:  path,
		goals: make(map[string]*Goal),
	}
	// Load existing data; corrupt files log rather than vanish silently.
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		var list []*Goal
		if jerr := json.Unmarshal(data, &list); jerr != nil {
			log.Printf("goal store: corrupt %s: %v", path, jerr)
		} else {
			for _, g := range list {
				s.goals[g.ID] = g
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		log.Printf("goal store: read %s: %v", path, err)
	}
	goalStoreSingleton.byPath[path] = s
	return s
}

// save writes the live map; saveWith merges overrides (for copy-then-
// commit updates); saveWithout excludes one id (for safe deletes).
func (s *GoalStore) save() error                               { return s.write(map[string]*Goal{}) }
func (s *GoalStore) saveWith(overrides map[string]*Goal) error { return s.write(overrides) }
func (s *GoalStore) saveWithout(excludeID string) error {
	return s.write(map[string]*Goal{}, excludeID)
}

func (s *GoalStore) write(overrides map[string]*Goal, exclude ...string) error {
	excluded := ""
	if len(exclude) > 0 {
		excluded = exclude[0]
	}
	list := make([]*Goal, 0, len(s.goals)+len(overrides))
	for id, g := range s.goals {
		if id == excluded {
			continue
		}
		if ov, ok := overrides[id]; ok {
			list = append(list, ov)
			continue
		}
		list = append(list, g)
	}
	for id, ov := range overrides {
		if _, exists := s.goals[id]; !exists {
			_ = id
			list = append(list, ov)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	// Atomic replace with a UNIQUE temp name: two store instances sharing
	// one goals.json (web sessions rebuilt the registry) must not race on
	// a fixed .tmp file.
	f, err := os.CreateTemp(filepath.Dir(s.path), ".goals-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *GoalStore) Create(title, description string, priority GoalPriority, tags []string) (*Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := uuid.NewString()
	g := &Goal{
		ID:          id,
		Title:       title,
		Description: description,
		Status:      GoalPending,
		Priority:    priority,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Tags:        tags,
		Tasks:       nil,
	}
	if g.Priority == "" {
		g.Priority = GoalMedium
	}
	s.goals[id] = g
	if err := s.save(); err != nil {
		delete(s.goals, id)
		return nil, err
	}
	cp := *g
	return &cp, nil
}

func (s *GoalStore) Get(id string) (*Goal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.goals[id]
	if !ok {
		return nil, false
	}
	cp := *g
	return &cp, true
}

func (s *GoalStore) List(status string, priority string) []*Goal {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Goal, 0, len(s.goals))
	for _, g := range s.goals {
		if status != "" && string(g.Status) != status {
			continue
		}
		if priority != "" && string(g.Priority) != priority {
			continue
		}
		cp := *g
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *GoalStore) Update(id string, status *GoalStatus, priority *GoalPriority, title, description *string, tags []string, tasks []string) (*Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.goals[id]
	if !ok {
		return nil, fmt.Errorf("goal %q not found", id)
	}
	// Mutate a copy first: a failed save must not leave the in-memory
	// goal in a state the disk never saw.
	updated := *g
	if status != nil {
		updated.Status = *status
		if *status == GoalCompleted {
			now := time.Now()
			updated.CompletedAt = &now
		}
	}
	if priority != nil {
		updated.Priority = *priority
	}
	if title != nil {
		updated.Title = *title
	}
	if description != nil {
		updated.Description = *description
	}
	if tags != nil {
		updated.Tags = tags
	}
	if tasks != nil {
		updated.Tasks = tasks
	}
	updated.UpdatedAt = time.Now()
	if err := s.saveWith(map[string]*Goal{id: &updated}); err != nil {
		return nil, err
	}
	s.goals[id] = &updated
	cp := updated
	return &cp, nil
}

func (s *GoalStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.goals[id]
	if !ok {
		return fmt.Errorf("goal %q not found", id)
	}
	if err := s.saveWithout(id); err != nil {
		return err // map untouched: the goal is still listed
	}
	delete(s.goals, id)
	_ = g
	return nil
}

// ---- Current store (package-level, set by runtime) ------------------------

var (
	currentGoalStore   *GoalStore
	currentGoalStoreMu sync.RWMutex
)

// SetGoalStore sets the active goal store for the current session.
func SetGoalStore(s *GoalStore) {
	currentGoalStoreMu.Lock()
	defer currentGoalStoreMu.Unlock()
	currentGoalStore = s
}

// CurrentGoalStore returns the active goal store, or nil.
func CurrentGoalStore() *GoalStore {
	currentGoalStoreMu.RLock()
	defer currentGoalStoreMu.RUnlock()
	return currentGoalStore
}

// ---- Tools ----------------------------------------------------------------

// GoalCreate creates a new goal.
type GoalCreate struct {
	tools.BaseTool
	gate *permission.Gate
}

func NewGoalCreate(gate *permission.Gate) GoalCreate {
	return GoalCreate{gate: gate}
}

func (GoalCreate) Name() string { return "GoalCreate" }

func (GoalCreate) Description() string {
	return "Create a new goal. Goals are long-term objectives that can span multiple tasks. Use GoalCreate to define what you want to achieve, then use TaskCreate to break it into actionable steps. Track progress with GoalUpdate."
}

func (GoalCreate) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"title", "description"},
		"properties": map[string]any{
			"title":       map[string]any{"type": "string", "description": "Goal title — short, imperative (e.g. 'Build CI pipeline')"},
			"description": map[string]any{"type": "string", "description": "Detailed description of what success looks like"},
			"priority":    map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}, "description": "Importance (default medium)"},
			"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional labels for categorisation"},
		},
	}
}

func (GoalCreate) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (g GoalCreate) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := g.gate.Check(context.Background(), "GoalCreate", "")
	return mapDecision(d), src
}

func (GoalCreate) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store := CurrentGoalStore()
	if store == nil {
		return errResult("GoalCreate: no goal store for the current session"), nil
	}
	title, _ := in["title"].(string)
	title = strings.TrimSpace(title)
	if title == "" {
		return errResult("GoalCreate: title is required"), nil
	}
	desc, _ := in["description"].(string)
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return errResult("GoalCreate: description is required"), nil
	}
	priority := GoalMedium
	if p, ok := in["priority"].(string); ok {
		switch GoalPriority(p) {
		case GoalHigh, GoalMedium, GoalLow:
			priority = GoalPriority(p)
		}
	}
	tags := anyToStringSlice(in["tags"])

	goal, err := store.Create(title, desc, priority, tags)
	if err != nil {
		return nil, err
	}
	return &tools.Result{Output: fmt.Sprintf("Goal #%s created: %s [%s]", goal.ID, goal.Title, goal.Priority)}, nil
}

// GoalUpdate updates an existing goal's status, priority, or metadata.
type GoalUpdate struct {
	tools.BaseTool
	gate *permission.Gate
}

func NewGoalUpdate(gate *permission.Gate) GoalUpdate {
	return GoalUpdate{gate: gate}
}

func (GoalUpdate) Name() string { return "GoalUpdate" }

func (GoalUpdate) Description() string {
	return "Update a goal's status, priority, title, description, tags, or linked tasks. Use GoalUpdate to mark progress (pending → in_progress → completed) or adjust priorities as work evolves."
}

func (GoalUpdate) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"goalId"},
		"properties": map[string]any{
			"goalId":      map[string]any{"type": "string", "description": "Goal ID to update"},
			"status":      map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "cancelled"}, "description": "New lifecycle status"},
			"priority":    map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}, "description": "New priority level"},
			"title":       map[string]any{"type": "string", "description": "New title"},
			"description": map[string]any{"type": "string", "description": "New description"},
			"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replace all tags"},
			"tasks":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Linked task IDs"},
		},
	}
}

func (GoalUpdate) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (g GoalUpdate) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := g.gate.Check(context.Background(), "GoalUpdate", "")
	return mapDecision(d), src
}

func (GoalUpdate) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store := CurrentGoalStore()
	if store == nil {
		return errResult("GoalUpdate: no goal store for the current session"), nil
	}
	id, _ := in["goalId"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return errResult("GoalUpdate: goalId is required"), nil
	}

	var status *GoalStatus
	if s, ok := in["status"].(string); ok && s != "" {
		st := GoalStatus(s)
		switch st {
		case GoalPending, GoalInProgress, GoalCompleted, GoalCancelled:
			status = &st
		default:
			return errResult(fmt.Sprintf("GoalUpdate: invalid status %q", s)), nil
		}
	}
	var priority *GoalPriority
	if p, ok := in["priority"].(string); ok && p != "" {
		pr := GoalPriority(p)
		switch pr {
		case GoalHigh, GoalMedium, GoalLow:
			priority = &pr
		default:
			return errResult(fmt.Sprintf("GoalUpdate: invalid priority %q", p)), nil
		}
	}
	var title *string
	if t, ok := in["title"].(string); ok {
		title = &t
	}
	var desc *string
	if d, ok := in["description"].(string); ok {
		desc = &d
	}
	tags := anyToStringSlice(in["tags"])
	var tasks []string
	if t, ok := in["tasks"].([]any); ok {
		for _, v := range t {
			if s, ok := v.(string); ok {
				tasks = append(tasks, s)
			}
		}
	}

	goal, err := store.Update(id, status, priority, title, desc, tags, tasks)
	if err != nil {
		return errResult("GoalUpdate: " + err.Error()), nil
	}
	return &tools.Result{Output: fmt.Sprintf("Goal #%s updated: %s (priority=%s, status=%s)", goal.ID, goal.Title, goal.Priority, goal.Status)}, nil
}

// GoalList lists goals, optionally filtered by status and priority.
type GoalList struct {
	tools.BaseTool
	gate *permission.Gate
}

func NewGoalList(gate *permission.Gate) GoalList {
	return GoalList{gate: gate}
}

func (GoalList) Name() string { return "GoalList" }

func (GoalList) Description() string {
	return "List all goals, optionally filtered by status (pending/in_progress/completed/cancelled) and priority (high/medium/low). Returns goals in reverse-chronological order."
}

func (GoalList) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":   map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "cancelled"}, "description": "Filter by status"},
			"priority": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}, "description": "Filter by priority"},
		},
	}
}

func (GoalList) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (g GoalList) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := g.gate.Check(context.Background(), "GoalList", "")
	return mapDecision(d), src
}

func (GoalList) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store := CurrentGoalStore()
	if store == nil {
		return errResult("GoalList: no goal store for the current session"), nil
	}
	status, _ := in["status"].(string)
	priority, _ := in["priority"].(string)

	goals := store.List(status, priority)
	if len(goals) == 0 {
		return &tools.Result{Output: "(no goals)"}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d goal(s):\n", len(goals))
	for _, g := range goals {
		tasks := ""
		if len(g.Tasks) > 0 {
			tasks = fmt.Sprintf(" (%d tasks)", len(g.Tasks))
		}
		tags := ""
		if len(g.Tags) > 0 {
			tags = " [" + strings.Join(g.Tags, ", ") + "]"
		}
		fmt.Fprintf(&b, "  #%-8s %-12s %-6s %s%s%s\n",
			g.ID, g.Status, g.Priority, g.Title, tags, tasks)
		// oneLine is rune-safe: byte slicing could split a CJK rune.
		desc := oneLine(g.Description, 60)
		if desc != "" {
			fmt.Fprintf(&b, "           %s\n", desc)
		}
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// GoalDelete removes a goal by ID.
type GoalDelete struct {
	tools.BaseTool
	gate *permission.Gate
}

func NewGoalDelete(gate *permission.Gate) GoalDelete {
	return GoalDelete{gate: gate}
}

func (GoalDelete) Name() string { return "GoalDelete" }

func (GoalDelete) Description() string {
	return "Delete a goal by ID. This removes the goal entirely; linked tasks are not affected."
}

func (GoalDelete) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"goalId"},
		"properties": map[string]any{
			"goalId": map[string]any{"type": "string", "description": "Goal ID to delete"},
		},
	}
}

func (GoalDelete) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (g GoalDelete) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := g.gate.Check(context.Background(), "GoalDelete", strFromAny(in["goalId"]))
	return mapDecision(d), src
}

func (GoalDelete) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store := CurrentGoalStore()
	if store == nil {
		return errResult("GoalDelete: no goal store for the current session"), nil
	}
	id, _ := in["goalId"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return errResult("GoalDelete: goalId is required"), nil
	}
	if err := store.Delete(id); err != nil {
		return errResult("GoalDelete: " + err.Error()), nil
	}
	return &tools.Result{Output: fmt.Sprintf("Goal #%s deleted", id)}, nil
}

// ---- helpers --------------------------------------------------------------

// anyToStringSlice extracts []string from a JSON array value.
func anyToStringSlice(v any) []string {
	if v == nil {
		return nil
	}
	if s, ok := v.([]string); ok {
		return s
	}
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
