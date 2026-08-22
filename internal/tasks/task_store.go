package tasks

// Structured Task* tools subsystem (claude-code's TaskCreate / TaskGet
// / TaskList / TaskUpdate / TaskOutput / TaskStop). Lives in this
// package (not internal/runtime) because the tool implementations live
// in internal/tools/builtin and would cycle through runtime otherwise.
//
// Distinct from the simple todo store in tasks.go — the two coexist:
//   - Item / Upsert / List(s,id)        → simple TodoWrite/TodoRead
//   - Task / TaskStore / TaskPatch      → structured Task* tools (here)

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// TaskStatus is the task lifecycle state. Default = "pending".
type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskCompleted  TaskStatus = "completed"
	TaskDeleted    TaskStatus = "deleted"
)

// Task is the persisted task record.
type Task struct {
	ID          string         `json:"id"`
	Subject     string         `json:"subject"`
	Description string         `json:"description"`
	ActiveForm  string         `json:"active_form,omitempty"`
	Status      TaskStatus     `json:"status"`
	Owner       string         `json:"owner,omitempty"`
	Blocks      []string       `json:"blocks,omitempty"`
	BlockedBy   []string       `json:"blocked_by,omitempty"`
	Output      string         `json:"output,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// TaskStore persists tasks under ~/.metis/sessions/<sid>/tasks-structured.json.
// Concurrency-safe; single sync.Mutex covers all of read/write since
// task throughput is low (LLM-driven, not high-fanout).
type TaskStore struct {
	mu        sync.Mutex
	dir       string // ~/.metis/sessions/<sid>
	tasks     map[string]*Task
	nextNum   int // for generating "1", "2", "3", ... ids
	sessionID string
}

// taskFilePath uses tasks-structured.json (not tasks.json) so the simple
// Item store in this same package can keep its existing tasks.json file.
func taskFilePath(sessionID string) string {
	// Path-traversal guard (mirrors session.Store.path): a crafted session
	// id with separators / ".." must not escape the sessions tree.
	return filepath.Join(config.Home(), "sessions", filepath.Base(sessionID), "tasks-structured.json")
}

// NewTaskStore opens (or creates empty) the task store for the given
// session id. Errors loading a corrupt file are non-fatal — we keep an
// empty store and log to stderr; the agent can still operate.
func NewTaskStore(sessionID string) *TaskStore {
	s := &TaskStore{
		dir:       filepath.Dir(taskFilePath(sessionID)),
		tasks:     map[string]*Task{},
		sessionID: sessionID,
	}
	s.load()
	return s
}

func (s *TaskStore) load() {
	b, err := os.ReadFile(taskFilePath(s.sessionID))
	if err != nil {
		return
	}
	var list []*Task
	if err := json.Unmarshal(b, &list); err != nil {
		fmt.Fprintf(os.Stderr, "task store: cannot parse %s: %v\n", taskFilePath(s.sessionID), err)
		return
	}
	maxN := 0
	for _, t := range list {
		s.tasks[t.ID] = t
		if n, err := strconv.Atoi(t.ID); err == nil && n > maxN {
			maxN = n
		}
	}
	s.nextNum = maxN + 1
}

func (s *TaskStore) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	all := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool {
		ni, _ := strconv.Atoi(all[i].ID)
		nj, _ := strconv.Atoi(all[j].ID)
		return ni < nj
	})
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := taskFilePath(s.sessionID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, taskFilePath(s.sessionID))
}

// Create adds a new pending task. Returns the generated id.
func (s *TaskStore) Create(subject, description, activeForm string, metadata map[string]any) (*Task, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, fmt.Errorf("subject is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextNum == 0 {
		s.nextNum = 1
	}
	id := strconv.Itoa(s.nextNum)
	s.nextNum++
	now := time.Now()
	t := &Task{
		ID:          id,
		Subject:     subject,
		Description: description,
		ActiveForm:  activeForm,
		Status:      TaskPending,
		Metadata:    metadata,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.tasks[id] = t
	if err := s.saveLocked(); err != nil {
		// Roll back the in-memory mutation so a save failure doesn't
		// surface a phantom task.
		delete(s.tasks, id)
		s.nextNum--
		return nil, err
	}
	return t, nil
}

// Get returns a copy of the task with id (so callers can't mutate the
// stored value).
func (s *TaskStore) Get(id string) (*Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// List returns a snapshot of all tasks (excluding deleted) in id order.
// Pass includeDeleted=true if the caller specifically wants to see
// tombstones (rare).
func (s *TaskStore) List(includeDeleted bool) []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		if !includeDeleted && t.Status == TaskDeleted {
			continue
		}
		cp := *t
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		ni, _ := strconv.Atoi(out[i].ID)
		nj, _ := strconv.Atoi(out[j].ID)
		return ni < nj
	})
	return out
}

// Update applies a partial update (only non-zero / non-nil fields are
// written). To delete a task, set status to TaskDeleted.
type TaskPatch struct {
	Subject      *string
	Description  *string
	ActiveForm   *string
	Status       *TaskStatus
	Owner        *string
	AppendOutput string         // appended to existing Output
	AddBlocks    []string       // appended (deduped) to Blocks
	AddBlockedBy []string       // appended (deduped) to BlockedBy
	Metadata     map[string]any // shallow-merged; nil value deletes the key
}

func (s *TaskStore) Update(id string, patch TaskPatch) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, fmt.Errorf("task %s not found", id)
	}
	if patch.Subject != nil {
		t.Subject = *patch.Subject
	}
	if patch.Description != nil {
		t.Description = *patch.Description
	}
	if patch.ActiveForm != nil {
		t.ActiveForm = *patch.ActiveForm
	}
	if patch.Status != nil {
		t.Status = *patch.Status
	}
	if patch.Owner != nil {
		t.Owner = *patch.Owner
	}
	if patch.AppendOutput != "" {
		if t.Output != "" && !strings.HasSuffix(t.Output, "\n") {
			t.Output += "\n"
		}
		t.Output += patch.AppendOutput
	}
	if len(patch.AddBlocks) > 0 {
		t.Blocks = mergeUnique(t.Blocks, patch.AddBlocks)
	}
	if len(patch.AddBlockedBy) > 0 {
		t.BlockedBy = mergeUnique(t.BlockedBy, patch.AddBlockedBy)
	}
	if patch.Metadata != nil {
		if t.Metadata == nil {
			t.Metadata = map[string]any{}
		}
		for k, v := range patch.Metadata {
			if v == nil {
				delete(t.Metadata, k)
			} else {
				t.Metadata[k] = v
			}
		}
	}
	t.UpdatedAt = time.Now()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cp := *t
	return &cp, nil
}

func mergeUnique(have, add []string) []string {
	seen := make(map[string]struct{}, len(have))
	for _, v := range have {
		seen[v] = struct{}{}
	}
	out := append([]string(nil), have...)
	for _, v := range add {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// activeStoreMu guards the global currentTaskStore handle below.
var (
	activeStoreMu      sync.Mutex
	currentTaskStore   *TaskStore
	currentTaskSession string
)

// SetCurrentTaskStore is called by the runtime as the chat session is
// resolved so the Task* tools — which are stateless objects registered
// once at startup — can find the right store at call time.
func SetCurrentTaskStore(sessionID string) {
	activeStoreMu.Lock()
	defer activeStoreMu.Unlock()
	if sessionID == "" {
		currentTaskStore = nil
		currentTaskSession = ""
		return
	}
	if currentTaskStore != nil && currentTaskSession == sessionID {
		return
	}
	currentTaskStore = NewTaskStore(sessionID)
	currentTaskSession = sessionID
}

// CurrentTaskStore returns the active store; nil when no session is set.
func CurrentTaskStore() *TaskStore {
	activeStoreMu.Lock()
	defer activeStoreMu.Unlock()
	return currentTaskStore
}

// deleteStructured removes the per-session directory used by Task* tools.
// Holding activeStoreMu prevents the global router from binding a new store
// for this session midway through deletion. If an old current store is still
// bound, its mutex also serializes against an in-flight save.
func deleteStructured(sessionID string) error {
	activeStoreMu.Lock()
	defer activeStoreMu.Unlock()

	if currentTaskStore != nil && currentTaskSession == sessionID {
		currentTaskStore.mu.Lock()
		defer currentTaskStore.mu.Unlock()
		currentTaskStore = nil
		currentTaskSession = ""
	}
	dir := filepath.Dir(taskFilePath(sessionID))
	return os.RemoveAll(dir)
}
