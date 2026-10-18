package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startUpdateLifecycleBackend(t *testing.T, delay string) *App {
	t.Helper()
	t.Setenv("GO_WANT_METIS_WEBUI_START_HELPER", "1")
	t.Setenv("METIS_WEBUI_HELPER_MODE", "")
	t.Setenv("METIS_WEBUI_HELPER_SHUTDOWN_DELAY", delay)
	app := &App{workDir: t.TempDir(), findMetis: func() (string, error) { return os.Args[0], nil }}
	if _, err := app.StartWebUI(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.stopWebUI)
	return app
}

func TestDesktopUpdateQuiesceWaitsForChildExitAndBlocksAdmission(t *testing.T) {
	stopped := filepath.Join(t.TempDir(), "persisted")
	t.Setenv("METIS_WEBUI_HELPER_STOPPED", stopped)
	app := startUpdateLifecycleBackend(t, "80ms")
	app.webuiMu.Lock()
	done := app.webuiDone
	app.webuiMu.Unlock()
	started := time.Now()
	if err := app.quiesceBackendForUpdate(context.Background(), "old.app"); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 70*time.Millisecond {
		t.Fatal("update continued before backend drain")
	}
	if !waitForWebUIProcess(done, 0) {
		t.Fatal("backend was not reaped before activation")
	}
	if _, err := os.Stat(stopped); err != nil {
		t.Fatalf("backend persistence not completed: %v", err)
	}
	if _, err := app.StartWebUI(); err == nil {
		t.Fatal("update gate started another backend")
	}
	if _, _, err := app.runCLI(time.Second, "query"); err == nil {
		t.Fatal("update gate admitted a native CLI command")
	}
	if response := app.SendMessage("new work", "", ""); response.Error == "" {
		t.Fatal("update gate admitted a legacy native message")
	}
	var restarted, quit bool
	app.restartDesktop = func(path, workspace, binary string) error {
		if path != "old.app" || workspace != app.workDir || binary != "" {
			t.Fatalf("recovery target = %q %q %q", path, workspace, binary)
		}
		restarted = true
		return nil
	}
	app.scheduleRestart = func(fn func()) { fn() }
	app.quit = func(context.Context) { quit = true }
	if restarting, err := app.recoverDesktopAfterFailedUpdate(context.Background(), "old.app", ""); err != nil || !restarting || !restarted || !quit {
		t.Fatalf("failed installation did not restart usable old Desktop: restarting=%v restarted=%v quit=%v err=%v", restarting, restarted, quit, err)
	}
}

func TestDesktopUpdateRefusedDrainKeepsBackendAndReopensAdmission(t *testing.T) {
	app := startUpdateLifecycleBackend(t, "")
	app.requestUpdateShutdown = func(context.Context, int, string) (bool, error) { return false, nil }
	url := app.webUIBaseURL()
	if err := app.quiesceBackendForUpdate(context.Background(), "old.app"); err == nil {
		t.Fatal("refused drain allowed activation")
	}
	if restarting, err := app.recoverDesktopAfterFailedUpdate(context.Background(), "old.app", ""); err != nil || restarting {
		t.Fatalf("unnecessary recovery = %v %v", restarting, err)
	}
	response, err := http.Get(url + "/api/health")
	if err != nil {
		t.Fatalf("unchanged old backend unavailable: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("old backend status = %d", response.StatusCode)
	}
	if _, err := app.StartWebUI(); err != nil {
		t.Fatalf("recovery did not reopen native admission: %v", err)
	}
}

func TestDesktopUpdateAcceptedDrainTimeoutRequiresRecovery(t *testing.T) {
	app := startUpdateLifecycleBackend(t, "500ms")
	app.updateDrainTimeout = 15 * time.Millisecond
	if err := app.quiesceBackendForUpdate(context.Background(), "old.app"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain timeout = %v", err)
	}
	if _, err := app.StartWebUI(); err == nil {
		t.Fatal("closing child was reused after drain timeout")
	}
	if restarting, err := app.recoverDesktopAfterFailedUpdate(context.Background(), "old.app", ""); err == nil || restarting || !strings.Contains(err.Error(), "reopen") {
		t.Fatalf("unavailable recovery was hidden: %v %v", restarting, err)
	}
}

func TestDesktopUpdateQuiesceWaitsForNativeCalls(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	app := &App{findMetis: func() (string, error) { return "fake", nil }, runMetis: func(context.Context, string, []string, string) (string, string, error) {
		close(entered)
		<-release
		return "", "", nil
	}}
	go func() { _, _, err := app.runCLI(time.Second, "status"); finished <- err }()
	<-entered
	quiesced := make(chan error, 1)
	go func() { quiesced <- app.quiesceBackendForUpdate(context.Background(), "old.app") }()
	deadline := time.Now().Add(time.Second)
	for {
		app.webuiMu.Lock()
		updating := app.webuiUpdating
		app.webuiMu.Unlock()
		if updating {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("update never closed admission")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-quiesced:
		close(release)
		t.Fatalf("update passed in-flight native command: %v", err)
	default:
	}
	if _, _, err := app.runCLI(time.Second, "query"); err == nil {
		close(release)
		t.Fatal("native command entered while update draining")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-quiesced; err != nil {
		t.Fatal(err)
	}
}
