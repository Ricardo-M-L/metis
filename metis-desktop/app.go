package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx      context.Context
	workDir  string
	metisBin string
	sendMu   sync.Mutex

	findMetis            func() (string, error)
	runMetis             func(ctx context.Context, binary string, args []string, dir string) (stdout, stderr string, err error)
	chooseWorkspace      func(context.Context, string) (string, error)
	checkDesktopUpdate   func(context.Context, string) (DesktopUpdateStatus, error)
	installDesktopUpdate func(context.Context, string, string) (DesktopUpdateStatus, error)
	desktopPath          func() (string, error)
	restartDesktop       func(path, workspace, metisBin string) error
	resolveUpdatedMetis  func(current string) (string, error)
	scheduleRestart      func(func())
	quit                 func(context.Context)
	updateMu             sync.Mutex

	// webuiCmd is the in-process-browser backend child (metis desktop
	// --web). The native window embeds it behind a tokenised frame URL; the
	// child is reaped when the Wails context shuts down.
	webuiCmd   *exec.Cmd
	webuiDone  chan error
	webuiPort  int
	webuiToken string
	webuiMu    sync.Mutex
}

func NewApp() *App {
	updater := defaultDesktopUpdater()
	app := &App{
		workDir:  parseWorkspaceArg(os.Args),
		metisBin: parseMetisBinArg(os.Args),
		runMetis: runMetisCommand,
		chooseWorkspace: func(ctx context.Context, defaultDir string) (string, error) {
			return wailsruntime.OpenDirectoryDialog(ctx, wailsruntime.OpenDialogOptions{
				Title:                "Choose a workspace folder",
				DefaultDirectory:     defaultDir,
				CanCreateDirectories: true,
			})
		},
		checkDesktopUpdate:   updater.Check,
		installDesktopUpdate: updater.Install,
		desktopPath:          currentDesktopPath,
		restartDesktop:       restartDesktopProcess,
		resolveUpdatedMetis:  resolveStableMetisBinary,
		scheduleRestart: func(fn func()) {
			go func() {
				time.Sleep(350 * time.Millisecond)
				fn()
			}()
		},
		quit: wailsruntime.Quit,
	}
	app.findMetis = func() (string, error) { return findMetisBinary(app.metisBin) }
	return app
}

func parseMetisBinArg(args []string) string {
	for i := 1; i < len(args); i++ {
		if args[i] == "--metis-bin" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "--metis-bin=") {
			return strings.TrimPrefix(args[i], "--metis-bin=")
		}
	}
	return ""
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// shutdown is registered with Wails' explicit OnShutdown hook. The startup
// context is a background context on macOS and is not cancelled by Cmd+Q, so
// waiting on ctx.Done would leave the web UI child orphaned.
func (a *App) shutdown(context.Context) {
	a.stopWebUI()
}

func (a *App) stopWebUI() {
	a.webuiMu.Lock()
	cmd := a.webuiCmd
	done := a.webuiDone
	port := a.webuiPort
	token := a.webuiToken
	a.webuiCmd = nil
	a.webuiDone = nil
	a.webuiPort = 0
	a.webuiToken = ""
	a.webuiMu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	stopWebUIProcess(cmd, done, port, token)
}

const (
	desktopShutdownTokenHeader  = "X-Metis-Desktop-Token"
	webUIShutdownRequestTimeout = time.Second
	// The backend's accepted shutdown path drains the active turn and then
	// joins memory distillation (35s) and Auto Memory (95s) before writing the
	// final session boundary. Keep the native shell alive for the complete
	// durability barrier instead of interrupting a healthy child after 5s.
	webUIShutdownGrace = 4 * time.Minute
	webUISignalGrace   = 10 * time.Second
	webUIKillWait      = 2 * time.Second
)

type webUIStopPolicy struct {
	shutdownGrace time.Duration
	signalGrace   time.Duration
	killWait      time.Duration
}

var defaultWebUIStopPolicy = webUIStopPolicy{
	shutdownGrace: webUIShutdownGrace,
	signalGrace:   webUISignalGrace,
	killWait:      webUIKillWait,
}

