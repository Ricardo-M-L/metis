package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/robfig/cron/v3"
)

// AutomationOptions are trusted composition inputs, never browser-supplied executable or storage paths.
type AutomationOptions struct {
	Root       string
	Executable string
	WorkDir    string
	Model      string
}

type automationSchedule struct {
	Kind            string `json:"kind"`
	IntervalSeconds int64  `json:"intervalSeconds,omitempty"`
	At              string `json:"at,omitempty"`
	Cron            string `json:"cron,omitempty"`
	Timezone        string `json:"timezone,omitempty"`
	JitterSeconds   int64  `json:"jitterSeconds,omitempty"`
}

type automationPatch struct {
	Name          *string             `json:"name"`
	Prompt        *string             `json:"prompt"`
	Schedule      *automationSchedule `json:"schedule"`
	Enabled       *bool               `json:"enabled"`
	Paused        *bool               `json:"paused"`
	Repeat        *int                `json:"repeat"`
	Silent        *bool               `json:"silent"`
	SessionMode   *string             `json:"sessionMode"`
	AllowTools    *[]string           `json:"allowTools"`
	DisabledTools *[]string           `json:"disabledTools"`
}

type automationInfo struct {
	WorkDir       string               `json:"workDir"`
	Model         string               `json:"model"`
	ModelSource   string               `json:"modelSource"`
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Prompt        string               `json:"prompt"`
	Schedule      automationSchedule   `json:"schedule"`
	Enabled       bool                 `json:"enabled"`
	Paused        bool                 `json:"paused"`
	NextRunAt     string               `json:"nextRunAt,omitempty"`
	LastRunAt     string               `json:"lastRunAt,omitempty"`
	RunCount      int                  `json:"runCount"`
	Repeat        int                  `json:"repeat"`
	Silent        bool                 `json:"silent"`
	SessionMode   string               `json:"sessionMode"`
	AllowTools    []string             `json:"allowTools"`
	DisabledTools []string             `json:"disabledTools"`
	Running       bool                 `json:"running"`
	LastError     string               `json:"lastError,omitempty"`
	LastRun       *agent.CronRunRecord `json:"lastRun,omitempty"`
}

