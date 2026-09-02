package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeCLICall struct {
	binary string
	args   []string
	dir    string
}

func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_METIS_WEBUI_START_HELPER") == "1" {
		os.Exit(runWebUIStartHelper(os.Args))
	}
	os.Exit(m.Run())
}

func runWebUIStartHelper(args []string) int {
	var port string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--port" {
			port = args[i+1]
			break
		}
	}
	if port == "" {
		fmt.Fprintln(os.Stderr, "simulated webui helper received no port")
		return 20
	}
	switch os.Getenv("METIS_WEBUI_HELPER_MODE") {
	case "fail-once":
		stateFile := os.Getenv("METIS_WEBUI_HELPER_STATE")
		file, err := os.OpenFile(stateFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_ = file.Close()
			fmt.Fprintln(os.Stderr, "simulated first webui startup failure")
			return 21
		}
	case "always-fail":
		fmt.Fprintln(os.Stderr, "simulated webui startup failure")
		return 22
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	shutdown := make(chan struct{}, 1)
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("X-Metis-Desktop-Token") != os.Getenv("METIS_DESKTOP_FRAME_TOKEN") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		shutdown <- struct{}{}
	})
	go func() {
		<-shutdown
		if delayText := os.Getenv("METIS_WEBUI_HELPER_SHUTDOWN_DELAY"); delayText != "" {
			if delay, err := time.ParseDuration(delayText); err == nil {
				time.Sleep(delay)
			}
		}
		if marker := os.Getenv("METIS_WEBUI_HELPER_STOPPED"); marker != "" {
			_ = os.WriteFile(marker, []byte("stopped"), 0o600)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "simulated webui listen failure:", err)
		return 23
	}
	return 0
}

func TestProcessLogBufferKeepsBoundedTail(t *testing.T) {
	buffer := &processLogBuffer{}
	input := strings.Repeat("x", webUIStartupLogLimit) + "important tail"
	written, err := buffer.Write([]byte(input))
	if err != nil || written != len(input) {
		t.Fatalf("Write() = %d, %v; want %d", written, err, len(input))
	}
	got := buffer.String()
	if len(got) != webUIStartupLogLimit || !strings.HasSuffix(got, "important tail") {
		t.Fatalf("bounded log length=%d suffix=%q", len(got), got[len(got)-20:])
	}
}

func TestStartWebUIRetriesTransientChildFailure(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "first-attempt")
	t.Setenv("GO_WANT_METIS_WEBUI_START_HELPER", "1")
	t.Setenv("METIS_WEBUI_HELPER_MODE", "fail-once")
	t.Setenv("METIS_WEBUI_HELPER_STATE", stateFile)
	app := &App{
		workDir: t.TempDir(),
		findMetis: func() (string, error) {
			return os.Args[0], nil
		},
	}
	url, err := app.StartWebUI()
	if err != nil {
		t.Fatalf("StartWebUI() failed after a transient child exit: %v", err)
	}
	t.Cleanup(app.stopWebUI)
	if !strings.Contains(url, "desktop-frame=") {
		t.Fatalf("StartWebUI() URL = %q, want a tokenized frame URL", url)
	}
	response, err := http.Get(app.webUIBaseURL() + "/api/health")
	if err != nil {
		t.Fatalf("health probe after retry: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status after retry = %d, want 200", response.StatusCode)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("first attempt marker: %v", err)
	}
}