// stopWebUIProcess first uses the authenticated loopback control channel. It
// works on Windows as well as Unix and lets cmdDesktop return normally through
// runtime.Cleanup. Interrupt and hard kill remain bounded fallbacks for an
// older or unresponsive child.
func stopWebUIProcess(cmd *exec.Cmd, done <-chan error, port int, token string) {
	stopWebUIProcessWithPolicy(cmd, done, port, token, defaultWebUIStopPolicy)
}

func stopWebUIProcessWithPolicy(cmd *exec.Cmd, done <-chan error, port int, token string, policy webUIStopPolicy) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if waitForWebUIProcess(done, 0) {
		return
	}
	if requestWebUIShutdown(port, token) && waitForWebUIProcess(done, policy.shutdownGrace) {
		return
	}
	if err := cmd.Process.Signal(os.Interrupt); err == nil {
		if waitForWebUIProcess(done, policy.signalGrace) {
			return
		}
	}
	_ = cmd.Process.Kill()
	_ = waitForWebUIProcess(done, policy.killWait)
}

func requestWebUIShutdown(port int, token string) bool {
	if port < 1 || strings.TrimSpace(token) == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), webUIShutdownRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/shutdown", http.NoBody)
	if err != nil {
		return false
	}
	request.Header.Set(desktopShutdownTokenHeader, token)
	// Never route the privileged loopback control request through an ambient
	// HTTP(S)_PROXY. This client exists only for the child bound to 127.0.0.1.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	return response.StatusCode == http.StatusAccepted
}

