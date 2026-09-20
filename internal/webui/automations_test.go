package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/google/uuid"
)

func automationTestServer(t *testing.T) (*Server, *automationManager) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cron")
	workDir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager := newAutomationManager(AutomationOptions{Root: root, WorkDir: workDir, Executable: binary, Model: "fixture-model"})
	manager.stopGrace = 3 * time.Second
	manager.command = func(_ string, args ...string) *exec.Cmd {
		command := exec.Command(binary, append([]string{"-test.run=^TestAutomationProcessHelper$", "--"}, args...)...)
		command.Env = []string{"METIS_AUTOMATION_HELPER=1", "AUTOMATION_TEST_ROOT=" + root}
		return command
	}
	server := NewServer("127.0.0.1:0", nil, nil)
	server.automations = manager
	t.Cleanup(manager.close)
	return server, manager
}
func automationRequest(server *Server, method, url, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, url, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	server.handler().ServeHTTP(recorder, request)
	return recorder
}
func createAutomationFixture(t *testing.T, server *Server) automationInfo {
	t.Helper()
	response := automationRequest(server, "POST", "/api/automations", `{"name":"Test automation","prompt":"Check the workspace","schedule":{"kind":"interval","intervalSeconds":60},"allowTools":["Read"]}`)
	if response.Code != 201 {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var result automationInfo
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func waitAutomation(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("automation condition timed out")
}

func TestAutomationsCRUDValidationAndBoundWorkspace(t *testing.T) {
	server, manager := automationTestServer(t)
	initial := automationRequest(server, "GET", "/api/automations", "")
	if initial.Code != 200 || !strings.Contains(initial.Body.String(), `"enabled":false`) {
		t.Fatalf("initial: %s", initial.Body.String())
	}
	job := createAutomationFixture(t, server)
	canonical, _ := filepath.EvalSymlinks(manager.options.WorkDir)
	if job.WorkDir != canonical || job.Model != "" || job.ModelSource != "workspace-default" || job.Schedule.IntervalSeconds != 60 || job.NextRunAt == "" {
		t.Fatalf("incorrect created job: %+v", job)
	}
	svc, _ := manager.service()
	if err := svc.Update(job.ID, func(j *agent.CronJob) {
		j.Skills = []string{"existing-skill"}
		j.SessionRef = "keep-reference"
		j.ExpiresAt = time.Now().Add(time.Hour)
	}); err != nil {
		t.Fatal(err)
	}
	patched := automationRequest(server, "PATCH", "/api/automations/"+job.ID, `{"name":"Renamed","paused":true}`)
	if patched.Code != 200 {
		t.Fatalf("patch: %s", patched.Body.String())
	}
	svc, _ = manager.service()
	stored, _ := svc.Get(job.ID)
	if stored.Name != "Renamed" || !stored.Paused || stored.WorkDir != canonical || stored.SessionRef != "keep-reference" || len(stored.Skills) != 1 {
		t.Fatalf("patch lost metadata: %+v", stored)
	}
	if result := automationRequest(server, "PATCH", "/api/automations/"+job.ID, `{"paused":false,"enabled":true}`); result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	for _, body := range []string{
		`{"name":"A","prompt":"B","schedule":{"kind":"interval","intervalSeconds":1}}`,
		`{"name":"A","prompt":"B","schedule":{"kind":"cron","cron":"0 9 * * *","timezone":"Not/AZone"}}`,
		`{"name":"A","prompt":"B","schedule":{"kind":"cron","cron":"nonsense"}}`,
		`{"name":"A","prompt":"B","schedule":{"kind":"once","at":"2001-01-01T00:00:00Z"}}`,
		`{"name":"A","prompt":"B","schedule":{"kind":"interval","intervalSeconds":60},"workDir":"/tmp"}`,
		`{"name":"A","prompt":"B","schedule":{"kind":"interval","intervalSeconds":60},"allowTools":["Read\nWrite"]}`,
	} {
		if response := automationRequest(server, "POST", "/api/automations", body); response.Code != 400 {
			t.Fatalf("invalid request accepted: %s %d", body, response.Code)
		}
	}
	response := automationRequest(server, "DELETE", "/api/automations/"+job.ID, "")
	if response.Code != 204 {
		t.Fatal(response.Body.String())
	}
	if response := automationRequest(server, "GET", "/api/automations/"+job.ID, ""); response.Code != 404 {
		t.Fatalf("deleted visible: %d", response.Code)
	}
}

func TestAutomationOnceAndPermissionRules(t *testing.T) {
	server, _ := automationTestServer(t)
	body := fmt.Sprintf(`{"name":"Once","prompt":"Task","schedule":{"kind":"once","at":%q},"allowTools":["Bash(git status:*)"],"disabledTools":["Write"]}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	response := automationRequest(server, "POST", "/api/automations", body)
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	var job automationInfo
	_ = json.Unmarshal(response.Body.Bytes(), &job)
	if job.Repeat != 1 || len(job.AllowTools) != 1 || len(job.DisabledTools) != 1 {
		t.Fatalf("once contract: %+v", job)
	}
	response = automationRequest(server, "PATCH", "/api/automations/"+job.ID, `{"repeat":0}`)
	if response.Code != 400 {
		t.Fatalf("repeat zero accepted for once: %d", response.Code)
	}
}

func TestAutomationEndpointsEnforceOriginAndInputLimit(t *testing.T) {
	server, _ := automationTestServer(t)
	foreign := httptest.NewRequest("POST", "http://127.0.0.1/api/automations", strings.NewReader(`{}`))
	foreign.Header.Set("Origin", "https://evil.invalid")
	recorder := httptest.NewRecorder()
	server.handler().ServeHTTP(recorder, foreign)
	if recorder.Code != 403 {
		t.Fatalf("foreign origin accepted: %d", recorder.Code)
	}
	oversized := automationRequest(server, "POST", "/api/automations", `{"name":"x","prompt":"`+strings.Repeat("a", 130<<10)+`"}`)
	if oversized.Code != 400 {
		t.Fatalf("oversized accepted: %d", oversized.Code)
	}
	if response := automationRequest(server, "GET", "/api/automations/../../secret", ""); response.Code == 200 {
		t.Fatal("path traversal accepted")
	}
}

func TestAutomationRunAdmissionHistoryAndShutdown(t *testing.T) {
	server, manager := automationTestServer(t)
	job := createAutomationFixture(t, server)
	response := automationRequest(server, "POST", "/api/automations/"+job.ID+"/run", "")
	if response.Code != 202 {
		t.Fatalf("run: %d %s", response.Code, response.Body.String())
	}
	var accepted struct {
		RunID string `json:"runId"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &accepted)
	if accepted.RunID == "" {
		t.Fatal("missing run ID")
	}
	if duplicate := automationRequest(server, "POST", "/api/automations/"+job.ID+"/run", ""); duplicate.Code != 409 {
		t.Fatalf("duplicate run: %d", duplicate.Code)
	}
	if deleted := automationRequest(server, "DELETE", "/api/automations/"+job.ID, ""); deleted.Code != 409 {
		t.Fatalf("deleted running job: %d", deleted.Code)
	}
	runs := automationRequest(server, "GET", "/api/automations/"+job.ID+"/runs", "")
	if runs.Code != 200 || !strings.Contains(runs.Body.String(), accepted.RunID) {
		t.Fatalf("history: %s", runs.Body.String())
	}
	record := automationRequest(server, "GET", "/api/automations/"+job.ID+"/runs/"+accepted.RunID, "")
	if record.Code != 200 || !strings.Contains(record.Body.String(), `"status":"running"`) {
		t.Fatalf("record: %s", record.Body.String())
	}
	manager.close()
	saved, err := agent.ReadCronRun(manager.options.Root, job.ID, accepted.RunID)
	if err != nil || saved.Status != "cancelled" {
		t.Fatalf("shutdown did not persist cancellation: %+v %v", saved, err)
	}
}

func TestAutomationRunningUsesExecutionLockNotNewestSkippedRecord(t *testing.T) {
	server, manager := automationTestServer(t)
	job := createAutomationFixture(t, server)
	active, err := agent.BeginCronRun(manager.options.Root, job.ID, uuid.NewString(), "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	defer active.Finish("succeeded", "", "", nil)
	_, err = agent.BeginCronRun(manager.options.Root, job.ID, uuid.NewString(), "manual")
	if !errors.Is(err, agent.ErrCronJobRunning) {
		t.Fatalf("expected busy: %v", err)
	}
	info, err := manager.get(job.ID)
	if err != nil || !info.Running {
		t.Fatalf("running hidden behind skipped: %+v %v", info, err)
	}
	if response := automationRequest(server, "DELETE", "/api/automations/"+job.ID, ""); response.Code != 409 {
		t.Fatalf("deleted externally running job: %d", response.Code)
	}
}

func TestAutomationSchedulerPersistsExplicitChoiceAndOwnsOnlyItsProcess(t *testing.T) {
	server, manager := automationTestServer(t)
	if response := automationRequest(server, "PATCH", "/api/automations/scheduler", `{"enabled":true}`); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if !manager.schedulerState().Running {
		t.Fatal("scheduler did not start")
	}
	manager.mu.Lock()
	process := manager.scheduler
	manager.mu.Unlock()
	manager.close()
	select {
	case <-process.done:
	default:
		t.Fatal("owned scheduler not joined")
	}
	reopened := newAutomationManager(manager.options)
	reopened.command = manager.command
	reopened.stopGrace = manager.stopGrace
	defer reopened.close()
	if !reopened.schedulerState().Enabled || reopened.schedulerState().Running {
		t.Fatal("preference did not persist independently from process state")
	}
	reopened.startConfigured()
	if !reopened.schedulerState().Running {
		t.Fatal("explicit opt-in not restored on app start")
	}
	if err := reopened.setEnabled(false); err != nil {
		t.Fatal(err)
	}
	if reopened.schedulerState().Running || reopened.schedulerState().Enabled {
		t.Fatal("scheduler did not stop")
	}
	final := newAutomationManager(manager.options)
	defer final.close()
	if final.schedulerState().Enabled {
		t.Fatal("off preference not persisted")
	}
	entries, err := os.ReadDir(manager.options.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "desktop-scheduler.json" {
			t.Fatal("preference would be mistaken for a cron job")
		}
	}
}

func TestAutomationProcessFailureIsVisibleAndCanRestart(t *testing.T) {
	_, manager := automationTestServer(t)
	workingCommand := manager.command
	manager.command = func(_ string, _ ...string) *exec.Cmd { return exec.Command("/does-not-exist-metis-test") }
	if err := manager.setEnabled(true); err == nil {
		t.Fatal("expected launch failure")
	}
	state := manager.schedulerState()
	if !state.Enabled || state.Running || state.LastError == "" {
		t.Fatalf("failure hidden: %+v", state)
	}
	manager.command = workingCommand
	if err := manager.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	if state := manager.schedulerState(); !state.Enabled || !state.Running || state.LastError != "" {
		t.Fatalf("restart did not recover scheduler: %+v", state)
	}
}

func TestAutomationProcessHelper(t *testing.T) {
	if os.Getenv("METIS_AUTOMATION_HELPER") != "1" {
		return
	}
	args := os.Args
	index := 0
	for index < len(args) && args[index] != "--" {
		index++
	}
	args = args[index+1:]
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	if len(args) >= 2 && args[0] == "cron" && args[1] == "start" {
		<-signals
		os.Exit(0)
	}
	if len(args) != 5 || args[0] != "cron" || args[1] != "run" || args[3] != "--run-id" {
		fmt.Fprintln(os.Stderr, "invalid fixture command")
		os.Exit(2)
	}
	root := os.Getenv("AUTOMATION_TEST_ROOT")
	run, err := agent.BeginCronRun(root, args[2], args[4], "manual")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	svc, err := agent.NewCronService(root)
	if err != nil {
		os.Exit(4)
	}
	_, _ = svc.RunNow(args[2])
	_ = run.SetSessionID(uuid.NewString())
	<-signals
	_ = run.Finish("cancelled", "Fixture was cancelled", "Fixture output", nil)
	os.Exit(0)
}

func TestAutomationBufferIsBounded(t *testing.T) {
	buffer := &automationLogBuffer{limit: 8}
	_, _ = buffer.Write(bytes.Repeat([]byte("a"), 20))
	_, _ = buffer.Write([]byte("tail"))
	if buffer.String() != "aaaatail" {
		t.Fatalf("unbounded output: %q", buffer.String())
	}
}

func TestAutomationInheritedOutputDoesNotBlockShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	_, manager := automationTestServer(t)
	manager.stopGrace = 50 * time.Millisecond
	manager.command = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "sleep 60 & echo child-started; exit 0")
	}
	if err := manager.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	process := manager.scheduler
	manager.mu.Unlock()
	t.Cleanup(func() { jobs.KillProcessGroup(process.cmd.Process) })
	waitAutomation(t, func() bool { return strings.Contains(process.output.String(), "child-started") })
	start := time.Now()
	manager.close()
	if time.Since(start) > 3*time.Second {
		t.Fatal("shutdown waited for descendant's inherited pipe")
	}
	select {
	case <-process.done:
	default:
		t.Fatal("shutdown did not join CLI wait")
	}
}

func TestAutomationLeaderExitBoundsInheritedOutputWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	_, manager := automationTestServer(t)
	manager.command = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "sleep 60 & echo child-started; exit 0")
	}
	if err := manager.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	process := manager.scheduler
	manager.mu.Unlock()
	t.Cleanup(func() { jobs.KillProcessGroup(process.cmd.Process) })
	select {
	case <-process.done:
	case <-time.After(3 * time.Second):
		t.Fatal("exited CLI stayed running because descendant inherited output")
	}
	if state := manager.schedulerState(); state.Running || state.LastError == "" {
		t.Fatalf("unexpected scheduler state: %+v", state)
	}
}
