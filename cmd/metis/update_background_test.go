package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestStartAutoUpdaterDoesNotBlockOnNetworkCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	checkStarted := make(chan struct{})
	releaseCheck := make(chan struct{})
	loopStopped := make(chan struct{})
	deps := autoUpdateDependencies{
		check: func(context.Context) string {
			close(checkStarted)
			<-releaseCheck
			return ""
		},
		wait: func(context.Context, time.Duration) bool {
			close(loopStopped)
			return false
		},
	}

	returned := make(chan struct{})
	starter := new(autoUpdaterStarter)
	go func() {
		starter.start(ctx, deps)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("startAutoUpdater blocked on the release check")
	}
	select {
	case <-checkStarted:
	case <-time.After(time.Second):
		t.Fatal("background release check did not start immediately")
	}
	close(releaseCheck)
	select {
	case <-loopStopped:
	case <-time.After(time.Second):
		t.Fatal("background updater did not stop")
	}
}

func TestAutoUpdaterStartsOnlyOneLoopPerProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	checks := make(chan struct{}, 2)
	stop := make(chan struct{})
	deps := autoUpdateDependencies{
		check: func(context.Context) string {
			checks <- struct{}{}
			<-stop
			return ""
		},
	}
	starter := new(autoUpdaterStarter)
	if !starter.start(ctx, deps) {
		t.Fatal("first start was not accepted")
	}
	if starter.start(ctx, deps) {
		t.Fatal("second start created another updater loop")
	}
	select {
	case <-checks:
	case <-time.After(time.Second):
		t.Fatal("updater loop did not start")
	}
	select {
	case <-checks:
		t.Fatal("more than one updater loop ran")
	default:
	}
	close(stop)
}

func TestAutoUpdateLoopChecksImmediatelyThenEveryThirtyMinutes(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	checks, cleanups, waits := 0, 0, 0
	deps := autoUpdateDependencies{
		check: func(context.Context) string {
			mu.Lock()
			defer mu.Unlock()
			checks++
			return ""
		},
		cleanupManaged: func(context.Context) { cleanups++ },
		wait: func(_ context.Context, interval time.Duration) bool {
			if interval != autoUpdateInterval {
				t.Fatalf("wait interval = %s, want %s", interval, autoUpdateInterval)
			}
			waits++
			return waits == 1
		},
	}

	runAutoUpdateLoop(ctx, deps)

	mu.Lock()
	defer mu.Unlock()
	if checks != 2 {
		t.Fatalf("release checks = %d, want 2", checks)
	}
	if cleanups != 2 {
		t.Fatalf("managed cleanups = %d, want one per check", cleanups)
	}
}

func TestAutoUpdateLoopInstallsOnceAndNotifiesAfterSuccess(t *testing.T) {
	ctx := context.Background()
	var checks, installs, marks int
	var gotTag, gotNotice string
	deps := autoUpdateDependencies{
		check: func(context.Context) string {
			checks++
			return "v1.2.3"
		},
		install: func(_ context.Context, tag string) (autoUpdateInstallResult, error) {
			installs++
			gotTag = tag
			return autoUpdateInstallResult{
				installed: true,
				notice:    "metis 1.2.3 installed (restart to apply)",
			}, nil
		},
		markNotified: func(tag string) {
			marks++
			if tag != "v1.2.3" {
				t.Errorf("marked tag = %q, want v1.2.3", tag)
			}
		},
		notify: func(notice string) { gotNotice = notice },
		wait: func(context.Context, time.Duration) bool {
			return checks == 1
		},
	}

	runAutoUpdateLoop(ctx, deps)

	if checks != 2 {
		t.Fatalf("release checks = %d, want 2", checks)
	}
	if installs != 1 {
		t.Fatalf("installs = %d, want one install for the same tag", installs)
	}
	if marks != 1 {
		t.Fatalf("notification marks = %d, want 1", marks)
	}
	if gotTag != "v1.2.3" {
		t.Fatalf("installed tag = %q, want v1.2.3", gotTag)
	}
	if gotNotice != "metis 1.2.3 installed (restart to apply)" {
		t.Fatalf("notice = %q", gotNotice)
	}
}

func TestAutoUpdateLoopSilentlyRetriesFailedInstall(t *testing.T) {
	ctx := context.Background()
	var checks, installs, marks, notices int
	deps := autoUpdateDependencies{
		check: func(context.Context) string {
			checks++
			return "v1.2.3"
		},
		install: func(context.Context, string) (autoUpdateInstallResult, error) {
			installs++
			if installs == 1 {
				return autoUpdateInstallResult{}, errors.New("temporary network failure")
			}
			return autoUpdateInstallResult{installed: true, notice: "installed"}, nil
		},
		markNotified: func(string) { marks++ },
		notify:       func(string) { notices++ },
		wait: func(context.Context, time.Duration) bool {
			return checks == 1
		},
	}

	runAutoUpdateLoop(ctx, deps)

	if installs != 2 {
		t.Fatalf("installs = %d, want retry on next interval", installs)
	}
	if marks != 1 || notices != 1 {
		t.Fatalf("marks/notices = %d/%d, want one successful low-noise notification", marks, notices)
	}
}

func TestAutoUpdateLoopDoesNotNotifyWhenAnotherProcessAlreadyInstalled(t *testing.T) {
	ctx := context.Background()
	var checks, installs, marks, notices int
	deps := autoUpdateDependencies{
		check: func(context.Context) string {
			checks++
			return "v1.2.3"
		},
		install: func(context.Context, string) (autoUpdateInstallResult, error) {
			installs++
			return autoUpdateInstallResult{installed: false}, nil
		},
		markNotified: func(string) { marks++ },
		notify:       func(string) { notices++ },
		wait: func(context.Context, time.Duration) bool {
			return checks == 1
		},
	}

	runAutoUpdateLoop(ctx, deps)

	if installs != 1 {
		t.Fatalf("installs = %d, want one same-tag core check", installs)
	}
	if marks != 0 || notices != 0 {
		t.Fatalf("marks/notices = %d/%d, want no false install notification", marks, notices)
	}
}

func TestStartAutoUpdaterHonorsNoUpdateCheck(t *testing.T) {
	t.Setenv("METIS_NO_UPDATE_CHECK", "1")
	called := make(chan struct{}, 1)
	deps := autoUpdateDependencies{
		check: func(context.Context) string {
			called <- struct{}{}
			return ""
		},
	}

	if new(autoUpdaterStarter).start(context.Background(), deps) {
		t.Fatal("startAutoUpdater reported started with METIS_NO_UPDATE_CHECK=1")
	}
	select {
	case <-called:
		t.Fatal("release check ran with METIS_NO_UPDATE_CHECK=1")
	default:
	}
}

func TestDefaultAutoUpdaterNeverWritesFromBackgroundToTerminal(t *testing.T) {
	deps := defaultAutoUpdateDependencies()
	if deps.notify != nil {
		t.Fatal("default background updater must not write directly into the active TUI")
	}
}