func waitForWebUIProcess(done <-chan error, timeout time.Duration) bool {
	if done == nil {
		return false
	}
	if timeout <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func parseWorkspaceArg(args []string) string {
	for i := 1; i < len(args); i++ {
		if args[i] == "--workspace" && i+1 < len(args) {
			if abs, err := filepath.Abs(args[i+1]); err == nil {
				return abs
			}
		}
		if strings.HasPrefix(args[i], "--workspace=") {
			if abs, err := filepath.Abs(strings.TrimPrefix(args[i], "--workspace=")); err == nil {
				return abs
			}
		}
	}
	dir, _ := os.Getwd()
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// StartWebUI launches the in-process browser UI backend (metis desktop
// --web) on a free loopback port and returns its tokenised frame URL. The
// native shell embeds it: one UI codebase, true SSE streaming, no per-operation
// CLI subprocesses, while Wails bindings remain available in the parent frame.
const (
	webUIStartupAttempts   = 2
	webUIStartupTimeout    = 15 * time.Second
	webUIStartupRetryDelay = 250 * time.Millisecond
	webUIStartupLogLimit   = 32 << 10
)

type processLogBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *processLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > webUIStartupLogLimit {
		b.data = append([]byte(nil), b.data[len(b.data)-webUIStartupLogLimit:]...)
	}
	return len(p), nil
}

func (b *processLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func (a *App) StartWebUI() (string, error) {
	a.webuiMu.Lock()
	defer a.webuiMu.Unlock()
	if a.webuiCmd != nil && a.webuiPort > 0 {
		return a.webUIURL(), nil
	}
	binary, err := a.findMetis()
	if err != nil {
		return "", err
	}
	failures := make([]string, 0, webUIStartupAttempts)
	for attempt := 1; attempt <= webUIStartupAttempts; attempt++ {
		frameURL, attemptErr := a.startWebUIAttempt(binary)
		if attemptErr == nil {
			return frameURL, nil
		}
		failure := fmt.Sprintf("attempt %d/%d: %v", attempt, webUIStartupAttempts, attemptErr)
		failures = append(failures, failure)
		fmt.Fprintln(os.Stderr, "Metis Desktop WebUI:", failure)
		if attempt < webUIStartupAttempts {
			time.Sleep(webUIStartupRetryDelay)
		}
	}
	return "", fmt.Errorf("webui failed after %d attempts: %s", webUIStartupAttempts, strings.Join(failures, "; "))
}

func (a *App) startWebUIAttempt(binary string) (string, error) {
	port, err := freePort()
	if err != nil {
		return "", err
	}
	frameToken, err := newDesktopFrameToken()
	if err != nil {
		return "", fmt.Errorf("create native frame token: %w", err)
	}
	logs := &processLogBuffer{}
	cmd := exec.Command(binary, "desktop", "--web", "--port", strconv.Itoa(port))
	cmd.Dir = a.workDir
	cmd.Env = append(os.Environ(), "METIS_DESKTOP_FRAME_TOKEN="+frameToken)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start webui: %w", err)
	}
	done := make(chan error, 1)
	a.webuiCmd = cmd
	a.webuiDone = done
	a.webuiPort = port
	a.webuiToken = frameToken
	// Reap the child on every exit path. Process.Kill alone does not release
	// the OS process record; Wait belongs in exactly one goroutine.
	go func() {
		err := cmd.Wait()
		done <- err
		close(done)
		a.webuiMu.Lock()
		if a.webuiCmd == cmd {
			a.clearWebUIAttempt(cmd)
		}
		a.webuiMu.Unlock()
	}()

	// Wait for the backend to answer its health probe before the webview
	// navigates, otherwise the first paint is a connection error.
	baseURL := a.webUIBaseURL()
	deadline := time.Now().Add(webUIStartupTimeout)
	client := &http.Client{Timeout: 750 * time.Millisecond}
	lastProbe := "health endpoint did not answer"
	for time.Now().Before(deadline) {
		select {
		case processErr := <-done:
			a.clearWebUIAttempt(cmd)
			if processErr == nil {
				processErr = errors.New("process exited")
			}
			return "", webUIAttemptError(fmt.Errorf("webui exited before becoming ready: %w", processErr), logs)
		default:
		}
		resp, probeErr := client.Get(baseURL + "/api/health")
		if probeErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return a.webUIURL(), nil
			}
			lastProbe = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			lastProbe = probeErr.Error()
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	a.clearWebUIAttempt(cmd)
	return "", webUIAttemptError(fmt.Errorf("webui did not become ready within %s; last health probe: %s", webUIStartupTimeout, lastProbe), logs)
}

func (a *App) clearWebUIAttempt(cmd *exec.Cmd) {
	if a.webuiCmd != cmd {
		return
	}
	a.webuiCmd = nil
	a.webuiDone = nil
	a.webuiPort = 0
	a.webuiToken = ""
}

func webUIAttemptError(err error, logs *processLogBuffer) error {
	detail := strings.TrimSpace(logs.String())
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w; child output: %s", err, detail)
}

func (a *App) webUIBaseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(a.webuiPort)
}

func (a *App) webUIURL() string {
	base := a.webUIBaseURL()
	if a.webuiToken == "" {
		return base
	}
	return base + "/?desktop-frame=" + url.QueryEscape(a.webuiToken)
}

func newDesktopFrameToken() (string, error) {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", token[:]), nil
}

// freePort binds an ephemeral loopback port, closes it, and returns the
// number. Small race window, acceptable for a local desktop app.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

func (a *App) GetVersion() string {
	return "0.4.38"
}

// ChooseWorkspaceDirectory is the native half of the iframe bridge. The web
// UI never receives arbitrary filesystem access: it may only ask Wails to show
// one system directory picker and gets back the path the user selected.
func (a *App) ChooseWorkspaceDirectory() (string, error) {
	if a.chooseWorkspace == nil {
		return "", errors.New("native directory picker is unavailable")
	}
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	path, err := a.chooseWorkspace(base, a.workDir)
	if err != nil || strings.TrimSpace(path) == "" {
		return strings.TrimSpace(path), err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve selected workspace: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.New("selected workspace is not a readable directory")
	}
	return abs, nil
}

// GetUpdateStatus checks only. It never downloads or changes the running app,
// which preserves the user's explicit choice to stay on the current version.
func (a *App) GetUpdateStatus() (DesktopUpdateStatus, error) {
	if a.checkDesktopUpdate == nil {
		return DesktopUpdateStatus{}, errors.New("Desktop updater is unavailable")
	}
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 20*time.Second)
	defer cancel()
	return a.checkDesktopUpdate(ctx, a.GetVersion())
}

