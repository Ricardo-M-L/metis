package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDesktopHandoffRecoveryInterruptedPhases(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Desktop recovery requires an atomic swap platform")
	}
	dead := exec.Command("/bin/sh", "-c", "exit 0")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, state string
		currentNew  bool
		oldIsSelf   bool
		ownerAlive  bool
		restart     bool
	}{
		{name: "preparing-without-transaction-current-old-self", oldIsSelf: true},
		{name: "preparing-current-old-other-process", restart: true},
		{name: "waiting-current-new", state: "waiting", currentNew: true, restart: true},
		{name: "aborting-current-new", state: "aborting", currentNew: true, restart: true},
		{name: "rolling-back-current-already-old", state: "rolling-back", restart: true},
		{name: "committed-keeps-new", state: "committed", currentNew: true},
		{name: "live-owner-refuses-recovery", ownerAlive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			app := filepath.Join(root, "METIS.app")
			current := "old fixture"
			if tc.currentNew {
				current = "new fixture"
			}
			writeTestDesktopBundle(t, app, "1.2.0", current)
			if tc.oldIsSelf {
				desktopTrialTestCopy(t, os.Args[0], desktopNativePath(app, "darwin"))
			}
			oldPath := app
			if tc.currentNew || tc.state == "rolling-back" {
				previous := "old fixture"
				if !tc.currentNew {
					previous = "new fixture"
				}
				writeTestDesktopBundle(t, app+".previous", "1.1.0", previous)
				if tc.currentNew {
					oldPath = app + ".previous"
				}
			}
			oldNative := desktopNativePath(oldPath, "darwin")
			oldSHA, err := desktopFileSHA256(oldNative)
			if err != nil {
				t.Fatal(err)
			}
			oldIdentity, err := os.Stat(oldNative)
			if err != nil {
				t.Fatal(err)
			}
			currentSHA, err := desktopFileSHA256(desktopNativePath(app, "darwin"))
			if err != nil {
				t.Fatal(err)
			}
			dir, err := os.MkdirTemp(root, ".metis-health-recovery-")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "transaction.json")
			ownerPID, ownerIdentity := dead.Process.Pid, "exited fixture owner"
			if tc.ownerAlive {
				ownerPID, ownerIdentity = os.Getpid(), desktopTrialProcessIdentity(os.Getpid())
			}
			preparation := desktopTrialPreparation{ID: strings.Repeat("e", 32), AppPath: app, Workspace: root, MetisBin: "/fixture/metis", OldSHA: oldSHA, OwnerPID: ownerPID, OwnerIdentity: ownerIdentity, Deadline: time.Now().Add(-time.Second), GOOS: "darwin"}
			marker := desktopTrialMarkerState{Path: path, ID: preparation.ID, OwnerPID: ownerPID}
			if err := desktopTrialWrite(filepath.Join(dir, "preparation.json"), preparation); err != nil {
				t.Fatal(err)
			}
			if err := desktopTrialWrite(desktopTrialMarker(app), marker); err != nil {
				t.Fatal(err)
			}
			if err := desktopTrialWrite(filepath.Join(dir, "supervisor.json"), desktopTrialOwner{ID: preparation.ID, PID: dead.Process.Pid, Identity: "exited fixture helper"}); err != nil {
				t.Fatal(err)
			}
			if tc.state != "" {
				tx := desktopUpdateTransaction{ID: preparation.ID, Token: strings.Repeat("f", 64), AppPath: app, Workspace: root, MetisBin: preparation.MetisBin, Version: "1.2.0", NativeSHA: currentSHA, CLISHA: oldSHA, HelperSHA: oldSHA, ParentPID: ownerPID, Deadline: preparation.Deadline, GOOS: "darwin"}
				if err := desktopTrialWrite(path, tx); err != nil {
					t.Fatal(err)
				}
				if err := desktopTrialWrite(filepath.Join(dir, "result.json"), desktopTrialResult{ID: preparation.ID, State: tc.state}); err != nil {
					t.Fatal(err)
				}
			}
			gotMarker, gotPreparation, err := desktopPreparation(app)
			if err != nil || gotMarker != marker || gotPreparation.OldSHA != oldSHA {
				t.Fatalf("preparation = %+v %+v, %v", gotMarker, gotPreparation, err)
			}
			restarts, quits := 0, 0
			a := &App{ctx: context.Background(), restartDesktop: func(path, workspace, binary string) error {
				if path != app || workspace != root || binary != preparation.MetisBin {
					t.Fatalf("recovery changed restart arguments: %q %q %q", path, workspace, binary)
				}
				if sha, err := desktopFileSHA256(desktopNativePath(app, "darwin")); err != nil || sha != oldSHA {
					t.Fatalf("recovery restarted before old bytes were restored: %s %v", sha, err)
				}
				restarts++
				return nil
			}, scheduleRestart: func(fn func()) { fn() }, quit: func(context.Context) { quits++ }}
			err = a.recoverStoppedDesktopTrial(app)
			if tc.ownerAlive {
				if err == nil || !strings.Contains(err.Error(), "still running") || restarts != 0 || quits != 0 {
					t.Fatalf("live owner was not refused: restarts=%d quits=%d err=%v", restarts, quits, err)
				}
				if _, err := os.Stat(dir); err != nil {
					t.Fatalf("live owner state was removed: %v", err)
				}
				if _, err := os.Stat(desktopTrialMarker(app)); err != nil {
					t.Fatalf("live owner marker was removed: %v", err)
				}
				return
			}
			wantRestart := 0
			if tc.restart {
				wantRestart = 1
				if err == nil || !strings.Contains(err.Error(), "restored and is restarting") {
					t.Fatalf("restored old process did not report its safe restart: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if restarts != wantRestart || quits != wantRestart {
				t.Fatalf("recovery restart/quit = %d/%d, want %d", restarts, quits, wantRestart)
			}
			wantState, wantSHA := "rolled-back", oldSHA
			if tc.state == "committed" {
				wantState, wantSHA = "committed", currentSHA
			}
			var result desktopTrialResult
			if err := desktopTrialRead(app+".metis-update-result", &result); err != nil || result.ID != preparation.ID || result.State != wantState {
				t.Fatalf("recovery result = %+v, %v; want %s", result, err, wantState)
			}
			if sha, err := desktopFileSHA256(desktopNativePath(app, "darwin")); err != nil || sha != wantSHA {
				t.Fatalf("recovered native SHA = %s, %v; want %s", sha, err, wantSHA)
			}
			if tc.state != "committed" {
				if after, err := os.Stat(desktopNativePath(app, "darwin")); err != nil || !os.SameFile(oldIdentity, after) {
					t.Fatalf("recovery did not preserve the old native object: %v", err)
				}
			}
			if tc.state == "rolling-back" {
				if contents, err := os.ReadFile(desktopNativePath(app+".previous", "darwin")); err != nil || string(contents) != "new fixture" {
					t.Fatalf("already restored old bundle was swapped again: %q %v", contents, err)
				}
			}
			for _, removed := range []string{dir, desktopTrialMarker(app)} {
				if _, err := os.Lstat(removed); !os.IsNotExist(err) {
					t.Fatalf("recovered private state remains at %s: %v", removed, err)
				}
			}
		})
	}
}
