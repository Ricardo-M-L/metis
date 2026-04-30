// Package task implements background task management.
// Inspired by Claude Code's TaskCreate/TaskStop: tasks run in background goroutines,
// write output to disk, can be polled or awaited, and survive REPL restarts.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TaskStatus is the current status of a background task.
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning  TaskStatus = "running"
	TaskDone     TaskStatus = "done"
	TaskFailed   TaskStatus = "failed"
	TaskKilled   TaskStatus = "killed"
)

// Task is a background task with output streamed to disk.
type Task struct {
	ID       string
	Name     string
	Status   TaskStatus
	ExitCode int
	OutputPath string
	CreatedAt time.Time
	StartTime time.Time
	EndTime  time.Time
	ctx      context.Context
	cancel   context.CancelFunc
}

type taskRunner struct {
	task   *Task
	output *os.File
	wg     sync.WaitGroup
}

// Manager tracks all background tasks.
type TaskManager struct {
	mu    sync.RWMutex
	tasks map[string]*Task
	runners map[string]*taskRunner
	root   string
}

func NewTaskManager(root string) *TaskManager {
	os.MkdirAll(root, 0o755)
	return &TaskManager{tasks: make(map[string]*Task), runners: make(map[string]*taskRunner), root: root}
}

func (m *TaskManager) Create(name, prompt string) (*Task, error) {
	id := fmt.Sprintf("t%d", time.Now().UnixNano()%1e12)
	task := &Task{
		ID:        id,
		Name:      name,
		Status:    TaskPending,
		OutputPath: filepath.Join(m.root, id+".txt"),
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.tasks[id] = task
	m.mu.Unlock()
	return task, nil
}

func (m *TaskManager) Start(id string, fn func(ctx context.Context) error) error {
	m.mu.Lock()
	task := m.tasks[id]
	if task == nil {
		m.mu.Unlock()
		return fmt.Errorf("task %s not found", id)
	}
	f, err := os.Create(task.OutputPath)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	task.ctx, task.cancel = context.WithCancel(context.Background())
	task.Status = TaskRunning
	task.StartTime = time.Now()
	runner := &taskRunner{task: task, output: f}
	m.runners[id] = runner
	m.mu.Unlock()

	runner.wg.Add(1)
	go func() {
		defer runner.wg.Done()
		defer f.Close()

		// Compute terminal status and persist it under the manager mutex —
		// Stop()/Get()/List() all read task fields concurrently and the
		// race detector flags writes here against those reads otherwise.
		err := fn(task.ctx)
		var newStatus TaskStatus
		var exitCode int
		switch {
		case task.ctx.Err() == context.Canceled:
			newStatus = TaskKilled
		case err != nil:
			newStatus = TaskFailed
			exitCode = 1
		default:
			newStatus = TaskDone
		}

		m.mu.Lock()
		task.Status = newStatus
		task.ExitCode = exitCode
		task.EndTime = time.Now()
		delete(m.runners, id)
		m.mu.Unlock()
	}()
	return nil
}

func (m *TaskManager) Stop(id string) error {
	m.mu.Lock()
	task := m.tasks[id]
	if task == nil {
		m.mu.Unlock()
		return fmt.Errorf("task %s not found", id)
	}
	cancel := task.cancel
	// Mark the task killed under the lock so concurrent Get/List see a
	// consistent state. The actual cancellation runs after we drop the
	// lock to avoid blocking other Manager calls if cancel is slow.
	task.Status = TaskKilled
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (m *TaskManager) Get(id string) *Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tasks[id]
}

func (m *TaskManager) List() []*Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	return out
}

func (m *TaskManager) Output(id string, offset, limit int) (string, error) {
	m.mu.RLock()
	task := m.tasks[id]
	m.mu.RUnlock()
	if task == nil {
		return "", fmt.Errorf("task %s not found", id)
	}
	b, err := os.ReadFile(task.OutputPath)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	if offset > 0 && offset < len(lines) {
		lines = lines[offset:]
	}
	if limit > 0 && limit < len(lines) {
		lines = lines[:limit]
	}
	return strings.Join(lines, "\n"), nil
}

func (m *TaskManager) SaveState(path string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	type taskJSON struct {
		ID    string    `json:"id"`
		Name  string    `json:"name"`
		Status TaskStatus `json:"status"`
	}
	out := make([]taskJSON, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, taskJSON{ID: t.ID, Name: t.Name, Status: t.Status})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// PendingOutputReader returns a channel that yields new output lines as they appear.
// Useful for streaming task output to the REPL.
func (m *TaskManager) PendingOutputReader(id string, pollInterval time.Duration) (<-chan string, error) {
	m.mu.RLock()
	task := m.tasks[id]
	m.mu.RUnlock()
	if task == nil {
		return nil, fmt.Errorf("task %s not found", id)
	}
	ch := make(chan string, 100)
	go func() {
		defer close(ch)
		pos := int64(0)
		for {
			m.mu.RLock()
			st := task.Status
			m.mu.RUnlock()
			if st == TaskDone || st == TaskFailed || st == TaskKilled {
				return
			}
			b, err := os.ReadFile(task.OutputPath)
			if err == nil && int64(len(b)) > pos {
				newLines := strings.Split(string(b[pos:]), "\n")
				pos = int64(len(b))
				for _, l := range newLines {
					if l != "" {
						select {
						case ch <- l:
						default:
						}
					}
				}
			}
			time.Sleep(pollInterval)
		}
	}()
	return ch, nil
}