// InstallUpdateAndRestart updates the CLI first (the CLI serves the shared
// Desktop UI), then installs the verified native bundle and starts the new
// bundle with the same workspace. Nothing runs unless the WebView's explicit
// update confirmation calls this method.
func (a *App) InstallUpdateAndRestart() (DesktopUpdateStatus, error) {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()

	status, err := a.GetUpdateStatus()
	if err != nil {
		return status, err
	}
	if !status.Available {
		return status, errors.New("Metis Desktop is already up to date")
	}
	if !status.CanUpdate {
		return status, errors.New(status.Message)
	}
	binary, err := a.findMetis()
	if err != nil {
		return status, err
	}
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	cliCtx, cliCancel := context.WithTimeout(base, 10*time.Minute)
	stdout, stderr, err := a.runMetis(cliCtx, binary, []string{"update"}, a.workDir)
	cliCancel()
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = strings.TrimSpace(stdout)
		}
		if detail == "" {
			detail = err.Error()
		}
		return status, fmt.Errorf("update Metis CLI: %s", detail)
	}
	if a.resolveUpdatedMetis != nil {
		binary, err = a.resolveUpdatedMetis(binary)
		if err != nil {
			return status, fmt.Errorf("resolve updated Metis CLI: %w", err)
		}
	}
	if a.desktopPath == nil || a.installDesktopUpdate == nil {
		return status, errors.New("native Desktop updater is unavailable")
	}
	appPath, err := a.desktopPath()
	if err != nil {
		return status, err
	}
	installCtx, installCancel := context.WithTimeout(base, 10*time.Minute)
	status, err = a.installDesktopUpdate(installCtx, a.GetVersion(), appPath)
	installCancel()
	if err != nil {
		return status, err
	}
	if a.restartDesktop == nil || a.scheduleRestart == nil || a.quit == nil {
		return status, errors.New("Desktop restart is unavailable")
	}
	if err := a.restartDesktop(appPath, a.workDir, binary); err != nil {
		return status, fmt.Errorf("restart updated Desktop: %w", err)
	}
	status.Restarting = true
	a.scheduleRestart(func() { a.quit(base) })
	return status, nil
}

// resolveStableMetisBinary keeps Desktop attached to the stable bin/metis
// launcher after self-update. Older launchers passed os.Executable's resolved
// share/metis/versions/<version>/metis target; reusing that immutable path
// would restart the new Desktop against the old CLI and eventually a cleaned
// up executable.
func resolveStableMetisBinary(current string) (string, error) {
	current = strings.TrimSpace(current)
	if current == "" {
		return "", errors.New("updated Metis CLI path is invalid")
	}
	abs, err := filepath.Abs(current)
	if err != nil || abs == "" {
		return "", errors.New("updated Metis CLI path is invalid")
	}
	if stable, managed := stableMetisLauncherForVersion(abs, runtime.GOOS); managed {
		info, statErr := os.Stat(stable)
		if statErr != nil || !executableMetisFile(stable, info, runtime.GOOS) {
			return "", fmt.Errorf("managed Metis launcher is unavailable after update: %s", stable)
		}
		return stable, nil
	}
	info, statErr := os.Stat(abs)
	if statErr != nil || !executableMetisFile(abs, info, runtime.GOOS) {
		return "", fmt.Errorf("updated Metis CLI is unavailable: %s", abs)
	}
	return abs, nil
}