func (s *Server) handleAutomations(w http.ResponseWriter, r *http.Request) {
	if s.automations == nil {
		if r.Method == http.MethodGet && r.URL.Path == "/api/automations" {
			writeJSON(w, http.StatusOK, map[string]any{"automations": []automationInfo{}, "scheduler": automationSchedulerState{Scope: "application", LastError: "Automation execution is not configured by this backend"}})
		} else {
			writeError(w, http.StatusServiceUnavailable, "Automation execution is not configured by this backend")
		}
		return
	}
	manager := s.automations
	if r.URL.Path == "/api/automations/scheduler" {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, manager.schedulerState())
			return
		}
		if r.Method != http.MethodPatch {
			writeError(w, 405, "method not allowed")
			return
		}
		var input struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeAutomationBody(w, r, &input); err != nil || input.Enabled == nil {
			writeError(w, 400, "enabled must be a boolean")
			return
		}
		if err := manager.setEnabled(*input.Enabled); err != nil {
			automationHTTPError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, manager.schedulerState())
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/api/automations")
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if suffix == "" || suffix == "/" {
		switch r.Method {
		case http.MethodGet:
			jobs, err := manager.jobs()
			if err != nil {
				automationHTTPError(w, err)
				return
			}
			writeJSON(w, 200, map[string]any{"automations": jobs, "scheduler": manager.schedulerState(), "workspace": manager.options.WorkDir, "model": "", "modelSource": "workspace-default"})
		case http.MethodPost:
			var input automationPatch
			if err := decodeAutomationBody(w, r, &input); err != nil {
				writeError(w, 400, err.Error())
				return
			}
			result, err := manager.create(input)
			if err != nil {
				automationHTTPError(w, err)
				return
			}
			writeJSON(w, 201, result)
		default:
			writeError(w, 405, "method not allowed")
		}
		return
	}
	id := parts[0]
	if !validSessionID(id) || len(parts) > 3 {
		writeError(w, 400, "invalid automation identifier")
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			job, err := manager.get(id)
			if err != nil {
				automationHTTPError(w, err)
				return
			}
			writeJSON(w, 200, job)
		case http.MethodPatch:
			var input automationPatch
			if err := decodeAutomationBody(w, r, &input); err != nil {
				writeError(w, 400, err.Error())
				return
			}
			job, err := manager.update(id, input)
			if err != nil {
				automationHTTPError(w, err)
				return
			}
			writeJSON(w, 200, job)
		case http.MethodDelete:
			if err := manager.remove(id); err != nil {
				automationHTTPError(w, err)
				return
			}
			w.WriteHeader(204)
		default:
			writeError(w, 405, "method not allowed")
		}
		return
	}
	if parts[1] == "run" && len(parts) == 2 {
		if r.Method != http.MethodPost {
			writeError(w, 405, "method not allowed")
			return
		}
		runID, err := manager.runNow(id)
		if err != nil {
			automationHTTPError(w, err)
			return
		}
		writeJSON(w, 202, map[string]any{"accepted": true, "runId": runID})
		return
	}
	if parts[1] == "runs" {
		if r.Method != http.MethodGet {
			writeError(w, 405, "method not allowed")
			return
		}
		if _, err := manager.get(id); err != nil {
			automationHTTPError(w, err)
			return
		}
		if len(parts) == 2 {
			records, err := agent.ListCronRuns(manager.options.Root, id, 100)
			if err != nil {
				automationHTTPError(w, err)
				return
			}
			if records == nil {
				records = []agent.CronRunRecord{}
			}
			writeJSON(w, 200, map[string]any{"runs": records})
			return
		}
		if !validSessionID(parts[2]) {
			writeError(w, 400, "invalid run identifier")
			return
		}
		record, err := agent.ReadCronRun(manager.options.Root, id, parts[2])
		if err != nil {
			automationHTTPError(w, err)
			return
		}
		writeJSON(w, 200, record)
		return
	}
	writeError(w, 404, "automation endpoint not found")
}

func decodeAutomationBody(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid automation request: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

type automationError struct {
	status  int
	message string
}

func (e *automationError) Error() string { return e.message }
func automationHTTPError(w http.ResponseWriter, err error) {
	var typed *automationError
	if errors.As(err, &typed) {
		writeError(w, typed.status, typed.message)
		return
	}
	if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "job not found") {
		writeError(w, 404, "Automation or run not found")
		return
	}
	writeError(w, 500, err.Error())
}
func invalidAutomation(message string) error { return &automationError{400, message} }

