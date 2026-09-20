package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/processutil"
	"github.com/google/uuid"
)

type automationSchedulerState struct {
	Enabled   bool   `json:"enabled"`
	Running   bool   `json:"running"`
	Available bool   `json:"available"`
	Scope     string `json:"scope"`
	LastError string `json:"lastError,omitempty"`
}

type automationProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	output   *automationLogBuffer
	err      error
	stopping bool
	runID    string
}

type automationManager struct {
	options     AutomationOptions
	mu          sync.Mutex
	operationMu sync.Mutex
	scheduler   *automationProcess
	manual      map[string]*automationProcess
	enabled     bool
	lastError   string
	closed      bool
	startOnce   sync.Once
	command     func(string, ...string) *exec.Cmd
	stopGrace   time.Duration
}

func newAutomationManager(options AutomationOptions) *automationManager {
	manager := &automationManager{options: options, manual: make(map[string]*automationProcess), command: exec.Command, stopGrace: 15 * time.Second}
	if options.Root == "" || !filepath.IsAbs(options.Root) || options.Executable == "" || !filepath.IsAbs(options.Executable) || options.WorkDir == "" || !filepath.IsAbs(options.WorkDir) {
		manager.lastError = "Automation backend requires absolute storage, executable and working-directory paths"
		return manager
	}
	data, err := os.ReadFile(automationPreferencePath(options.Root))
	if err == nil {
		var saved struct {
			Enabled bool `json:"enabled"`
		}
		if json.Unmarshal(data, &saved) != nil {
			manager.lastError = "Saved scheduler preference is invalid; scheduling remains off"
		} else {
			manager.enabled = saved.Enabled
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		manager.lastError = "Cannot read scheduler preference: " + err.Error()
	}
	return manager
}
func (a *automationManager) available() bool {
	if !filepath.IsAbs(a.options.Root) || !filepath.IsAbs(a.options.Executable) || !filepath.IsAbs(a.options.WorkDir) {
		return false
	}
	st, err := os.Stat(a.options.Executable)
	if err != nil || st.IsDir() {
		return false
	}
	dir, err := os.Stat(a.options.WorkDir)
	return err == nil && dir.IsDir()
}
func (a *automationManager) schedulerState() automationSchedulerState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return automationSchedulerState{Enabled: a.enabled, Running: a.scheduler != nil, Available: a.available() && !a.closed, Scope: "application", LastError: a.lastError}
}
func (a *automationManager) startConfigured() {
	if a == nil {
		return
	}
	a.startOnce.Do(func() {
		a.operationMu.Lock()
		defer a.operationMu.Unlock()
		a.mu.Lock()
		enabled := a.enabled
		a.mu.Unlock()
		if enabled {
			_ = a.startScheduler()
		}
	})
}
func (a *automationManager) setEnabled(enabled bool) error {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return &automationError{503, "Desktop backend is shutting down"}
	}
	a.mu.Unlock()
	if !a.available() {
		return &automationError{503, "Automation executable or working directory is unavailable"}
	}
	if err := a.savePreference(enabled); err != nil {
		return err
	}
	a.mu.Lock()
	a.enabled = enabled
	process := a.scheduler
	a.mu.Unlock()
	if enabled {
		return a.startScheduler()
	}
	if process != nil {
		a.stop(process)
	}
	return nil
}
func (a *automationManager) savePreference(enabled bool) error {
	if err := os.MkdirAll(a.options.Root, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Enabled bool `json:"enabled"`
	}{enabled})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(a.options.Root, ".desktop-scheduler-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), automationPreferencePath(a.options.Root))
}
func (a *automationManager) startScheduler() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return &automationError{503, "Desktop backend is shutting down"}
	}
	if a.scheduler != nil {
		return nil
	}
	if !a.available() {
		a.lastError = "Automation executable or working directory is unavailable"
		return &automationError{503, a.lastError}
	}
	process, err := a.startProcessLocked([]string{"cron", "start"}, "")
	if err != nil {
		a.lastError = err.Error()
		return err
	}
	a.lastError = ""
	a.scheduler = process
	go a.observe(process, "")
	return nil
}
func (a *automationManager) runNow(id string) (string, error) {
	info, err := a.get(id)
	if err != nil {
		return "", err
	}
	if info.Running {
		return "", &automationError{409, "This automation is already running"}
	}
	busy, err := agent.CronJobRunning(a.options.Root, id)
	if err != nil {
		return "", err
	}
	if busy {
		return "", &automationError{409, "This automation is already running in another process"}
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return "", &automationError{503, "Desktop backend is shutting down"}
	}
	if a.manual[id] != nil {
		a.mu.Unlock()
		return "", &automationError{409, "This automation is already running"}
	}
	if !a.available() {
		a.mu.Unlock()
		return "", &automationError{503, "Automation executable or working directory is unavailable"}
	}
	runID := uuid.NewString()
	process, err := a.startProcessLocked([]string{"cron", "run", id, "--run-id", runID}, runID)
	if err != nil {
		a.mu.Unlock()
		return "", err
	}
	a.manual[id] = process
	a.mu.Unlock()
	go a.observe(process, id)
	// The CLI obtains the cross-process execution lock and writes a durable run before model work.
	// Confirm that admission instead of returning success for a competing process that immediately refuses it.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if record, readErr := agent.ReadCronRun(a.options.Root, id, runID); readErr == nil && record != nil {
			if record.Status == "skipped" {
				return "", &automationError{409, "This automation is already running in another process"}
			}
			return runID, nil
		}
		select {
		case <-process.done:
			if record, readErr := agent.ReadCronRun(a.options.Root, id, runID); readErr == nil && record != nil {
				if record.Status == "skipped" {
					return "", &automationError{409, "This automation is already running in another process"}
				}
				return runID, nil
			}
			detail := strings.TrimSpace(process.output.String())
			if detail == "" {
				detail = "CLI exited before creating a run record"
			}
			return "", &automationError{502, detail}
		case <-deadline.C:
			a.stop(process)
			return "", &automationError{504, "CLI did not acknowledge the run within 3 seconds; the process was stopped. Check run history before retrying."}
		case <-tick.C:
		}
	}
}
func (a *automationManager) startProcessLocked(args []string, runID string) (*automationProcess, error) {
	cmd := a.command(a.options.Executable, args...)
	cmd.Dir = a.options.WorkDir
	jobs.ApplyProcessGroup(cmd)
	// A descendant may inherit our output pipes after the CLI leader exits.
	// Bound os/exec's pipe-drain wait so shutdown cannot hang on that descendant.
	cmd.WaitDelay = time.Second
	// Cron authorization is enforced by CLI ModeDefault + the job's explicit allow-list.
	// The private native-shell shutdown token is unrelated to scheduled work.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "METIS_DESKTOP_FRAME_TOKEN=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	output := &automationLogBuffer{limit: 64 << 10}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start automation CLI: %w", err)
	}
	return &automationProcess{cmd: cmd, done: make(chan struct{}), output: output, runID: runID}, nil
}
func (a *automationManager) observe(process *automationProcess, jobID string) {
	err := process.cmd.Wait()
	// This is a private process group created above; descendants belong to this
	// CLI invocation and must not outlive its leader.
	jobs.KillProcessGroup(process.cmd.Process)
	a.mu.Lock()
	process.err = err
	if jobID == "" && a.scheduler == process {
		a.scheduler = nil
		if !process.stopping && !a.closed {
			detail := strings.TrimSpace(process.output.String())
			if detail == "" {
				detail = "Scheduler process exited"
			}
			a.lastError = detail
		}
	} else if jobID != "" && a.manual[jobID] == process {
		delete(a.manual, jobID)
	}
	close(process.done)
	a.mu.Unlock()
}
func (a *automationManager) stop(process *automationProcess) {
	a.mu.Lock()
	select {
	case <-process.done:
		a.mu.Unlock()
		return
	default:
	}
	process.stopping = true
	a.mu.Unlock()
	_ = processutil.Terminate(process.cmd.Process.Pid)
	timer := time.NewTimer(a.stopGrace)
	defer timer.Stop()
	select {
	case <-process.done:
		return
	case <-timer.C:
	}
	jobs.KillProcessGroup(process.cmd.Process)
	forced := time.NewTimer(process.cmd.WaitDelay + time.Second)
	defer forced.Stop()
	select {
	case <-process.done:
	case <-forced.C:
		a.mu.Lock()
		a.lastError = "Automation process did not finish after forced shutdown"
		a.mu.Unlock()
	}
}
func (a *automationManager) close() {
	if a == nil {
		return
	}
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	processes := make([]*automationProcess, 0, len(a.manual)+1)
	if a.scheduler != nil {
		processes = append(processes, a.scheduler)
	}
	for _, process := range a.manual {
		processes = append(processes, process)
	}
	a.mu.Unlock()
	var group sync.WaitGroup
	for _, process := range processes {
		group.Add(1)
		go func() { defer group.Done(); a.stop(process) }()
	}
	group.Wait()
}

type automationLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	limit  int
}

func (b *automationLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(data)
	b.buffer.Write(data)
	if b.buffer.Len() > b.limit {
		remaining := append([]byte(nil), b.buffer.Bytes()[b.buffer.Len()-b.limit:]...)
		b.buffer.Reset()
		b.buffer.Write(remaining)
	}
	return n, nil
}
func (b *automationLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