func stableMetisLauncherForVersion(path, goos string) (string, bool) {
	name := "metis"
	if goos == "windows" {
		name = "metis.exe"
	}
	if (goos == "windows" && !strings.EqualFold(filepath.Base(path), name)) ||
		(goos != "windows" && filepath.Base(path) != name) {
		return "", false
	}
	versionDir := filepath.Dir(path)
	versionsRoot := filepath.Dir(versionDir)
	version := filepath.Base(versionDir)
	if !validDesktopVersion(version) {
		return "", false
	}
	if (goos == "windows" && !strings.EqualFold(filepath.Base(versionsRoot), "versions")) ||
		(goos != "windows" && filepath.Base(versionsRoot) != "versions") {
		return "", false
	}
	if goos == "windows" {
		return filepath.Join(filepath.Dir(versionsRoot), "bin", name), true
	}
	managedRoot := filepath.Dir(versionsRoot)
	shareRoot := filepath.Dir(managedRoot)
	if filepath.Base(managedRoot) != "metis" || filepath.Base(shareRoot) != "share" {
		return "", false
	}
	return filepath.Join(filepath.Dir(shareRoot), "bin", name), true
}

func (a *App) GetProjectDir() string {
	return a.workDir
}

type SessionInfo struct {
	ID        string `json:"ID"`
	Title     string `json:"Title"`
	Model     string `json:"Model"`
	CreatedAt string `json:"CreatedAt"`
}

// ScheduledTaskInfo is the native client's compact view of a durable Metis
// cron job. The CLI remains the source of truth; the desktop process never
// reads or mutates cron files directly.
type ScheduledTaskInfo struct {
	ID          string `json:"ID"`
	Name        string `json:"Name"`
	Prompt      string `json:"Prompt"`
	Schedule    string `json:"Schedule"`
	Enabled     bool   `json:"Enabled"`
	Paused      bool   `json:"Paused"`
	NextRun     string `json:"NextRun"`
	LastRun     string `json:"LastRun"`
	RunCount    int    `json:"RunCount"`
	Repeat      int    `json:"Repeat"`
	Silent      bool   `json:"Silent"`
	SessionMode string `json:"SessionMode"`
}

type cronListWireRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	Schedule struct {
		Kind     string `json:"kind"`
		EveryMs  int64  `json:"every_ms"`
		CronExpr string `json:"cron_expr"`
		At       string `json:"at"`
		JitterMs int64  `json:"jitter_ms"`
		TZ       string `json:"tz"`
	} `json:"schedule"`
	Enabled     bool   `json:"enabled"`
	Paused      bool   `json:"paused"`
	NextRun     string `json:"nextRun"`
	LastRun     string `json:"lastRun"`
	RunCount    int    `json:"runCount"`
	Repeat      int    `json:"repeat"`
	Silent      bool   `json:"silent"`
	SessionMode string `json:"sessionMode"`
}

// GetScheduledTasks returns durable jobs from `metis cron list --json`.
// Keeping this behind the CLI ensures custom METIS_HOME/config resolution and
// any scheduler migrations are shared with terminal users.
func (a *App) GetScheduledTasks() ([]ScheduledTaskInfo, error) {
	stdout, stderr, err := a.runCLI(20*time.Second, "cron", "list", "--json")
	if err != nil {
		return nil, cronCLIError("list scheduled tasks", stderr, err)
	}
	var records []cronListWireRecord
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		return nil, fmt.Errorf("list scheduled tasks: decode CLI response: %w", err)
	}
	out := make([]ScheduledTaskInfo, 0, len(records))
	for _, record := range records {
		if !validCronJobID(record.ID) {
			continue
		}
		name := strings.TrimSpace(record.Name)
		if name == "" {
			name = "Untitled task"
		}
		out = append(out, ScheduledTaskInfo{
			ID:          record.ID,
			Name:        name,
			Prompt:      record.Prompt,
			Schedule:    formatCronSchedule(record),
			Enabled:     record.Enabled,
			Paused:      record.Paused,
			NextRun:     record.NextRun,
			LastRun:     record.LastRun,
			RunCount:    record.RunCount,
			Repeat:      record.Repeat,
			Silent:      record.Silent,
			SessionMode: record.SessionMode,
		})
	}
	return out, nil
}