func (a *automationManager) service() (*agent.CronService, error) {
	return agent.NewCronService(a.options.Root)
}
func (a *automationManager) jobs() ([]automationInfo, error) {
	svc, err := a.service()
	if err != nil {
		return nil, err
	}
	jobs := svc.List()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	result := make([]automationInfo, 0, len(jobs))
	for _, job := range jobs {
		if job.Ephemeral {
			continue
		}
		record, err := a.describe(job)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}
func (a *automationManager) get(id string) (automationInfo, error) {
	svc, err := a.service()
	if err != nil {
		return automationInfo{}, err
	}
	job, ok := svc.Get(id)
	if !ok {
		return automationInfo{}, &automationError{404, "Automation not found"}
	}
	return a.describe(job)
}
func (a *automationManager) describe(job *agent.CronJob) (automationInfo, error) {
	schedule := automationSchedule{Kind: job.Schedule.Kind, IntervalSeconds: job.Schedule.EveryMs / 1000, At: job.Schedule.At, Cron: job.Schedule.CronExpr, Timezone: job.Schedule.TZ, JitterSeconds: job.Schedule.JitterMs / 1000}
	if schedule.Kind == "every" {
		schedule.Kind = "interval"
	}
	if schedule.Kind == "at" {
		schedule.Kind = "once"
	}
	mode := job.SessionMode
	if mode == "" {
		mode = agent.SessionModeIsolated
	}
	workDir := job.WorkDir
	if workDir == "" {
		workDir = a.options.WorkDir
	}
	result := automationInfo{WorkDir: workDir, ModelSource: "workspace-default", ID: job.ID, Name: job.Name, Prompt: job.Prompt, Schedule: schedule, Enabled: job.Enabled, Paused: job.Paused, RunCount: job.RunCount, Repeat: job.Repeat, Silent: job.Silent, SessionMode: mode, AllowTools: append([]string{}, job.AllowTools...), DisabledTools: append([]string{}, job.DisabledTools...)}
	if job.Enabled && !job.Paused && !job.NextRun.IsZero() {
		result.NextRunAt = job.NextRun.UTC().Format(time.RFC3339)
	}
	if !job.LastRun.IsZero() {
		result.LastRunAt = job.LastRun.UTC().Format(time.RFC3339)
	}
	records, err := agent.ListCronRuns(a.options.Root, job.ID, 1)
	if err != nil {
		return result, err
	}
	if len(records) > 0 {
		result.LastRun = &records[0]
		result.LastError = records[0].Error
		result.Running = records[0].Status == "running"
	}
	busy, busyErr := agent.CronJobRunning(a.options.Root, job.ID)
	if busyErr != nil {
		return result, busyErr
	}
	result.Running = busy
	a.mu.Lock()
	if a.manual[job.ID] != nil {
		result.Running = true
	}
	a.mu.Unlock()
	return result, nil
}
func (a *automationManager) create(input automationPatch) (automationInfo, error) {
	if input.Name == nil || input.Prompt == nil || input.Schedule == nil {
		return automationInfo{}, invalidAutomation("name, prompt and schedule are required")
	}
	workDir, err := filepath.EvalSymlinks(a.options.WorkDir)
	if err != nil {
		return automationInfo{}, err
	}
	info, err := os.Stat(workDir)
	if err != nil || !info.IsDir() {
		return automationInfo{}, invalidAutomation("automation workspace is unavailable")
	}
	job := &agent.CronJob{Enabled: true, SessionMode: agent.SessionModeIsolated, WorkDir: workDir}
	if err := patchAutomation(job, input, true); err != nil {
		return automationInfo{}, err
	}
	svc, err := a.service()
	if err != nil {
		return automationInfo{}, err
	}
	if err := svc.Create(job); err != nil {
		return automationInfo{}, invalidAutomation(err.Error())
	}
	return a.describe(job)
}
func (a *automationManager) update(id string, input automationPatch) (automationInfo, error) {
	svc, err := a.service()
	if err != nil {
		return automationInfo{}, err
	}
	current, ok := svc.Get(id)
	if !ok {
		return automationInfo{}, &automationError{404, "Automation not found"}
	}
	if err := patchAutomation(current, input, false); err != nil {
		return automationInfo{}, err
	}
	// Apply only explicitly edited fields to the latest storage transaction. Preserve counters, TTL, skill/session metadata and sibling changes.
	var patchErr error
	if err := svc.Update(id, func(job *agent.CronJob) {
		candidate := *job
		patchErr = patchAutomation(&candidate, input, false)
		if patchErr == nil {
			*job = candidate
		}
	}); err != nil {
		return automationInfo{}, err
	}
	if patchErr != nil {
		return automationInfo{}, patchErr
	}
	return a.get(id)
}
func (a *automationManager) remove(id string) error {
	info, err := a.get(id)
	if err != nil {
		return err
	}
	if info.Running {
		return &automationError{409, "This automation is running. Wait for it to finish before deleting it."}
	}
	svc, err := a.service()
	if err != nil {
		return err
	}
	err = agent.WithCronJobIdle(a.options.Root, id, func() error { return svc.Remove(id) })
	if errors.Is(err, agent.ErrCronJobRunning) {
		return &automationError{409, "This automation is running. Wait for it to finish before deleting it."}
	}
	return err
}
func patchAutomation(job *agent.CronJob, input automationPatch, creating bool) error {
	if input.Name != nil {
		value := strings.TrimSpace(*input.Name)
		if value == "" || len([]rune(value)) > 160 {
			return invalidAutomation("name must contain 1–160 characters")
		}
		job.Name = value
	}
	if input.Prompt != nil {
		value := strings.TrimSpace(*input.Prompt)
		if value == "" || len(value) > 64000 {
			return invalidAutomation("prompt must contain 1–64000 bytes")
		}
		job.Prompt = value
	}
	if input.Schedule != nil {
		value, err := validateAutomationSchedule(*input.Schedule)
		if err != nil {
			return err
		}
		job.Schedule = value
		if value.Kind == "at" {
			job.Repeat = 1
		}
	}
	if input.Repeat != nil {
		if *input.Repeat < 0 || *input.Repeat > 1000000 {
			return invalidAutomation("repeat must be between 0 and 1000000")
		}
		job.Repeat = *input.Repeat
	}
	if job.Schedule.Kind == "at" && job.Repeat != 1 {
		return invalidAutomation("once schedules require repeat=1")
	}
	if input.Enabled != nil {
		job.Enabled = *input.Enabled
	}
	if input.Paused != nil {
		job.Paused = *input.Paused
	}
	if input.Silent != nil {
		job.Silent = *input.Silent
	}
	if input.SessionMode != nil {
		switch *input.SessionMode {
		case agent.SessionModeIsolated, agent.SessionModePersistent, agent.SessionModeMain:
			job.SessionMode = *input.SessionMode
		default:
			return invalidAutomation("sessionMode must be isolated, persistent or main")
		}
	}
	for _, entry := range []struct {
		source *[]string
		target *[]string
	}{{input.AllowTools, &job.AllowTools}, {input.DisabledTools, &job.DisabledTools}} {
		if entry.source == nil {
			continue
		}
		if len(*entry.source) > 100 {
			return invalidAutomation("at most 100 tool rules are allowed")
		}
		values := make([]string, 0, len(*entry.source))
		for _, rule := range *entry.source {
			rule = strings.TrimSpace(rule)
			if rule == "" || len(rule) > 1000 || strings.ContainsAny(rule, "\r\n\x00") {
				return invalidAutomation("tool rules must be nonempty single lines of at most 1000 bytes")
			}
			values = append(values, rule)
		}
		*entry.target = values
	}
	// Existing once schedules may be inspected or paused after expiry. Starting one again requires a new future time.
	if !creating && job.Schedule.Kind == "at" && input.Schedule == nil && ((input.Enabled != nil && *input.Enabled) || (input.Paused != nil && !*input.Paused)) {
		at, err := time.Parse(time.RFC3339, job.Schedule.At)
		if err != nil || !at.After(time.Now()) {
			return invalidAutomation("choose a future date before re-enabling a once schedule")
		}
	}
	return nil
}
func validateAutomationSchedule(input automationSchedule) (agent.CronSchedule, error) {
	result := agent.CronSchedule{TZ: input.Timezone}
	if input.Timezone != "" {
		if _, err := time.LoadLocation(input.Timezone); err != nil {
			return result, invalidAutomation("invalid IANA timezone")
		}
	}
	if input.JitterSeconds < 0 || input.JitterSeconds > 86400 {
		return result, invalidAutomation("jitterSeconds must be between 0 and 86400")
	}
	result.JitterMs = input.JitterSeconds * 1000
	switch input.Kind {
	case "interval":
		if input.IntervalSeconds < 30 || input.IntervalSeconds > 315360000 {
			return result, invalidAutomation("intervalSeconds must be between 30 and 315360000")
		}
		result.Kind = "every"
		result.EveryMs = input.IntervalSeconds * 1000
	case "once":
		at, err := time.Parse(time.RFC3339, input.At)
		if err != nil || !at.After(time.Now()) {
			return result, invalidAutomation("at must be a future RFC3339 timestamp")
		}
		result.Kind = "at"
		result.At = at.Format(time.RFC3339)
	case "cron":
		if len(input.Cron) > 256 {
			return result, invalidAutomation("cron expression is too long")
		}
		parser := cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
		if _, err := parser.Parse(input.Cron); err != nil {
			return result, invalidAutomation("invalid cron expression: " + err.Error())
		}
		result.Kind = "cron"
		result.CronExpr = input.Cron
	default:
		return result, invalidAutomation("schedule kind must be interval, once or cron")
	}
	return result, nil
}
func automationPreferencePath(root string) string {
	return filepath.Join(root, "desktop-scheduler.state")
}
