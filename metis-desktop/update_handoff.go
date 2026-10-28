package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var errDesktopTrialStillRunning = errors.New("candidate processes have not stopped")
var desktopRollbackRestart = restartDesktopProcess

const desktopTrialTimeout = 90 * time.Second
const desktopTrialFlag = "--desktop-update-trial"
const desktopHelperFlag = "--desktop-update-helper"

type desktopUpdateTransaction struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	AppPath   string    `json:"appPath"`
	Workspace string    `json:"workspace"`
	MetisBin  string    `json:"metisBin,omitempty"`
	Version   string    `json:"version"`
	NativeSHA string    `json:"nativeSHA"`
	CLISHA    string    `json:"cliSHA"`
	HelperSHA string    `json:"helperSHA"`
	ParentPID int       `json:"parentPID"`
	Deadline  time.Time `json:"deadline"`
	GOOS      string    `json:"goos"`
}
type desktopTrialClaim struct {
	ID            string `json:"id"`
	Token         string `json:"token"`
	NativePID     int    `json:"nativePID"`
	GroupPID      int    `json:"groupPID,omitempty"`
	BackendPID    int    `json:"backendPID,omitempty"`
	Port          int    `json:"port,omitempty"`
	ShutdownToken string `json:"shutdownToken,omitempty"`
	NativeSHA     string `json:"nativeSHA"`
	CLISHA        string `json:"cliSHA,omitempty"`
	Version       string `json:"version"`
}
type desktopTrialMarkerState struct {
	Path     string `json:"path"`
	ID       string `json:"id"`
	OwnerPID int    `json:"ownerPID,omitempty"`
}
type desktopTrialResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

func desktopUpdateArg(args []string, flag string) string {
	for i := 1; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
func desktopNativePath(app, goos string) string {
	if goos == "darwin" {
		return filepath.Join(app, "Contents", "MacOS", "metis-desktop")
	}
	return app
}
func desktopTrialMarker(app string) string { return app + ".metis-health-update" }
func desktopTrialWrite(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".health-state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err == nil {
		if dir, e := os.Open(filepath.Dir(path)); e == nil {
			err = dir.Sync()
			_ = dir.Close()
		}
	}
	return err
}
func desktopTrialRead(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 32<<10 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("unsafe Desktop health transaction file")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 32<<10))
	dec.DisallowUnknownFields()
	if err = dec.Decode(value); err != nil {
		return err
	}
	var extra any
	if err = dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("extra Desktop health transaction data")
	}
	return nil
}
func loadDesktopTransaction(path string) (desktopUpdateTransaction, error) {
	var t desktopUpdateTransaction
	path, err := filepath.Abs(path)
	if err != nil {
		return t, err
	}
	if filepath.Base(path) != "transaction.json" {
		return t, errors.New("invalid Desktop health transaction path")
	}
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return t, errors.New("unsafe Desktop health transaction directory")
	}
	if err = desktopTrialRead(path, &t); err != nil {
		return t, err
	}
	if t.Deadline.IsZero() || t.Deadline.After(time.Now().Add(desktopTrialTimeout+time.Minute)) {
		return t, errors.New("invalid Desktop health deadline")
	}
	if len(t.Token) != 64 || len(t.ID) != 32 || t.ParentPID <= 0 || (t.GOOS != "darwin" && t.GOOS != "linux") || !filepath.IsAbs(t.AppPath) || filepath.Dir(dir) != filepath.Dir(t.AppPath) || !strings.HasPrefix(filepath.Base(dir), ".metis-health-") {
		return t, errors.New("invalid Desktop health transaction identity")
	}
	for _, v := range []string{t.Token, t.ID, t.NativeSHA, t.CLISHA, t.HelperSHA} {
		if _, err = hex.DecodeString(v); err != nil {
			return t, errors.New("invalid Desktop health transaction digest")
		}
	}
	if len(t.NativeSHA) != 64 || len(t.CLISHA) != 64 || len(t.HelperSHA) != 64 {
		return t, errors.New("invalid Desktop health transaction digest size")
	}
	var marker desktopTrialMarkerState
	if err = desktopTrialRead(desktopTrialMarker(t.AppPath), &marker); err != nil {
		return t, err
	}
	if marker.ID != t.ID || marker.Path != path {
		return t, errors.New("Desktop health transaction is no longer current")
	}
	return t, nil
}