func formatCronSchedule(record cronListWireRecord) string {
	var schedule string
	switch record.Schedule.Kind {
	case "every":
		schedule = "Every " + (time.Duration(record.Schedule.EveryMs) * time.Millisecond).String()
	case "cron":
		schedule = record.Schedule.CronExpr
	case "at":
		schedule = "At " + record.Schedule.At
	default:
		schedule = record.Schedule.Kind
	}
	if schedule == "" {
		schedule = "Unknown schedule"
	}
	if record.Schedule.TZ != "" {
		schedule += " · " + record.Schedule.TZ
	}
	if record.Schedule.JitterMs > 0 {
		schedule += " · jitter " + (time.Duration(record.Schedule.JitterMs) * time.Millisecond).String()
	}
	return schedule
}

func (a *App) PauseScheduledTask(id string) error {
	_, err := a.runCronCommand(20*time.Second, "pause", id)
	return err
}

func (a *App) ResumeScheduledTask(id string) error {
	_, err := a.runCronCommand(20*time.Second, "resume", id)
	return err
}

func (a *App) DeleteScheduledTask(id string) error {
	_, err := a.runCronCommand(20*time.Second, "rm", id)
	return err
}

func (a *App) RunScheduledTask(id string) (string, error) {
	return a.runCronCommand(10*time.Minute, "run", id)
}

func (a *App) runCronCommand(timeout time.Duration, action, id string) (string, error) {
	if !validCronJobID(id) {
		return "", fmt.Errorf("%s scheduled task: invalid job id", action)
	}
	stdout, stderr, err := a.runCLI(timeout, "cron", action, id)
	if err != nil {
		return strings.TrimSpace(stdout), cronCLIError(action+" scheduled task", stderr, err)
	}
	return strings.TrimSpace(stdout), nil
}

func cronCLIError(action, stderr string, err error) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" && err != nil {
		detail = err.Error()
	}
	if detail == "" {
		detail = "unknown CLI error"
	}
	return fmt.Errorf("%s: %s", action, detail)
}

func validCronJobID(id string) bool {
	return validSessionID(id)
}

func (a *App) GetSessions() []SessionInfo {
	// Ask the CLI to apply the workspace predicate before the limit. Fetching
	// the global top 30 and filtering here makes an older conversation vanish
	// as soon as other workspaces have enough newer sessions.
	stdout, _, err := a.runCLI(20*time.Second, "sessions", "list", "--json", "--work-dir", a.workDir, "--limit", "30")
	if err != nil {
		return nil
	}
	var records []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Model     string `json:"model"`
		WorkDir   string `json:"workDir"`
		CreatedAt string `json:"createdAt"`
	}
	if json.Unmarshal([]byte(stdout), &records) != nil {
		return nil
	}
	out := make([]SessionInfo, 0, len(records))
	for _, record := range records {
		if !validSessionID(record.ID) {
			continue
		}
		if record.WorkDir != "" && !samePath(record.WorkDir, a.workDir) {
			continue
		}
		title := record.Title
		if title == "" {
			title = "Untitled"
		}
		out = append(out, SessionInfo{
			ID:        record.ID,
			Title:     title,
			Model:     record.Model,
			CreatedAt: record.CreatedAt,
		})
		if len(out) == 30 {
			break
		}
	}
	return out
}

func samePath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo)
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr == nil {
		left = leftAbs
	}
	if rightErr == nil {
		right = rightAbs
	}
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

type ChatMessage struct {
	Role    string `json:"Role"`
	Content string `json:"Content"`
}

// GetSessionMessages returns the persisted text transcript for a sidebar
// selection. Tool-only blocks are intentionally omitted from this compact
// chat view; their surrounding assistant text remains visible.
func (a *App) GetSessionMessages(id string) []ChatMessage {
	if !validSessionID(id) {
		return nil
	}
	stdout, _, err := a.runCLI(20*time.Second, "sessions", "export", id)
	if err != nil {
		return nil
	}
	var out []ChatMessage
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		var entry struct {
			Type    string `json:"type"`
			Message *struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Type != "message" || entry.Message == nil {
			continue
		}
		if entry.Message.Role != "user" && entry.Message.Role != "assistant" {
			continue
		}
		var text []string
		for _, block := range entry.Message.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				text = append(text, block.Text)
			}
		}
		if len(text) > 0 {
			out = append(out, ChatMessage{Role: entry.Message.Role, Content: strings.Join(text, "\n")})
		}
	}
	return out
}

