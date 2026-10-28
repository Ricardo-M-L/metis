package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const desktopTrialTestBackendFlag = "--desktop-handoff-test-backend"

// TestMain calls this before parsing testing flags. All executables below are
// copies of the test binary; none starts Wails, a real CLI, or LaunchServices.
func desktopTrialTestProcess() bool {
	if os.Getenv("METIS_HANDOFF_PROCESS_FIXTURE") != "1" {
		return false
	}
	if path := desktopUpdateArg(os.Args, desktopTrialTestBackendFlag); path != "" {
		os.Exit(desktopTrialTestBackend(path))
		return true
	}
	if path := desktopUpdateArg(os.Args, desktopHelperFlag); path != "" {
		tx, err := loadDesktopTransaction(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(30)
		}
		desktopRollbackRestart = func(app, workspace, binary string) error {
			if app != tx.AppPath || workspace != tx.Workspace || binary != tx.MetisBin {
				return errors.New("fixture rollback changed restart arguments")
			}
			var launched desktopTrialClaim
			if err := desktopTrialRead(filepath.Join(filepath.Dir(path), "launched.json"), &launched); err == nil && desktopProcessAlive(launched.NativePID) {
				return errors.New("fixture rollback reopened while the launched candidate was alive")
			}
			if err := desktopTrialTestStopped(tx.Workspace); err != nil {
				return err
			}
			version, err := os.ReadFile(filepath.Join(app, "fixture-version"))
			if err != nil || string(version) != "old" {
				return errors.New("fixture rollback reopened before the old bundle was restored")
			}
			if _, err := os.Stat(desktopTrialMarker(app)); !os.IsNotExist(err) {
				return errors.New("fixture rollback retained transaction arguments for a second trial")
			}
			f, err := os.OpenFile(filepath.Join(workspace, "old-reopened"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString("native and backend stopped; old bundle restored; no transaction\n")
			return err
		}
		handled, err := desktopUpdateHelperCommand(os.Args)
		if !handled || err != nil {
			fmt.Fprintln(os.Stderr, "fixture supervisor:", err)
			os.Exit(31)
		}
		os.Exit(0)
		return true
	}
	if path := desktopUpdateArg(os.Args, desktopTrialFlag); path != "" {
		os.Exit(desktopTrialTestNative(path))
		return true
	}
	return false
}

func desktopTrialTestNative(path string) int {
	tx, err := loadDesktopTransaction(path)
	if err != nil {
		return 40
	}
	if err := os.WriteFile(filepath.Join(tx.Workspace, "native.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return 41
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(stop)
	if os.Getenv("METIS_HANDOFF_PROCESS_MODE") == "expired" {
		select {
		case <-stop:
			return 0
		case <-time.After(3 * time.Second):
			return 49
		}
	}
	backend := exec.Command(os.Args[0], desktopTrialTestBackendFlag, path)
	backend.Env = os.Environ() // Inherit the native's process group, too.
	if err := backend.Start(); err != nil {
		return 42
	}
	backendDone := make(chan struct{})
	go func() { _ = backend.Wait(); close(backendDone) }()
	var claim desktopTrialClaim
	for time.Now().Before(tx.Deadline) {
		if desktopTrialRead(filepath.Join(tx.Workspace, "backend.json"), &claim) == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if claim.BackendPID == 0 {
		_ = backend.Process.Signal(syscall.SIGTERM)
		<-backendDone
		return 43
	}
	if os.Getenv("METIS_HANDOFF_PROCESS_MODE") == "crash" {
		// No claim exists yet. The supervisor must stop the orphan via its group.
		return 44
	}
	claim.ID, claim.Token = tx.ID, tx.Token
	claim.NativePID, claim.NativeSHA = os.Getpid(), tx.NativeSHA
	claim.CLISHA, claim.Version = tx.CLISHA, tx.Version
	if desktopTrialWrite(filepath.Join(filepath.Dir(path), "claim.json"), claim) != nil {
		return 45
	}
	if os.Getenv("METIS_HANDOFF_PROCESS_MODE") != "no-ack" {
		ack := claim
		if os.Getenv("METIS_HANDOFF_PROCESS_MODE") == "wrong-nonce" {
			ack.Token = strings.Repeat("c", 64)
		}
		if os.Getenv("METIS_HANDOFF_PROCESS_MODE") == "stale-id" {
			ack.ID = strings.Repeat("d", 32)
		}
		if desktopTrialWrite(filepath.Join(filepath.Dir(path), "ack.json"), ack) != nil {
			return 46
		}
	}
	if os.Getenv("METIS_HANDOFF_PROCESS_MODE") == "ack-crash" {
		time.Sleep(100 * time.Millisecond)
		// Bypass os.Exit's race-runtime delay: this is a real native exit
		// between health probes, while its backend deliberately stays alive.
		syscall.Exit(49)
		return 49
	}
	finish := func() int {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = requestBackendUpdateShutdown(ctx, claim.Port, claim.ShutdownToken)
		select {
		case <-backendDone:
		case <-time.After(3 * time.Second):
			_ = backend.Process.Signal(syscall.SIGTERM)
			select {
			case <-backendDone:
			case <-time.After(3 * time.Second):
				return 47
			}
		}
		return 0
	}
	for time.Now().Before(tx.Deadline.Add(3 * time.Second)) {
		var result desktopTrialResult
		if desktopTrialRead(filepath.Join(filepath.Dir(path), "result.json"), &result) == nil && result.ID == tx.ID && result.State == "committed" {
			_ = os.WriteFile(filepath.Join(filepath.Dir(path), "consumed"), []byte(tx.ID), 0o600)
			return finish()
		}
		select {
		case <-stop:
			return finish()
		case <-time.After(10 * time.Millisecond):
		}
	}
	_ = finish()
	return 48
}

func desktopTrialTestBackend(path string) int {
	tx, err := loadDesktopTransaction(path)
	if err != nil {
		return 50
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 51
	}
	claim := desktopTrialClaim{BackendPID: os.Getpid(), Port: listener.Addr().(*net.TCPAddr).Port, ShutdownToken: "fixture-shutdown-" + tx.ID}
	if desktopTrialWrite(filepath.Join(tx.Workspace, "backend.json"), claim) != nil {
		_ = listener.Close()
		return 52
	}
	shutdown := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		if os.Getenv("METIS_HANDOFF_PROCESS_MODE") == "backend-failure" {
			http.Error(w, "fixture backend unhealthy", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get(desktopShutdownTokenHeader) != claim.ShutdownToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		select {
		case shutdown <- "authenticated":
		default:
		}
	})
	server := &http.Server{Handler: mux}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(stop)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		reason := "signal"
		select {
		case reason = <-shutdown:
		case <-stop:
		}
		// The supervisor must join this persistence boundary before reopening old.
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(tx.Workspace, "backend-persisted"), []byte(reason), 0o600)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return 53
	}
	<-drained
	return 0
}

func desktopTrialTestStopped(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "native.pid"))
	mode := os.Getenv("METIS_HANDOFF_PROCESS_MODE")
	mayNotStart := mode == "expired" || strings.HasPrefix(mode, "not-executable")
	if err != nil && !(mayNotStart && os.IsNotExist(err)) {
		return err
	}
	if err == nil {
		pid, err := strconv.Atoi(string(data))
		if err != nil || desktopProcessAlive(pid) {
			return errors.New("fixture old reopen preceded candidate native exit")
		}
	}
	var backend desktopTrialClaim
	if err := desktopTrialRead(filepath.Join(dir, "backend.json"), &backend); err != nil {
		if mayNotStart && os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if desktopProcessAlive(backend.BackendPID) {
		return errors.New("fixture old reopen preceded candidate backend exit")
	}
	if _, err := os.Stat(filepath.Join(dir, "backend-persisted")); err != nil {
		return errors.New("fixture old reopen preceded backend persistence")
	}
	return nil
}

func TestDesktopHandoffProcessHealthAndRollback(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Desktop supervisor supports macOS and Linux")
	}
	for _, mode := range []string{"healthy", "no-ack", "backend-failure", "crash", "ack-crash", "wrong-nonce", "stale-id", "expired", "not-executable", "not-executable-unreadable"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "fixture-state")
			if err := os.Mkdir(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			app := filepath.Join(root, "METIS.app")
			for _, item := range []struct{ path, version string }{{app, "candidate"}, {app + ".previous", "old"}} {
				writeTestDesktopBundle(t, item.path, "1.2.0", "placeholder")
				desktopTrialTestCopy(t, os.Args[0], desktopNativePath(item.path, "darwin"))
				if err := os.WriteFile(filepath.Join(item.path, "fixture-version"), []byte(item.version), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dir, err := os.MkdirTemp(root, ".metis-health-")
			if err != nil {
				t.Fatal(err)
			}
			helper := filepath.Join(dir, "trusted-update-helper")
			desktopTrialTestCopy(t, os.Args[0], helper)
			sha, err := desktopFileSHA256(helper)
			if err != nil {
				t.Fatal(err)
			}
			parent := exec.Command("/bin/sh", "-c", "exit 0")
			if err := parent.Run(); err != nil {
				t.Fatal(err)
			}
			tx := desktopUpdateTransaction{ID: strings.Repeat("a", 32), Token: strings.Repeat("b", 64), AppPath: app, Workspace: workspace, MetisBin: helper, Version: "1.2.0", NativeSHA: sha, CLISHA: sha, HelperSHA: sha, ParentPID: parent.Process.Pid, Deadline: time.Now().Add(5 * time.Second), GOOS: "darwin"}
			if mode == "expired" {
				tx.Deadline = time.Now().Add(-time.Second)
			}
			path := filepath.Join(dir, "transaction.json")
			if err := desktopTrialWrite(path, tx); err != nil {
				t.Fatal(err)
			}
			if err := desktopTrialWrite(desktopTrialMarker(app), struct {
				Path string `json:"path"`
				ID   string `json:"id"`
			}{path, tx.ID}); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "not-executable") {
				permission := os.FileMode(0o644)
				if mode == "not-executable-unreadable" {
					permission = 0
				}
				if err := os.Chmod(desktopNativePath(app, "darwin"), permission); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("METIS_HANDOFF_PROCESS_FIXTURE", "1")
			t.Setenv("METIS_HANDOFF_PROCESS_MODE", mode)
			t.Cleanup(func() {
				var group int
				var identity string
				if desktopTrialRead(filepath.Join(dir, "group-pid.json"), &group) == nil && group > 0 &&
					desktopTrialRead(filepath.Join(dir, "group-identity.json"), &identity) == nil && identity != "" {
					_ = stopDesktopTrialGroup(group, time.Second, map[int]string{group: identity})
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, helper, desktopHelperFlag, path)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("supervisor: %v; output: %s", err, output)
			}
			var result desktopTrialResult
			if err := desktopTrialRead(app+".metis-update-result", &result); err != nil {
				t.Fatal(err)
			}
			wantState, wantVersion := "rolled-back", "old"
			if mode == "healthy" {
				wantState, wantVersion = "committed", "candidate"
			}
			if result.ID != tx.ID || result.State != wantState {
				t.Fatalf("result = %+v, want %s", result, wantState)
			}
			if (mode == "expired" || mode == "stale-id") && !strings.Contains(result.Error, "deadline") {
				t.Fatalf("%s did not reject health confirmation at the deadline: %+v", mode, result)
			}
			if mode == "not-executable" && !strings.Contains(result.Error, "start new Desktop") {
				t.Fatalf("readable but non-executable candidate did not exercise launch failure: %+v", result)
			}
			version, err := os.ReadFile(filepath.Join(app, "fixture-version"))
			if err != nil || string(version) != wantVersion {
				t.Fatalf("installed bundle = %q, %v; want %q", version, err, wantVersion)
			}
			deadline := time.Now().Add(4 * time.Second)
			for desktopTrialTestStopped(workspace) != nil && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if err := desktopTrialTestStopped(workspace); err != nil {
				t.Fatal(err)
			}
			reopened, err := os.ReadFile(filepath.Join(workspace, "old-reopened"))
			if mode == "healthy" {
				if !os.IsNotExist(err) {
					t.Fatalf("healthy candidate unexpectedly reopened old: %q, %v", reopened, err)
				}
			} else if err != nil || len(reopened) == 0 {
				t.Fatalf("rollback did not safely reopen old: %q, %v", reopened, err)
			}
			if _, err := os.Stat(desktopTrialMarker(app)); !os.IsNotExist(err) {
				t.Fatalf("transaction marker remained after %s: %v", result.State, err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("private transaction directory remained: %v", err)
			}
			if mode == "expired" || strings.HasPrefix(mode, "not-executable") {
				return // No backend or user work is admitted for these startup failures.
			}
			persisted, err := os.ReadFile(filepath.Join(workspace, "backend-persisted"))
			wantDrain := "authenticated"
			if mode == "crash" {
				wantDrain = "signal"
			}
			if err != nil || string(persisted) != wantDrain {
				t.Fatalf("backend persistence = %q, %v; want %q", persisted, err, wantDrain)
			}
		})
	}
}

func TestDesktopHandoffGroupEscalationRequiresKnownIdentity(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process group ownership requires POSIX signals")
	}
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprintf("known_%v", known), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "term-ignored")
			command := exec.Command("/bin/sh", "-c", `trap '' TERM; printf ready > "$1"; while :; do :; done`, "desktop-trial-group-test", marker)
			desktopTrialProcessGroup(command)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			t.Cleanup(func() {
				_ = command.Process.Kill() // Only the test's direct shell, never another process group.
			})
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture shell did not install its TERM trap")
				}
				time.Sleep(time.Millisecond)
			}
			var identities map[int]string
			if known {
				identity := desktopTrialProcessIdentity(command.Process.Pid)
				if identity == "" {
					t.Fatal("fixture shell has no process identity")
				}
				identities = map[int]string{command.Process.Pid: identity}
			}
			err := stopDesktopTrialGroup(command.Process.Pid, 10*time.Millisecond, identities)
			if known {
				if err != nil || desktopProcessAlive(command.Process.Pid) {
					t.Fatalf("owned TERM-ignoring shell was not killed and joined: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "ownership") || !desktopProcessAlive(command.Process.Pid) {
					t.Fatalf("unknown shell was not left alive after refusing escalation: %v", err)
				}
				_ = command.Process.Kill()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("TERM-ignoring fixture unexpectedly exited successfully")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("test fixture shell was not reaped")
			}
		})
	}
}

func TestDesktopHandoffProcessWithoutTransactionDoesNotStartTrial(t *testing.T) {
	if handled, err := desktopUpdateHelperCommand([]string{"metis-desktop"}); handled || err != nil {
		t.Fatalf("ordinary startup entered helper: handled=%v err=%v", handled, err)
	}
	t.Setenv("METIS_HANDOFF_PROCESS_FIXTURE", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ordinary fixture launch recursed into trial: %v; output: %s", err, output)
	}
}

func desktopTrialTestCopy(t *testing.T, source, destination string) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy test executable: %v %v", copyErr, closeErr)
	}
}