func TestStartWebUIReportsChildOutputAfterRetriesFail(t *testing.T) {
	t.Setenv("GO_WANT_METIS_WEBUI_START_HELPER", "1")
	t.Setenv("METIS_WEBUI_HELPER_MODE", "always-fail")
	app := &App{
		workDir: t.TempDir(),
		findMetis: func() (string, error) {
			return os.Args[0], nil
		},
	}
	_, err := app.StartWebUI()
	if err == nil {
		t.Fatal("StartWebUI() unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "attempt 2/2") || !strings.Contains(err.Error(), "simulated webui startup failure") {
		t.Fatalf("StartWebUI() error = %q, want retry count and child stderr", err)
	}
}

func TestShutdownStopsWebUIChild(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestWebUIChildHelper$")
	cmd.Env = append(os.Environ(), "GO_WANT_METIS_WEBUI_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		close(done)
	}()
	app := &App{webuiCmd: cmd, webuiDone: done, webuiPort: 49123}

	app.shutdown(context.Background())

	if cmd.ProcessState == nil {
		t.Fatal("web UI child was not reaped after desktop shutdown")
	}
	if app.webuiCmd != nil || app.webuiDone != nil || app.webuiPort != 0 {
		t.Fatalf("web UI state not cleared: cmd=%v done=%v port=%d", app.webuiCmd, app.webuiDone, app.webuiPort)
	}
}

func TestStopWebUIAllowsBackendGracefulCleanup(t *testing.T) {
	dir := t.TempDir()
	stopped := filepath.Join(dir, "stopped")
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	token := "test-native-shutdown-token"
	cmd := exec.Command(os.Args[0], "desktop", "--web", "--port", strconv.Itoa(port))
	cmd.Env = append(os.Environ(),
		"GO_WANT_METIS_WEBUI_START_HELPER=1",
		"METIS_DESKTOP_FRAME_TOKEN="+token,
		"METIS_WEBUI_HELPER_STOPPED="+stopped,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, probeErr := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/health")
		if probeErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("graceful child did not become ready")
	}

	// The native control channel must remain local even when the user's shell
	// exports a proxy that would reject the request.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	app := &App{webuiCmd: cmd, webuiDone: done, webuiPort: port, webuiToken: token}
	app.stopWebUI()

	if _, err := os.Stat(stopped); err != nil {
		t.Fatalf("backend cleanup marker missing; desktop likely killed it before Cleanup: %v", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("backend was not reaped after graceful exit: state=%v", cmd.ProcessState)
	}
}

func TestStopWebUIWaitsForAcceptedDelayedCleanup(t *testing.T) {
	dir := t.TempDir()
	stopped := filepath.Join(dir, "stopped-after-delay")
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	token := "test-delayed-native-shutdown-token"
	cmd := exec.Command(os.Args[0], "desktop", "--web", "--port", strconv.Itoa(port))
	cmd.Env = append(os.Environ(),
		"GO_WANT_METIS_WEBUI_START_HELPER=1",
		"METIS_DESKTOP_FRAME_TOKEN="+token,
		"METIS_WEBUI_HELPER_STOPPED="+stopped,
		"METIS_WEBUI_HELPER_SHUTDOWN_DELAY=120ms",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		close(done)
	}()
	waitForHelperHealth(t, port, cmd, done)

	stopWebUIProcessWithPolicy(cmd, done, port, token, webUIStopPolicy{
		shutdownGrace: 3 * time.Second,
		signalGrace:   100 * time.Millisecond,
		killWait:      100 * time.Millisecond,
	})

	if _, err := os.Stat(stopped); err != nil {
		t.Fatalf("delayed cleanup marker missing; accepted shutdown was interrupted too early: %v", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("delayed backend was not allowed to exit cleanly: state=%v", cmd.ProcessState)
	}
}

func waitForHelperHealth(t *testing.T, port int, cmd *exec.Cmd, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-done:
			t.Fatalf("helper exited before health check: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	<-done
	t.Fatal("helper did not become ready")
}

func TestWebUIChildHelper(t *testing.T) {
	if os.Getenv("GO_WANT_METIS_WEBUI_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}

type fakeCLIResult struct {
	stdout string
	stderr string
	err    error
}

type fakeCLI struct {
	t       *testing.T
	results []fakeCLIResult
	calls   []fakeCLICall
}

func (f *fakeCLI) runner(_ context.Context, binary string, args []string, dir string) (string, string, error) {
	f.t.Helper()
	f.calls = append(f.calls, fakeCLICall{binary: binary, args: append([]string(nil), args...), dir: dir})
	if len(f.results) == 0 {
		f.t.Fatalf("unexpected CLI call: %s %#v", binary, args)
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.stdout, result.stderr, result.err
}

func newFakeApp(t *testing.T, workDir string, results ...fakeCLIResult) (*App, *fakeCLI) {
	t.Helper()
	fake := &fakeCLI{t: t, results: append([]fakeCLIResult(nil), results...)}
	app := &App{
		workDir: workDir,
		findMetis: func() (string, error) {
			return "/fake/bin/metis", nil
		},
		runMetis: fake.runner,
	}
	return app, fake
}

func TestBundleMetadataUsesMetisDisplayNameAndSingleInstance(t *testing.T) {
	data, err := os.ReadFile("wails.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Name           string `json:"name"`
		OutputFilename string `json:"outputfilename"`
		Info           struct {
			ProductName string `json:"productName"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.Name != "metis-desktop" || config.OutputFilename != "metis-desktop" {
		t.Fatalf("internal bundle names = name:%q output:%q, release-compatible names must stay metis-desktop", config.Name, config.OutputFilename)
	}
	if config.Info.ProductName != "Metis" {
		t.Fatalf("product display name = %q, want Metis", config.Info.ProductName)
	}
	plist, err := os.ReadFile(filepath.Join("build", "darwin", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	metadata := string(plist)
	for _, want := range []string{
		"<key>CFBundleDisplayName</key>\n        <string>Metis</string>",
		"<key>LSMultipleInstancesProhibited</key>\n        <true/>",
	} {
		if !strings.Contains(metadata, want) {
			t.Fatalf("Info.plist missing %q", want)
		}
	}
}

func TestParseDesktopLaunchArguments(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	for _, tc := range []struct {
		name        string
		args        []string
		wantBin     string
		wantWorkdir string
	}{
		{
			name: "separate", args: []string{"metis-desktop", "--workspace", workspace, "--metis-bin", "/opt/metis"},
			wantBin: "/opt/metis", wantWorkdir: workspace,
		},
		{
			name: "equals", args: []string{"metis-desktop", "--metis-bin=/custom/metis", "--workspace=" + workspace},
			wantBin: "/custom/metis", wantWorkdir: workspace,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseMetisBinArg(tc.args); got != tc.wantBin {
				t.Fatalf("parseMetisBinArg() = %q, want %q", got, tc.wantBin)
			}
			if got := parseWorkspaceArg(tc.args); got != tc.wantWorkdir {
				t.Fatalf("parseWorkspaceArg() = %q, want %q", got, tc.wantWorkdir)
			}
		})
	}
	if got := parseMetisBinArg([]string{"metis-desktop", "--metis-bin"}); got != "" {
		t.Fatalf("missing --metis-bin value = %q, want empty", got)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := parseWorkspaceArg([]string{"metis-desktop"}); got != cwd {
		t.Fatalf("workspace fallback = %q, want cwd %q", got, cwd)
	}
}

func TestDesktopFrameURLCarriesFreshLaunchToken(t *testing.T) {
	tokenA, err := newDesktopFrameToken()
	if err != nil {
		t.Fatal(err)
	}
	tokenB, err := newDesktopFrameToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokenA) != 48 || tokenA == tokenB {
		t.Fatalf("frame tokens = %q and %q", tokenA, tokenB)
	}
	app := &App{webuiPort: 49123, webuiToken: tokenA}
	want := "http://127.0.0.1:49123/?desktop-frame=" + tokenA
	if got := app.webUIURL(); got != want {
		t.Fatalf("webUIURL() = %q, want %q", got, want)
	}
}

func TestNormalizeApprovalMode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"auto", "acceptEdits"}, {"acceptEdits", "acceptEdits"}, {"ask", "default"},
		{"plan", "plan"}, {"deny", "dontAsk"}, {"bypass", "bypassPermissions"},
		{"fullAccess", "fullAccess"}, {"full", "fullAccess"},
		{"unexpected", "acceptEdits"}, {"", "acceptEdits"},
	} {
		if got := normalizeApprovalMode(tc.in); got != tc.want {
			t.Errorf("normalizeApprovalMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSendMessageNewSessionCLIArgs(t *testing.T) {
	workDir := t.TempDir()
	app, fake := newFakeApp(t, workDir, fakeCLIResult{stdout: "  completed\n"})
	response := app.SendMessage("  implement desktop flow  ", "", "auto")
	if response.Error != "" || response.Text != "completed" {
		t.Fatalf("response = %+v", response)
	}
	if !strings.HasPrefix(response.ThreadID, "desktop-") || !validSessionID(response.ThreadID) {
		t.Fatalf("generated session id = %q", response.ThreadID)
	}
	want := []string{
		"run", "--no-auth-wizard",
		"--session-id", response.ThreadID,
		"--name", "implement desktop flow",
		"--mode", "acceptEdits",
		"--", "implement desktop flow",
	}
	assertSingleCall(t, fake, workDir, want)
}

func TestSendMessageExistingSessionCLIArgs(t *testing.T) {
	workDir := t.TempDir()
	app, fake := newFakeApp(t, workDir, fakeCLIResult{stdout: "continued"})
	response := app.SendMessage("continue", "session_123", "plan")
	if response.Error != "" || response.ThreadID != "session_123" || response.Text != "continued" {
		t.Fatalf("response = %+v", response)
	}
	want := []string{
		"run", "--no-auth-wizard", "--resume", "session_123",
		"--mode", "plan", "--", "continue",
	}
	assertSingleCall(t, fake, workDir, want)
}

func TestSendMessageFullAccessCLIArgs(t *testing.T) {
	workDir := t.TempDir()
	app, fake := newFakeApp(t, workDir, fakeCLIResult{stdout: "continued"})
	response := app.SendMessage("continue", "session_full", "fullAccess")
	if response.Error != "" || response.ThreadID != "session_full" {
		t.Fatalf("response = %+v", response)
	}
	assertSingleCall(t, fake, workDir, []string{
		"run", "--no-auth-wizard", "--resume", "session_full",
		"--mode", "fullAccess", "--", "continue",
	})
}

func TestGetSessionsParsesJSONAndFiltersWorkspaceAndIDs(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "repo")
	stdout := `[
		{"id":"current-1","title":"Sprint","model":"m1","workDir":"` + workDir + `","createdAt":"2026-07-14T01:02:03Z"},
		{"id":"untitled","title":"","model":"m2","workDir":"` + filepath.Join(workDir, ".") + `","createdAt":"now"},
		{"id":"legacy-no-workdir","title":"Legacy","model":"m3","createdAt":"then"},
		{"id":"other-repo","title":"Other","model":"m4","workDir":"` + t.TempDir() + `"},
		{"id":"../escape","title":"Bad","model":"m5","workDir":"` + workDir + `"},
		{"id":"bad id","title":"Bad","model":"m6","workDir":"` + workDir + `"}
	]`
	app, fake := newFakeApp(t, workDir, fakeCLIResult{stdout: stdout})
	got := app.GetSessions()
	want := []SessionInfo{
		{ID: "current-1", Title: "Sprint", Model: "m1", CreatedAt: "2026-07-14T01:02:03Z"},
		{ID: "untitled", Title: "Untitled", Model: "m2", CreatedAt: "now"},
		{ID: "legacy-no-workdir", Title: "Legacy", Model: "m3", CreatedAt: "then"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetSessions() = %#v, want %#v", got, want)
	}
	assertSingleCall(t, fake, workDir, []string{"sessions", "list", "--json", "--work-dir", workDir, "--limit", "30"})
}

func TestGetSessionsDefensivelyCapsFilteredResults(t *testing.T) {
	workDir := t.TempDir()
	var records []map[string]string
	for i := 0; i < 35; i++ {
		records = append(records, map[string]string{
			"id":      fmt.Sprintf("session-%02d", i),
			"title":   fmt.Sprintf("Session %d", i),
			"workDir": workDir,
		})
	}
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := newFakeApp(t, workDir, fakeCLIResult{stdout: string(data)})
	if got := app.GetSessions(); len(got) != 30 {
		t.Fatalf("GetSessions() returned %d records, want 30", len(got))
	}
}

func TestSamePathResolvesWorkspaceSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if !samePath(realDir, linkDir) {
		t.Fatalf("real path %q and symlink %q should match", realDir, linkDir)
	}
}

func TestGetSessionsInvalidJSONAndCLIErrorReturnNil(t *testing.T) {
	for _, result := range []fakeCLIResult{{stdout: "not-json"}, {err: errors.New("exit 1")}} {
		app, _ := newFakeApp(t, t.TempDir(), result)
		if got := app.GetSessions(); got != nil {
			t.Fatalf("GetSessions() = %#v, want nil", got)
		}
	}
}

func TestGetScheduledTasksUsesCronListJSON(t *testing.T) {
	workDir := t.TempDir()
	stdout := `[
		{"id":"job-1","name":"Daily review","prompt":"review changes","schedule":{"kind":"cron","cron_expr":"0 9 * * *","tz":"Asia/Shanghai","jitter_ms":30000},"enabled":true,"paused":false,"nextRun":"2026-07-16T01:00:00Z","lastRun":"2026-07-15T01:00:00Z","runCount":3,"repeat":0,"silent":true,"sessionMode":"persistent"},
		{"id":"job-2","name":"","prompt":"health check","schedule":{"kind":"every","every_ms":300000},"enabled":true,"paused":true,"runCount":1,"repeat":5,"sessionMode":"isolated"},
		{"id":"../escape","name":"Bad","schedule":{"kind":"every","every_ms":1000}}
	]`
	app, fake := newFakeApp(t, workDir, fakeCLIResult{stdout: stdout})
	got, err := app.GetScheduledTasks()
	if err != nil {
		t.Fatal(err)
	}
	want := []ScheduledTaskInfo{
		{
			ID: "job-1", Name: "Daily review", Prompt: "review changes",
			Schedule: "0 9 * * * · Asia/Shanghai · jitter 30s", Enabled: true,
			NextRun: "2026-07-16T01:00:00Z", LastRun: "2026-07-15T01:00:00Z",
			RunCount: 3, Silent: true, SessionMode: "persistent",
		},
		{
			ID: "job-2", Name: "Untitled task", Prompt: "health check",
			Schedule: "Every 5m0s", Enabled: true, Paused: true,
			RunCount: 1, Repeat: 5, SessionMode: "isolated",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetScheduledTasks() = %#v, want %#v", got, want)
	}
	assertSingleCall(t, fake, workDir, []string{"cron", "list", "--json"})
}

func TestGetScheduledTasksReturnsDecodeAndCLIErrors(t *testing.T) {
	app, _ := newFakeApp(t, t.TempDir(), fakeCLIResult{stdout: "not-json"})
	if _, err := app.GetScheduledTasks(); err == nil || !strings.Contains(err.Error(), "decode CLI response") {
		t.Fatalf("decode error = %v", err)
	}

	app, _ = newFakeApp(t, t.TempDir(), fakeCLIResult{stderr: "config unavailable", err: errors.New("exit status 1")})
	if _, err := app.GetScheduledTasks(); err == nil || !strings.Contains(err.Error(), "config unavailable") {
		t.Fatalf("CLI error = %v", err)
	}
}

func TestScheduledTaskActionsUseCronCLI(t *testing.T) {
	workDir := t.TempDir()
	app, fake := newFakeApp(t, workDir,
		fakeCLIResult{},
		fakeCLIResult{},
		fakeCLIResult{},
		fakeCLIResult{stdout: "task result\n"},
	)
	if err := app.PauseScheduledTask("job-1"); err != nil {
		t.Fatal(err)
	}
	if err := app.ResumeScheduledTask("job-1"); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteScheduledTask("job-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := app.RunScheduledTask("job-1"); err != nil || got != "task result" {
		t.Fatalf("RunScheduledTask() = %q, %v", got, err)
	}
	want := [][]string{
		{"cron", "pause", "job-1"},
		{"cron", "resume", "job-1"},
		{"cron", "rm", "job-1"},
		{"cron", "run", "job-1"},
	}
	if len(fake.calls) != len(want) {
		t.Fatalf("calls = %#v", fake.calls)
	}
	for i := range want {
		if fake.calls[i].binary != "/fake/bin/metis" || fake.calls[i].dir != workDir || !reflect.DeepEqual(fake.calls[i].args, want[i]) {
			t.Fatalf("call %d = %#v, want args %#v", i, fake.calls[i], want[i])
		}
	}
}

func TestScheduledTaskActionsRejectIDsAndSurfaceStderr(t *testing.T) {
	app, fake := newFakeApp(t, t.TempDir())
	for _, id := range []string{"", "..", "../escape", `..\\escape`, "bad id", " job-1 "} {
		if err := app.PauseScheduledTask(id); err == nil || !strings.Contains(err.Error(), "invalid job id") {
			t.Errorf("PauseScheduledTask(%q) error = %v", id, err)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("invalid IDs reached CLI: %#v", fake.calls)
	}

	app, _ = newFakeApp(t, t.TempDir(), fakeCLIResult{stdout: "partial", stderr: "provider unavailable", err: errors.New("exit status 1")})
	if got, err := app.RunScheduledTask("job-1"); got != "partial" || err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("RunScheduledTask error = %q, %v", got, err)
	}
}

func TestGetSessionMessagesUsesExportJSONL(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"header","header":{"id":"desktop-history"}}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image","data":"ignored"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","text":"secret"},{"type":"text","text":"first"},{"type":"text","text":"second"}]}}`,
		`{"type":"message","message":{"role":"tool","content":[{"type":"text","text":"tool output"}]}}`,
		`not-json`,
	}, "\n")
	workDir := t.TempDir()
	app, fake := newFakeApp(t, workDir, fakeCLIResult{stdout: stdout})
	want := []ChatMessage{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "first\nsecond"}}
	if got := app.GetSessionMessages("desktop-history"); !reflect.DeepEqual(got, want) {
		t.Fatalf("GetSessionMessages() = %#v, want %#v", got, want)
	}
	assertSingleCall(t, fake, workDir, []string{"sessions", "export", "desktop-history"})
}

func TestGetSessionMessagesRejectsMaliciousIDBeforeCLI(t *testing.T) {
	app, fake := newFakeApp(t, t.TempDir())
	for _, id := range []string{"", ".", "..", "../escape", `..\\escape`, "bad id", "bad;$id", " session-1 "} {
		if got := app.GetSessionMessages(id); got != nil {
			t.Errorf("GetSessionMessages(%q) = %#v, want nil", id, got)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("malicious IDs reached CLI: %#v", fake.calls)
	}
}

func TestGetCurrentModelUsesConfigShowJSON(t *testing.T) {
	workDir := t.TempDir()
	app, fake := newFakeApp(t, workDir, fakeCLIResult{stdout: `{"model":"claude-sonnet-4-6"}`})
	if got := app.GetCurrentModel(); got != "claude-sonnet-4-6" {
		t.Fatalf("GetCurrentModel() = %q", got)
	}
	assertSingleCall(t, fake, workDir, []string{"config", "show", "--json"})

	for _, result := range []fakeCLIResult{{stdout: `{}`}, {stdout: `bad-json`}, {err: errors.New("exit 1")}} {
		app, _ := newFakeApp(t, workDir, result)
		if got := app.GetCurrentModel(); got != "unknown" {
			t.Fatalf("GetCurrentModel() = %q, want unknown", got)
		}
	}
}

func TestSendMessageSurfacesWarningsAndErrors(t *testing.T) {
	app, _ := newFakeApp(t, t.TempDir(), fakeCLIResult{
		stdout: "answer",
		stderr: "debug line\n[permission] Bash denied\n[askuser] dismissed\nmetrics line\n",
	})
	response := app.SendMessage("hello", "session-1", "ask")
	if response.Error != "" || response.Warning != "[permission] Bash denied\n[askuser] dismissed" {
		t.Fatalf("warning response = %+v", response)
	}

	app, _ = newFakeApp(t, t.TempDir(), fakeCLIResult{stdout: "partial", stderr: "provider unavailable", err: errors.New("exit status 1")})
	response = app.SendMessage("hello", "session-2", "ask")
	if response.Text != "partial" || response.Error != "provider unavailable" || response.ThreadID != "session-2" {
		t.Fatalf("stderr error response = %+v", response)
	}

	app, _ = newFakeApp(t, t.TempDir(), fakeCLIResult{err: errors.New("signal: killed")})
	response = app.SendMessage("hello", "session-3", "ask")
	if response.Error != "signal: killed" {
		t.Fatalf("fallback error response = %+v", response)
	}
}

func TestSendMessageDropsUnpersistedNewSessionIDAfterEarlyFailure(t *testing.T) {
	workDir := t.TempDir()
	app, fake := newFakeApp(t, workDir,
		fakeCLIResult{stderr: "provider configuration is invalid", err: errors.New("exit status 1")},
		fakeCLIResult{stderr: "session not found", err: errors.New("exit status 1")},
	)
	response := app.SendMessage("hello", "", "ask")
	if response.ThreadID != "" || response.Error != "provider configuration is invalid" {
		t.Fatalf("response = %+v", response)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("CLI calls = %#v, want run followed by export probe", fake.calls)
	}
	generatedID := fake.calls[0].args[3]
	if got := fake.calls[1].args; !reflect.DeepEqual(got, []string{"sessions", "export", generatedID}) {
		t.Fatalf("persistence probe args = %#v", got)
	}
}

func TestSendMessageKeepsPersistedNewSessionIDAfterProviderFailure(t *testing.T) {
	workDir := t.TempDir()
	app, fake := newFakeApp(t, workDir,
		fakeCLIResult{stderr: "provider unavailable", err: errors.New("exit status 1")},
		fakeCLIResult{stdout: "{\"type\":\"header\"}\n"},
	)
	response := app.SendMessage("hello", "", "ask")
	if response.ThreadID == "" || response.Error != "provider unavailable" {
		t.Fatalf("response = %+v", response)
	}
	if got := fake.calls[1].args; !reflect.DeepEqual(got, []string{"sessions", "export", response.ThreadID}) {
		t.Fatalf("persistence probe args = %#v", got)
	}
}

func TestSendMessageRejectsInvalidInputAndMissingCLI(t *testing.T) {
	runCalled := false
	findCalled := false
	app := &App{
		findMetis: func() (string, error) {
			findCalled = true
			return "/fake/metis", nil
		},
		runMetis: func(context.Context, string, []string, string) (string, string, error) {
			runCalled = true
			return "", "", nil
		},
	}
	if response := app.SendMessage("hello", "../../outside", "ask"); response.Error != "invalid session id" {
		t.Fatalf("invalid ID response = %+v", response)
	}
	if response := app.SendMessage("  ", "session-1", "ask"); response.Error != "message is empty" {
		t.Fatalf("empty input response = %+v", response)
	}
	if findCalled || runCalled {
		t.Fatalf("invalid input reached CLI: find=%v run=%v", findCalled, runCalled)
	}

	app = &App{
		findMetis: func() (string, error) { return "", errors.New("CLI missing") },
		runMetis: func(context.Context, string, []string, string) (string, string, error) {
			t.Fatal("runner must not run when CLI lookup fails")
			return "", "", nil
		},
	}
	if response := app.SendMessage("hello", "", "ask"); response.Error != "CLI missing" || response.ThreadID != "" {
		t.Fatalf("missing CLI response = %+v", response)
	}
}

func TestFindMetisBinaryRejectsNonExecutableOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metis")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	if err := os.WriteFile(path, []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		invalid := filepath.Join(dir, "metis")
		if err := os.WriteFile(invalid, []byte("binary"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("METIS_BIN", invalid)
		if _, err := findMetisBinary(""); err == nil || !strings.Contains(err.Error(), "executable") {
			t.Fatalf("extensionless METIS_BIN error = %v", err)
		}
		t.Setenv("METIS_BIN", path)
		if got, err := findMetisBinary(""); err != nil || got != path {
			t.Fatalf("Windows METIS_BIN = %q, %v", got, err)
		}
		return
	}
	t.Setenv("METIS_BIN", path)
	if _, err := findMetisBinary(""); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("non-executable METIS_BIN error = %v", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := findMetisBinary(""); err != nil || got != path {
		t.Fatalf("executable METIS_BIN = %q, %v", got, err)
	}
}

func TestExecutableMetisFileUsesWindowsExtensionInsteadOfUnixBits(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "metis.EXE")
	if err := os.WriteFile(exe, []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !executableMetisFile(exe, info, "windows") {
		t.Fatal("Windows .exe should not require Unix executable bits")
	}
	if executableMetisFile(dir, mustStat(t, dir), "windows") {
		t.Fatal("directory was accepted as an executable")
	}
	if runtime.GOOS == "windows" {
		return
	}
	if executableMetisFile(exe, info, "darwin") {
		t.Fatal("Unix file without executable bits was accepted")
	}
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !executableMetisFile(exe, info, "darwin") {
		t.Fatal("Unix executable file was rejected")
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestSaveSettingsAtomicallyWritesAndOverwritesJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	app := &App{}

	first := `{"theme":"dark","workMode":"coding","language":"en","fileOpenTarget":"terminal","defaultPermissions":true,"autoReview":true,"fullAccess":false,"markdownEnabled":true,"showTokens":true}`
	if err := app.SaveSettings(first); err != nil {
		t.Fatalf("first SaveSettings() error = %v", err)
	}
	second := `{"theme":"light","workMode":"daily","language":"zh","fileOpenTarget":"editor","defaultPermissions":false,"autoReview":false,"fullAccess":false,"markdownEnabled":false,"showTokens":false}`
	if err := app.SaveSettings(second); err != nil {
		t.Fatalf("second SaveSettings() error = %v", err)
	}
	data, err := os.ReadFile(app.settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	var got DesktopSettings
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("saved settings are invalid JSON: %v\n%s", err, data)
	}
	if got.Theme != "light" || got.WorkMode != "daily" || got.Language != "zh" || got.DefaultPerms || got.AutoReview || got.MarkdownEnabled || got.ShowTokens {
		t.Fatalf("saved settings = %+v", got)
	}
	matches, err := filepath.Glob(filepath.Join(home, ".desktop-settings.json-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary settings files left behind: %#v", matches)
	}
}

func TestSaveSettingsReturnsActionableErrors(t *testing.T) {
	app := &App{}
	if err := app.SaveSettings("not-json"); err == nil || !strings.Contains(err.Error(), "invalid desktop settings") {
		t.Fatalf("invalid JSON error = %v", err)
	}

	homeFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(homeFile, []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", homeFile)
	if err := app.SaveSettings(`{"theme":"dark"}`); err == nil || !strings.Contains(err.Error(), "save desktop settings") {
		t.Fatalf("filesystem error = %v", err)
	}
}

func TestChooseWorkspaceDirectoryUsesNativePicker(t *testing.T) {
	current := t.TempDir()
	selected := t.TempDir()
	var gotDefault string
	app := &App{
		ctx:     context.Background(),
		workDir: current,
		chooseWorkspace: func(_ context.Context, defaultDir string) (string, error) {
			gotDefault = defaultDir
			return selected, nil
		},
	}
	got, err := app.ChooseWorkspaceDirectory()
	if err != nil || got != selected {
		t.Fatalf("ChooseWorkspaceDirectory() = %q, %v", got, err)
	}
	if gotDefault != app.workDir {
		t.Fatalf("native picker default = %q, want %q", gotDefault, app.workDir)
	}
}

func TestGetUpdateStatusUsesDesktopVersion(t *testing.T) {
	var gotVersion string
	app := &App{
		ctx: context.Background(),
		checkDesktopUpdate: func(_ context.Context, current string) (DesktopUpdateStatus, error) {
			gotVersion = current
			return DesktopUpdateStatus{CurrentVersion: current, LatestVersion: "9.9.9", Available: true, CanUpdate: true}, nil
		},
	}
	status, err := app.GetUpdateStatus()
	if err != nil || !status.Available || !status.CanUpdate {
		t.Fatalf("GetUpdateStatus() = %+v, %v", status, err)
	}
	if gotVersion != app.GetVersion() {
		t.Fatalf("checked version = %q, want %q", gotVersion, app.GetVersion())
	}
}

func TestInstallUpdateAndRestartUpdatesCLIThenDesktop(t *testing.T) {
	workDir := t.TempDir()
	appPath := filepath.Join(workDir, "Metis.app")
	fake := &fakeCLI{t: t, results: []fakeCLIResult{{stdout: "installed"}}}
	var installedPath, restartedPath, restartedWorkspace, restartedCLI, resolvedCLI string
	quitCalled := false
	app := &App{
		ctx:      context.Background(),
		workDir:  workDir,
		metisBin: "/fake/bin/metis",
		findMetis: func() (string, error) {
			return "/fake/bin/metis", nil
		},
		runMetis: fake.runner,
		checkDesktopUpdate: func(_ context.Context, current string) (DesktopUpdateStatus, error) {
			return DesktopUpdateStatus{CurrentVersion: current, LatestVersion: "9.9.9", Available: true, CanUpdate: true}, nil
		},
		installDesktopUpdate: func(_ context.Context, current, path string) (DesktopUpdateStatus, error) {
			installedPath = path
			return DesktopUpdateStatus{CurrentVersion: current, LatestVersion: "9.9.9", Available: false, CanUpdate: true, Installed: true}, nil
		},
		desktopPath: func() (string, error) { return appPath, nil },
		restartDesktop: func(path, workspace, cli string) error {
			restartedPath, restartedWorkspace, restartedCLI = path, workspace, cli
			return nil
		},
		resolveUpdatedMetis: func(current string) (string, error) {
			resolvedCLI = current
			return "/stable/bin/metis", nil
		},
		scheduleRestart: func(fn func()) { fn() },
		quit:            func(context.Context) { quitCalled = true },
	}

	status, err := app.InstallUpdateAndRestart()
	if err != nil || !status.Installed {
		t.Fatalf("InstallUpdateAndRestart() = %+v, %v", status, err)
	}
	assertSingleCall(t, fake, workDir, []string{"update"})
	if installedPath != appPath {
		t.Fatalf("installed desktop path = %q, want %q", installedPath, appPath)
	}
	if resolvedCLI != "/fake/bin/metis" || restartedPath != appPath || restartedWorkspace != workDir || restartedCLI != "/stable/bin/metis" || !quitCalled {
		t.Fatalf("restart = path:%q workspace:%q cli:%q quit:%v", restartedPath, restartedWorkspace, restartedCLI, quitCalled)
	}
}

func TestResolveStableMetisBinaryRebindsManagedVersionPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix managed layout regression")
	}
	root := t.TempDir()
	oldBinary := filepath.Join(root, "share", "metis", "versions", "0.4.28", "metis")
	newBinary := filepath.Join(root, "share", "metis", "versions", "0.4.29", "metis")
	stable := filepath.Join(root, "bin", "metis")
	for _, binary := range []string{oldBinary, newBinary} {
		if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(binary, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(stable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newBinary, stable); err != nil {
		t.Fatal(err)
	}

	got, err := resolveStableMetisBinary(oldBinary)
	if err != nil {
		t.Fatal(err)
	}
	if got != stable {
		t.Fatalf("resolveStableMetisBinary() = %q, want stable launcher %q", got, stable)
	}
}

func TestStableMetisLauncherForVersionRejectsLookalikeLayout(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, goos, current, want string
		managed                   bool
	}{
		{
			name:    "unix managed",
			goos:    "darwin",
			current: filepath.Join(root, "share", "metis", "versions", "1.2.3", "metis"),
			want:    filepath.Join(root, "bin", "metis"),
			managed: true,
		},
		{
			name:    "windows managed",
			goos:    "windows",
			current: filepath.Join(root, "versions", "1.2.3", "metis.exe"),
			want:    filepath.Join(root, "bin", "metis.exe"),
			managed: true,
		},
		{
			name:    "invalid version",
			goos:    "linux",
			current: filepath.Join(root, "share", "metis", "versions", "not-a-version", "metis"),
		},
		{
			name:    "unrelated versions tree",
			goos:    "linux",
			current: filepath.Join(root, "unrelated", "versions", "1.2.3", "metis"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, managed := stableMetisLauncherForVersion(tc.current, tc.goos)
			if got != tc.want || managed != tc.managed {
				t.Fatalf("stableMetisLauncherForVersion() = %q, %v; want %q, %v", got, managed, tc.want, tc.managed)
			}
		})
	}
}

func TestInstallUpdateAndRestartDoesNothingWhenCurrent(t *testing.T) {
	app := &App{
		ctx: context.Background(),
		checkDesktopUpdate: func(_ context.Context, current string) (DesktopUpdateStatus, error) {
			return DesktopUpdateStatus{CurrentVersion: current, LatestVersion: current, CanUpdate: true}, nil
		},
		findMetis: func() (string, error) {
			t.Fatal("CLI must not run without an available update")
			return "", nil
		},
	}
	if _, err := app.InstallUpdateAndRestart(); err == nil || !strings.Contains(err.Error(), "already up to date") {
		t.Fatalf("InstallUpdateAndRestart() error = %v", err)
	}
}

func assertSingleCall(t *testing.T, fake *fakeCLI, workDir string, wantArgs []string) {
	t.Helper()
	if len(fake.calls) != 1 {
		t.Fatalf("CLI calls = %#v, want one", fake.calls)
	}
	call := fake.calls[0]
	if call.binary != "/fake/bin/metis" || call.dir != workDir || !reflect.DeepEqual(call.args, wantArgs) {
		t.Fatalf("CLI call = %#v, want binary=%q dir=%q args=%#v", call, "/fake/bin/metis", workDir, wantArgs)
	}
}