func (a *App) GetCurrentModel() string {
	stdout, _, err := a.runCLI(20*time.Second, "config", "show", "--json")
	if err != nil {
		return "unknown"
	}
	var summary struct {
		Model string `json:"model"`
	}
	if json.Unmarshal([]byte(stdout), &summary) != nil || strings.TrimSpace(summary.Model) == "" {
		return "unknown"
	}
	return summary.Model
}

type MessageResponse struct {
	Text     string `json:"text"`
	ThreadID string `json:"threadId"`
	Warning  string `json:"warning,omitempty"`
	Error    string `json:"error,omitempty"`
}

// SendMessage runs one normal `metis run` turn against the selected session.
// Using the CLI's existing runtime keeps model/provider/tool/session behaviour
// identical to terminal use and avoids the old global daemon conversation,
// which ignored threadID and could deadlock on permission events.
func (a *App) SendMessage(input, threadID, approvalMode string) MessageResponse {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()

	input = strings.TrimSpace(input)
	if input == "" {
		return MessageResponse{ThreadID: threadID, Error: "message is empty"}
	}
	if threadID != "" && !validSessionID(threadID) {
		return MessageResponse{Error: "invalid session id"}
	}
	binary, err := a.findMetis()
	if err != nil {
		return MessageResponse{ThreadID: threadID, Error: err.Error()}
	}
	mode := normalizeApprovalMode(approvalMode)
	args := []string{"run", "--no-auth-wizard"}
	newSession := threadID == ""
	if threadID == "" {
		threadID, err = newDesktopSessionID()
		if err != nil {
			return MessageResponse{Error: "create session id: " + err.Error()}
		}
		args = append(args, "--session-id", threadID, "--name", sessionTitle(input))
	} else {
		args = append(args, "--resume", threadID)
	}
	args = append(args, "--mode", mode)
	args = append(args, "--", input)

	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 10*time.Minute)
	defer cancel()
	stdout, stderr, err := a.runMetis(ctx, binary, args, a.workDir)
	text := strings.TrimSpace(stdout)
	if err != nil {
		// setupRuntime can fail before `metis run` writes the fresh header (for
		// example, when provider configuration is invalid). Do not hand that
		// unpersisted random ID to the frontend: its next retry would use
		// `--resume` and fail with "session not found". If the header was
		// already written and the provider failed later, keep the ID so the
		// user can continue the persisted conversation.
		if newSession && !a.sessionPersisted(binary, threadID) {
			threadID = ""
		}
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return MessageResponse{Text: text, ThreadID: threadID, Error: detail}
	}
	if text == "" {
		text = "Metis completed without a text response."
	}
	return MessageResponse{Text: text, ThreadID: threadID, Warning: userVisibleCLIWarning(stderr)}
}

func (a *App) sessionPersisted(binary, id string) bool {
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 5*time.Second)
	defer cancel()
	stdout, _, err := a.runMetis(ctx, binary, []string{"sessions", "export", id}, a.workDir)
	return err == nil && strings.TrimSpace(stdout) != ""
}

func newDesktopSessionID() (string, error) {
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("desktop-%x", token[:]), nil
}

func sessionTitle(firstPrompt string) string {
	runes := []rune(strings.TrimSpace(firstPrompt))
	if len(runes) > 60 {
		runes = runes[:60]
	}
	return string(runes)
}

func userVisibleCLIWarning(stderr string) string {
	var warnings []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[permission]") || strings.HasPrefix(line, "[askuser]") {
			warnings = append(warnings, line)
		}
	}
	return strings.Join(warnings, "\n")
}

func normalizeApprovalMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "default", "acceptEdits", "plan", "dontAsk", "bypassPermissions", "fullAccess":
		return strings.TrimSpace(mode)
	case "auto", "accept":
		return "acceptEdits"
	case "ask":
		return "default"
	case "bypass":
		return "bypassPermissions"
	case "full":
		return "fullAccess"
	case "deny":
		return "dontAsk"
	default:
		return "acceptEdits"
	}
}