// Copy the running, trusted helper BEFORE the installed application is renamed.
// A bad candidate (including a loader failure) must never supervise its own rollback.
func prepareDesktopUpdateHandoff(appPath, workspace, binary string) (func(string) error, func(), error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, nil, errors.New("health-confirmed Desktop updates are unsupported on this platform")
	}
	dir, err := os.MkdirTemp(filepath.Dir(appPath), ".metis-health-")
	if err != nil {
		return nil, nil, err
	}
	owned := true
	markerID := ""
	cleanup := func() {
		if owned {
			if markerID != "" {
				if marker, p, e := desktopPreparation(appPath); e == nil && marker.ID == markerID {
					if sha, e := desktopFileSHA256(desktopNativePath(appPath, p.GOOS)); e != nil || sha != p.OldSHA {
						return
					}
				}
			}
			var current desktopTrialMarkerState
			if markerID != "" && desktopTrialRead(desktopTrialMarker(appPath), &current) == nil && current.ID == markerID && current.Path == filepath.Join(dir, "transaction.json") {
				_ = os.Remove(desktopTrialMarker(appPath))
			}
			_ = os.RemoveAll(dir)
		}
	}
	fail := func(err error) (func(string) error, func(), error) { cleanup(); return nil, nil, err }
	if err = os.Chmod(dir, 0o700); err != nil {
		return fail(err)
	}
	host, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	expected := desktopNativePath(appPath, runtime.GOOS)
	if filepath.Clean(host) != filepath.Clean(expected) {
		return fail(errors.New("running Desktop does not own the update destination"))
	}
	input, err := os.Open(host)
	if err != nil {
		return fail(err)
	}
	defer input.Close()
	helper := filepath.Join(dir, "trusted-update-helper")
	output, err := os.OpenFile(helper, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		return fail(err)
	}
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return fail(copyErr)
	}
	helperSHA, err := desktopFileSHA256(helper)
	if err != nil {
		return fail(err)
	}
	hostSHA, err := desktopFileSHA256(host)
	if err != nil || helperSHA != hostSHA {
		return fail(errors.New("trusted Desktop helper copy changed"))
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return fail(err)
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		return fail(err)
	}
	tx := filepath.Join(dir, "transaction.json")
	marker := desktopTrialMarker(appPath)
	markerID = hex.EncodeToString(id)
	preparation := desktopTrialPreparation{ID: markerID, AppPath: appPath, Workspace: workspace, MetisBin: binary, OldSHA: hostSHA, OwnerPID: os.Getpid(), OwnerIdentity: desktopTrialProcessIdentity(os.Getpid()), Deadline: time.Now().Add(12 * time.Minute), GOOS: runtime.GOOS}
	if err = desktopTrialWrite(filepath.Join(dir, "preparation.json"), preparation); err != nil {
		return fail(err)
	}
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(fmt.Errorf("another Desktop health handoff is pending: %w", err))
	}
	data, _ := json.Marshal(desktopTrialMarkerState{Path: tx, ID: markerID, OwnerPID: os.Getpid()})
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeMarkerErr := f.Close()
	if err == nil {
		err = closeMarkerErr
	}
	if err != nil {
		return fail(err)
	}
	start := func(version string) error {
		nativeSHA, err := desktopFileSHA256(desktopNativePath(appPath, runtime.GOOS))
		if err != nil {
			return err
		}
		cliSHA, err := desktopUpdateRuntimeSHA(context.Background(), appPath, binary, version, runtime.GOOS, runtime.GOARCH, nativeSHA)
		if err != nil {
			return err
		}
		t := desktopUpdateTransaction{ID: hex.EncodeToString(id), Token: hex.EncodeToString(token), AppPath: appPath, Workspace: workspace, MetisBin: binary, Version: version, NativeSHA: nativeSHA, CLISHA: cliSHA, HelperSHA: helperSHA, ParentPID: os.Getpid(), Deadline: time.Now().Add(desktopTrialTimeout), GOOS: runtime.GOOS}
		if err = desktopTrialWrite(tx, t); err != nil {
			return err
		}
		var current desktopTrialMarkerState
		if err = desktopTrialRead(marker, &current); err != nil || current.ID != t.ID || current.Path != tx || current.OwnerPID != os.Getpid() {
			return errors.New("Desktop health handoff reservation changed")
		}
		cmd := exec.Command(helper, desktopHelperFlag, tx)
		cmd.Env = desktopRestartEnvironment(os.Environ())
		if err = cmd.Start(); err != nil {
			_ = os.Remove(marker)
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var ready desktopTrialResult
			if desktopTrialRead(filepath.Join(dir, "ready.json"), &ready) == nil && ready.ID == t.ID && ready.State == "supervising" {
				owned = false
				return nil
			}
			select {
			case <-done:
				_ = os.Remove(marker)
				return errors.New("Desktop health helper exited before taking ownership")
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = cmd.Process.Kill()
		<-done
		_ = os.Remove(marker)
		return errors.New("Desktop health helper did not take ownership")
	}
	return start, cleanup, nil
}

func desktopUpdateHelperCommand(args []string) (bool, error) {
	path := desktopUpdateArg(args, desktopHelperFlag)
	if path == "" {
		return false, nil
	}
	if len(args) != 3 || args[1] != desktopHelperFlag {
		return true, errors.New("invalid Desktop health helper arguments")
	}
	t, err := loadDesktopTransaction(path)
	if err != nil {
		return true, err
	}
	own, err := os.Executable()
	if err != nil {
		return true, err
	}
	sha, err := desktopFileSHA256(own)
	if err != nil || filepath.Clean(own) != filepath.Join(filepath.Dir(path), "trusted-update-helper") || sha != t.HelperSHA {
		return true, errors.New("untrusted Desktop health helper executable")
	}
	return true, runDesktopUpdateSupervisor(path, t)
}

func readDesktopTrialClaim(path string, t desktopUpdateTransaction, pid int) (desktopTrialClaim, error) {
	var c desktopTrialClaim
	err := desktopTrialRead(filepath.Join(filepath.Dir(path), "claim.json"), &c)
	if err != nil {
		return c, err
	}
	if c.ID != t.ID || c.Token != t.Token || c.NativePID != pid || c.NativeSHA != t.NativeSHA || c.Version != t.Version {
		return c, errors.New("stale or mismatched Desktop health claim")
	}
	return c, nil
}
func validDesktopHealthAck(path string, t desktopUpdateTransaction, pid int) bool {
	var c desktopTrialClaim
	if desktopTrialRead(filepath.Join(filepath.Dir(path), "ack.json"), &c) != nil {
		return false
	}
	return c.ID == t.ID && c.Token == t.Token && c.NativePID == pid && c.NativeSHA == t.NativeSHA && c.CLISHA == t.CLISHA && c.Version == t.Version && c.BackendPID > 0 && c.Port > 0 && c.Port < 65536 && c.ShutdownToken != ""
}
func desktopTrialResultWrite(path string, t desktopUpdateTransaction, state string, err error) error {
	r := desktopTrialResult{ID: t.ID, State: state}
	if err != nil {
		r.Error = err.Error()
	}
	return desktopTrialWrite(filepath.Join(filepath.Dir(path), "result.json"), r)
}
func clearDesktopTrialMarker(path string, t desktopUpdateTransaction) {
	var m desktopTrialMarkerState
	if desktopTrialRead(desktopTrialMarker(t.AppPath), &m) == nil && m.Path == path && m.ID == t.ID {
		_ = os.Remove(desktopTrialMarker(t.AppPath))
	}
}

func runDesktopUpdateSupervisor(path string, t desktopUpdateTransaction) error {
	dir := filepath.Dir(path)
	if err := desktopTrialWrite(filepath.Join(dir, "supervisor.json"), desktopTrialOwner{ID: t.ID, PID: os.Getpid(), Identity: desktopTrialProcessIdentity(os.Getpid())}); err != nil {
		return err
	}
	if err := desktopTrialResultWrite(path, t, "waiting", nil); err != nil {
		return err
	}
	if err := desktopTrialWrite(filepath.Join(dir, "ready.json"), desktopTrialResult{ID: t.ID, State: "supervising"}); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery-abort")); err == nil {
		return nil
	}
	for desktopProcessAlive(t.ParentPID) {
		if time.Now().After(t.Deadline) {
			_ = desktopTrialResultWrite(path, t, "failed-before-launch", errors.New("old Desktop did not exit before the update deadline"))
			return errors.New("old Desktop did not exit before the update deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery-abort")); err == nil {
		return nil
	}
	native := desktopNativePath(t.AppPath, t.GOOS)
	actual, err := desktopFileSHA256(native)
	if err != nil || actual != t.NativeSHA {
		err = errors.New("Desktop candidate changed before launch")
	} else {
		err = superviseDesktopCandidate(path, t, native)
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, errDesktopTrialStillRunning) {
		_ = desktopTrialResultWrite(path, t, "failed", err)
		return err
	}
	_ = desktopTrialResultWrite(path, t, "rolling-back", err)
	if rollbackErr := rollbackDesktopActivation(t.AppPath, t.GOOS); rollbackErr != nil {
		_ = desktopTrialResultWrite(path, t, "failed", rollbackErr)
		return fmt.Errorf("Desktop health failed: %v; rollback failed: %w", err, rollbackErr)
	}
	clearDesktopTrialMarker(path, t)
	// No transaction arguments on this launch: rollback is attempted only once.
	restartErr := desktopRollbackRestart(t.AppPath, t.Workspace, t.MetisBin)
	result := desktopTrialResult{ID: t.ID, State: "rolled-back", Error: err.Error()}
	if restartErr != nil {
		result.State = "failed"
		result.Error += "; reopen old Desktop: " + restartErr.Error()
	}
	_ = desktopTrialWrite(t.AppPath+".metis-update-result", result)
	if restartErr == nil {
		_ = os.RemoveAll(dir)
	} else {
		_ = desktopTrialResultWrite(path, t, "failed", restartErr)
	}
	if restartErr != nil {
		return fmt.Errorf("%v; reopen restored Desktop: %w", err, restartErr)
	}
	return nil
}

func superviseDesktopCandidate(path string, t desktopUpdateTransaction, native string) error {
	args := []string{"--workspace", t.Workspace, desktopTrialFlag, path}
	if t.MetisBin != "" {
		args = append(args, "--metis-bin", t.MetisBin)
	}
	info, err := os.Stat(native)
	if err != nil || info.Mode()&0o111 == 0 {
		return errors.New("start new Desktop: candidate is not executable")
	}
	dir := filepath.Dir(path)
	// The small shell remains alive after a native crash. It is a stable,
	// directly-owned process-group anchor even before a backend claim exists.
	// Every dynamic value is a positional argument, never interpolated code.
	const launch = `
umask 077
state=$1
shift
printf '%s\n' "$$" > "$state/group-pid.json"
identity=$(/bin/ps -p "$$" -o lstart=,comm=)
printf '"%s"\n' "$identity" > "$state/group-identity.json"
if [ -f "$state/recovery-abort" ]; then exit 0; fi
"$@" &
native_pid=$!
printf '%s\n' "$native_pid" > "$state/native-pid.json"
wait "$native_pid"
status=$?
printf '%s\n' "$status" > "$state/native-exit.json"
while [ -d "$state" ] && [ ! -f "$state/launch-committed" ]; do /bin/sleep 0.1; done
exit "$status"
`
	launchArgs := append([]string{"-c", launch, "metis-update-trial", dir, native}, args...)
	cmd := exec.Command("/bin/sh", launchArgs...)
	cmd.Env = desktopRestartEnvironment(os.Environ())
	desktopTrialProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start new Desktop: %w", err)
	}
	groupPID := cmd.Process.Pid
	known := map[int]string{groupPID: desktopTrialProcessIdentity(groupPID)}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	nativePID := 0
	for time.Now().Before(t.Deadline) {
		if desktopTrialRead(filepath.Join(dir, "native-pid.json"), &nativePID) == nil && nativePID > 0 {
			break
		}
		select {
		case err := <-done:
			return fmt.Errorf("candidate launcher exited: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := desktopTrialWrite(filepath.Join(dir, "launched.json"), desktopTrialClaim{ID: t.ID, Token: t.Token, NativePID: nativePID, GroupPID: groupPID, NativeSHA: t.NativeSHA, Version: t.Version}); err != nil {
		_ = stopDesktopTrialGroup(groupPID, webUISignalGrace, known)
		return fmt.Errorf("%w: cannot persist candidate process identity: %v", errDesktopTrialStillRunning, err)
	}
	nativeIdentity := desktopTrialProcessIdentity(nativePID)
	nativeAlive := func() bool {
		if nativeIdentity == "" || !desktopTrialPIDInGroup(nativePID, groupPID) {
			return false
		}
		if strings.Join(strings.Fields(desktopTrialProcessIdentity(nativePID)), " ") != strings.Join(strings.Fields(nativeIdentity), " ") {
			return false
		}
		_, err := os.Lstat(filepath.Join(dir, "native-exit.json"))
		return os.IsNotExist(err)
	}
	var failure error
	for time.Now().Before(t.Deadline) {
		select {
		case err := <-done:
			failure = fmt.Errorf("new Desktop exited before health confirmation: %v", err)
			done = nil
		default:
		}
		if failure != nil {
			break
		}
		var nativeExit int
		if desktopTrialRead(filepath.Join(dir, "native-exit.json"), &nativeExit) == nil {
			failure = fmt.Errorf("new Desktop exited before health confirmation: exit %d", nativeExit)
			break
		}
		var candidateFailure desktopTrialResult
		if desktopTrialRead(filepath.Join(filepath.Dir(path), "failure.json"), &candidateFailure) == nil && candidateFailure.ID == t.ID && candidateFailure.State == "candidate-failed" {
			failure = errors.New("new Desktop could not start its verified backend")
			break
		}
		if validDesktopHealthAck(path, t, nativePID) {
			claim, claimErr := readDesktopTrialClaim(path, t, nativePID)
			var ack desktopTrialClaim
			ackErr := desktopTrialRead(filepath.Join(filepath.Dir(path), "ack.json"), &ack)
			if nativeAlive() && claimErr == nil && ackErr == nil && claim == ack && claim.CLISHA == t.CLISHA && desktopTrialPIDInGroup(claim.BackendPID, groupPID) && desktopTrialHealthProbe(claim.Port) {
				// Keep the process alive through a second probe, so an immediate exit does
				// not turn a buffered ACK into a successful update.
				time.Sleep(200 * time.Millisecond)
				select {
				case err := <-done:
					failure = fmt.Errorf("new Desktop exited during confirmation: %v", err)
					done = nil
				default:
				}
				if failure != nil {
					break
				}
				if nativeAlive() && desktopTrialPIDInGroup(claim.BackendPID, groupPID) && desktopTrialHealthProbe(claim.Port) {
					if err := desktopTrialResultWrite(path, t, "committed", nil); err != nil {
						failure = err
						break
					}
					_ = os.WriteFile(filepath.Join(dir, "launch-committed"), []byte(t.ID), 0o600)
					// A durable small result lets the candidate consume commit even if
					// the supervisor exits between commit and private-stage cleanup.
					if err := desktopTrialWrite(t.AppPath+".metis-update-result", desktopTrialResult{ID: t.ID, State: "committed"}); err != nil {
						return nil
					}
					clearDesktopTrialMarker(path, t)
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						if _, err := os.Stat(filepath.Join(filepath.Dir(path), "consumed")); err == nil {
							_ = os.RemoveAll(filepath.Dir(path))
							break
						}
						time.Sleep(20 * time.Millisecond)
					}
					_ = os.RemoveAll(dir)
					return nil
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if failure == nil {
		failure = errors.New("new Desktop did not confirm native and backend health before the deadline")
	}
	_ = desktopTrialResultWrite(path, t, "aborting", failure)
	// This candidate never received a usable frame URL. Still use the backend's
	// normal authenticated persistence/drain path before stopping its native host.
	signalGrace := webUIShutdownGrace
	if claim, e := readDesktopTrialClaim(path, t, nativePID); e == nil && claim.BackendPID > 0 && claim.Port > 0 && claim.Port < 65536 && claim.ShutdownToken != "" && desktopTrialPIDInGroup(claim.BackendPID, groupPID) {
		signalGrace = webUISignalGrace
		if desktopTrialPIDInGroup(claim.BackendPID, groupPID) {
			known[claim.BackendPID] = desktopTrialProcessIdentity(claim.BackendPID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), webUIShutdownGrace)
		_, _ = requestBackendUpdateShutdown(ctx, claim.Port, claim.ShutdownToken)
		for desktopProcessAlive(claim.BackendPID) && ctx.Err() == nil {
			time.Sleep(50 * time.Millisecond)
		}
		cancel()

	}
	// The new native owns a fresh process group. SIGTERM also reaches a backend
	// created just before a native crash, before it could publish its claim.
	if err := stopDesktopTrialGroup(groupPID, signalGrace, known); err != nil {
		return fmt.Errorf("%w: %v", errDesktopTrialStillRunning, err)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("%w: native process has not exited", errDesktopTrialStillRunning)
		}
	}
	return failure
}
func desktopTrialHealthProbe(port int) bool {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 750 * time.Millisecond}
	r, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", port))
	if err != nil {
		return false
	}
	defer r.Body.Close()
	return r.StatusCode == http.StatusOK
}

func rollbackDesktopActivation(appPath, goos string) error {
	previous := appPath + ".previous"
	if goos == "darwin" {
		if err := validateDesktopRollbackBundle(previous); err != nil {
			return err
		}
	} else {
		if err := validateDesktopRollbackBinary(previous, goos); err != nil {
			return err
		}
	}
	if err := desktopAtomicSwap(previous, appPath); err != nil {
		return fmt.Errorf("atomically restore previous Desktop: %w", err)
	}
	if err := desktopSyncDirectory(filepath.Dir(appPath)); err != nil {
		return err
	}
	// .previous now contains only the failed, stopped candidate.
	if goos == "darwin" {
		return os.RemoveAll(previous)
	}
	return os.Remove(previous)
}

func (a *App) desktopTrialClaim(selected desktopRuntimeSelection) error {
	if a.updateTrialPath == "" {
		return nil
	}
	t, err := loadDesktopTransaction(a.updateTrialPath)
	if err != nil {
		return err
	}
	native, err := os.Executable()
	if err != nil {
		return err
	}
	sha, err := desktopFileSHA256(native)
	cliSHA, cliErr := desktopFileSHA256(selected.Path)
	if err != nil || cliErr != nil || sha != t.NativeSHA || cliSHA != t.CLISHA || filepath.Clean(native) != desktopNativePath(t.AppPath, t.GOOS) || normalizeDesktopVersion(a.GetVersion()) != t.Version || (t.MetisBin == "" && !selected.Bundled) {
		return errors.New("new Desktop health runtime identity mismatch")
	}
	c := desktopTrialClaim{ID: t.ID, Token: t.Token, NativePID: os.Getpid(), GroupPID: os.Getppid(), BackendPID: a.webuiCmd.Process.Pid, Port: a.webuiPort, ShutdownToken: a.webuiToken, NativeSHA: sha, CLISHA: cliSHA, Version: t.Version}
	return desktopTrialWrite(filepath.Join(filepath.Dir(a.updateTrialPath), "claim.json"), c)
}
func (a *App) confirmDesktopTrial() error {
	if a.updateTrialPath == "" {
		return nil
	}
	path := a.updateTrialPath
	t, err := loadDesktopTransaction(path)
	if err != nil {
		return err
	}
	var claim desktopTrialClaim
	if err = desktopTrialRead(filepath.Join(filepath.Dir(path), "claim.json"), &claim); err != nil {
		return err
	}
	if err = desktopTrialWrite(filepath.Join(filepath.Dir(path), "ack.json"), claim); err != nil {
		return err
	}
	for time.Now().Before(t.Deadline) {
		var result desktopTrialResult
		readErr := desktopTrialRead(filepath.Join(filepath.Dir(path), "result.json"), &result)
		if readErr != nil {
			_ = desktopTrialRead(t.AppPath+".metis-update-result", &result)
		}
		if result.ID == t.ID {
			if result.State == "committed" {
				// Keep the result visible in the new window, rather than declaring success
				// in the old process merely because a detached launcher was started.
				a.publishDesktopUpdateProgress(DesktopUpdateProgress{Phase: "completed", Message: "METIS updated and ready", Percent: 100, Done: true})
				_ = os.WriteFile(filepath.Join(filepath.Dir(path), "consumed"), []byte(t.ID), 0o600)
				a.updateTrialPath = ""
				return nil
			}
			if result.State == "aborting" || result.State == "rolling-back" || result.State == "failed" {
				return errors.New("new Desktop health confirmation failed; restoring the previous version")
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("Desktop health supervisor did not commit this update")
}

func desktopTrialInstallAdmission(appPath string) error {
	var marker desktopTrialMarkerState
	err := desktopTrialRead(desktopTrialMarker(appPath), &marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect pending Desktop health handoff: %w", err)
	}
	if marker.OwnerPID != os.Getpid() || len(marker.ID) != 32 || !filepath.IsAbs(marker.Path) || filepath.Base(marker.Path) != "transaction.json" || filepath.Dir(filepath.Dir(marker.Path)) != filepath.Dir(appPath) || !strings.HasPrefix(filepath.Base(filepath.Dir(marker.Path)), ".metis-health-") {
		return errors.New("a Desktop update is still awaiting startup health confirmation")
	}
	return nil
}
