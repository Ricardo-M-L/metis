package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// quiesceBackendForUpdate runs only after the downloaded candidate has been
// verified. Closing the child rejects new turns before any application path is
// replaced; waiting for Wait's completion prevents an old process from spawning
// a child through a newly replaced bundle path.
func (a *App) quiesceBackendForUpdate(ctx context.Context, _ string) error {
	a.webuiMu.Lock()
	if a.webuiUpdating {
		a.webuiMu.Unlock()
		return errors.New("Desktop backend is already preparing an update")
	}
	a.webuiUpdating = true
	cmd, done, port, token := a.webuiCmd, a.webuiDone, a.webuiPort, a.webuiToken
	a.webuiMu.Unlock()

	limit := a.updateDrainTimeout
	if limit <= 0 {
		limit = webUIShutdownGrace
	}
	drainCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	a.webuiMu.Lock()
	a.webuiUpdateDrainDeadline, _ = drainCtx.Deadline()
	a.webuiMu.Unlock()
	// Native bindings also use the runtime. Admission and Add share webuiMu,
	// so no new call can enter after the update gate is set.
	callsDone := make(chan struct{})
	go func() {
		a.nativeCLICalls.Wait()
		close(callsDone)
	}()
	select {
	case <-callsDone:
	case <-drainCtx.Done():
		return fmt.Errorf("wait for native CLI commands before update: %w", drainCtx.Err())
	}
	if cmd == nil || cmd.Process == nil {
		// Even without a child, the current native process must not resume
		// against a newly activated release after a restart launch fails.
		a.markBackendUpdateNeedsRestart()
		return nil
	}
	if waitForWebUIProcess(done, 0) {
		a.markBackendUpdateNeedsRestart()
		return nil
	}
	request := a.requestUpdateShutdown
	if request == nil {
		request = requestBackendUpdateShutdown
	}
	accepted, err := request(drainCtx, port, token)
	if err != nil {
		// A response can be lost after the server has entered closing. Recovery
		// must restart the unchanged application rather than revive that child.
		a.markBackendUpdateNeedsRestart()
		return fmt.Errorf("request Desktop backend drain: %w", err)
	}
	if !accepted {
		return errors.New("Desktop backend refused the authenticated drain request")
	}
	a.markBackendUpdateNeedsRestart()
	select {
	case <-done:
		return nil
	case <-drainCtx.Done():
		return fmt.Errorf("wait for Desktop backend to exit before update: %w", drainCtx.Err())
	}
}

func (a *App) markBackendUpdateNeedsRestart() {
	a.webuiMu.Lock()
	a.webuiUpdateNeedsRestart = true
	a.webuiMu.Unlock()
}

// A failed install leaves the old app on disk (the installer rolls activation
// back). Once its child has entered closing, restart that app to restore a live
// iframe and a matching runtime. Before closing began it is enough to reopen
// admission. The same recovery also handles failure to launch the new app.
func (a *App) recoverDesktopAfterFailedUpdate(base context.Context, appPath, metisBin string) (bool, error) {
	a.webuiMu.Lock()
	needsRestart := a.webuiUpdateNeedsRestart
	if !needsRestart {
		a.webuiUpdating = false
	}
	cmd, done, port, token := a.webuiCmd, a.webuiDone, a.webuiPort, a.webuiToken
	drainDeadline := a.webuiUpdateDrainDeadline
	a.webuiMu.Unlock()
	if !needsRestart {
		return false, nil
	}
	if a.restartDesktop == nil || a.scheduleRestart == nil || a.quit == nil {
		return false, errors.New("Desktop recovery restart is unavailable; reopen the application")
	}
	if cmd != nil && cmd.Process != nil && !waitForWebUIProcess(done, 0) {
		// A lost response may fail before the drain grace elapsed. Preserve its
		// remaining time before falling back to a signal, and never start a
		// replacement while the closing backend can still write state.
		remaining := time.Until(drainDeadline)
		if remaining < 0 {
			remaining = 0
		}
		stopWebUIProcessWithPolicy(cmd, done, port, token, webUIStopPolicy{
			shutdownGrace: remaining, signalGrace: webUISignalGrace, killWait: webUIKillWait,
		})
		if !waitForWebUIProcess(done, 0) {
			return false, errors.New("closing Desktop backend has not exited; reopen after it stops")
		}
	}
	if err := a.restartDesktop(appPath, a.workDir, metisBin); err != nil {
		return false, fmt.Errorf("restart Desktop after interrupted update: %w; reopen the application", err)
	}
	a.scheduleRestart(func() { a.quit(base) })
	return true, nil
}

func requestBackendUpdateShutdown(ctx context.Context, port int, token string) (bool, error) {
	if port < 1 || strings.TrimSpace(token) == "" {
		return false, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, webUIShutdownRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/shutdown", http.NoBody)
	if err != nil {
		return false, err
	}
	request.Header.Set(desktopShutdownTokenHeader, token)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	return response.StatusCode == http.StatusAccepted, nil
}