func (a *App) metisHome() string {
	if home := os.Getenv("METIS_HOME"); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".metis")
}

func validSessionID(id string) bool {
	trimmed := strings.TrimSpace(id)
	if id != trimmed || id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func findMetisBinary(preferred string) (string, error) {
	if preferred = strings.TrimSpace(preferred); preferred != "" {
		if info, err := os.Stat(preferred); err == nil && executableMetisFile(preferred, info, runtime.GOOS) {
			return preferred, nil
		}
		return "", fmt.Errorf("launcher-provided Metis CLI is not executable: %s", preferred)
	}
	if override := strings.TrimSpace(os.Getenv("METIS_BIN")); override != "" {
		if info, err := os.Stat(override); err == nil && executableMetisFile(override, info, runtime.GOOS) {
			return override, nil
		}
		return "", fmt.Errorf("METIS_BIN does not point to an executable file: %s", override)
	}
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	for _, candidate := range []string{
		filepath.Join(cwd, "metis"),
		filepath.Join(cwd, "bin", "metis"),
		"/opt/homebrew/bin/metis",
		"/usr/local/bin/metis",
		filepath.Join(home, "go", "bin", "metis"),
		filepath.Join(home, ".local", "bin", "metis"),
	} {
		if info, err := os.Stat(candidate); err == nil && executableMetisFile(candidate, info, runtime.GOOS) {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath("metis"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("metis CLI not found; set METIS_BIN or install metis in PATH")
}

// Windows FileMode does not expose Unix executable bits. Require an .exe
// there; Unix platforms continue to require at least one execute bit.
func executableMetisFile(path string, info os.FileInfo, goos string) bool {
	if info == nil || info.IsDir() || !info.Mode().IsRegular() {
		return false
	}
	if goos == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	return info.Mode()&0o111 != 0
}

func (a *App) runCLI(timeout time.Duration, args ...string) (string, string, error) {
	binary, err := a.findMetis()
	if err != nil {
		return "", "", err
	}
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	return a.runMetis(ctx, binary, args, a.workDir)
}

func runMetisCommand(ctx context.Context, binary string, args []string, dir string) (string, string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

type DesktopSettings struct {
	Theme           string `json:"theme"`
	WorkMode        string `json:"workMode"`
	Language        string `json:"language"`
	FileOpenTarget  string `json:"fileOpenTarget"`
	DefaultPerms    bool   `json:"defaultPermissions"`
	AutoReview      bool   `json:"autoReview"`
	FullAccess      bool   `json:"fullAccess"`
	MarkdownEnabled bool   `json:"markdownEnabled"`
	ShowTokens      bool   `json:"showTokens"`
}

func (a *App) settingsPath() string {
	return filepath.Join(a.metisHome(), "desktop-settings.json")
}

func (a *App) GetSettings() string {
	data, err := os.ReadFile(a.settingsPath())
	if err != nil {
		defaults := DesktopSettings{
			Theme:           "dark",
			WorkMode:        "coding",
			Language:        "en",
			FileOpenTarget:  "terminal",
			DefaultPerms:    true,
			AutoReview:      true,
			FullAccess:      false,
			MarkdownEnabled: true,
			ShowTokens:      true,
		}
		out, _ := json.Marshal(defaults)
		return string(out)
	}
	return string(data)
}

func (a *App) SaveSettings(settingsJson string) error {
	var settings DesktopSettings
	if err := json.Unmarshal([]byte(settingsJson), &settings); err != nil {
		return fmt.Errorf("invalid desktop settings: %w", err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode desktop settings: %w", err)
	}
	if err := writeFileAtomic(a.settingsPath(), data, 0o644); err != nil {
		return fmt.Errorf("save desktop settings: %w", err)
	}
	return nil
}

// writeFileAtomic writes a complete temporary file in the destination
// directory and then renames it into place. A crash can therefore leave the
// previous settings or the new settings, but never a truncated JSON file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
