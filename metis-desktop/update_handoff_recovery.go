package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type desktopTrialPreparation struct {
	ID            string    `json:"id"`
	AppPath       string    `json:"appPath"`
	Workspace     string    `json:"workspace"`
	MetisBin      string    `json:"metisBin,omitempty"`
	OldSHA        string    `json:"oldSHA"`
	OwnerPID      int       `json:"ownerPID"`
	OwnerIdentity string    `json:"ownerIdentity"`
	Deadline      time.Time `json:"deadline"`
	GOOS          string    `json:"goos"`
}
type desktopTrialOwner struct {
	ID       string `json:"id"`
	PID      int    `json:"pid"`
	Identity string `json:"identity"`
}
type desktopActivationRecord struct {
	Candidate string `json:"candidate"`
	OldSHA    string `json:"oldSHA"`
	NewSHA    string `json:"newSHA"`
}

func desktopTrialOwnerAlive(pid int, identity string) bool {
	if !desktopProcessAlive(pid) {
		return false
	}
	// Unknown identity is conservative; it is never authority to signal a PID.
	return identity == "" || strings.Join(strings.Fields(desktopTrialProcessIdentity(pid)), " ") == strings.Join(strings.Fields(identity), " ")
}
func desktopSyncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func desktopPreparation(app string) (desktopTrialMarkerState, desktopTrialPreparation, error) {
	var marker desktopTrialMarkerState
	var p desktopTrialPreparation
	if err := desktopTrialRead(desktopTrialMarker(app), &marker); err != nil {
		return marker, p, err
	}
	dir := filepath.Dir(marker.Path)
	if !filepath.IsAbs(marker.Path) || filepath.Base(marker.Path) != "transaction.json" || filepath.Dir(dir) != filepath.Dir(app) || !strings.HasPrefix(filepath.Base(dir), ".metis-health-") {
		return marker, p, errors.New("invalid pending Desktop health directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return marker, p, errors.New("unsafe pending Desktop health directory")
	}
	if err = desktopTrialRead(filepath.Join(dir, "preparation.json"), &p); err != nil {
		return marker, p, err
	}
	if p.ID != marker.ID || p.AppPath != app || p.OwnerPID != marker.OwnerPID || len(p.OldSHA) != 64 || p.Deadline.IsZero() {
		return marker, p, errors.New("pending Desktop preparation identity mismatch")
	}
	return marker, p, nil
}
func recordDesktopActivation(candidate, appPath string) error {
	marker, p, err := desktopPreparation(appPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	oldSHA, err := desktopFileSHA256(desktopNativePath(appPath, p.GOOS))
	if err != nil {
		return err
	}
	if oldSHA != p.OldSHA {
		return errors.New("installed Desktop changed during update preparation")
	}
	newSHA, err := desktopFileSHA256(desktopNativePath(candidate, p.GOOS))
	if err != nil {
		return err
	}
	return desktopTrialWrite(filepath.Join(filepath.Dir(marker.Path), "activation.json"), desktopActivationRecord{Candidate: candidate, OldSHA: oldSHA, NewSHA: newSHA})
}
func desktopActivationHasParkedOld(app, stageRoot string) bool {
	marker, p, err := desktopPreparation(app)
	if err != nil {
		return false
	}
	var a desktopActivationRecord
	if desktopTrialRead(filepath.Join(filepath.Dir(marker.Path), "activation.json"), &a) != nil {
		return false
	}
	relative, err := filepath.Rel(stageRoot, a.Candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return false
	}
	sha, err := desktopFileSHA256(desktopNativePath(a.Candidate, p.GOOS))
	return err == nil && sha == p.OldSHA
}
func restorePreparedDesktop(marker desktopTrialMarkerState, p desktopTrialPreparation) error {
	current, err := desktopFileSHA256(desktopNativePath(p.AppPath, p.GOOS))
	if err == nil && current == p.OldSHA {
		return nil
	} // Includes a crash just after rollback's swap.
	previous := p.AppPath + ".previous"
	sha, err := desktopFileSHA256(desktopNativePath(previous, p.GOOS))
	if err == nil && sha == p.OldSHA {
		return rollbackDesktopActivation(p.AppPath, p.GOOS)
	}
	var a desktopActivationRecord
	if err = desktopTrialRead(filepath.Join(filepath.Dir(marker.Path), "activation.json"), &a); err != nil {
		return err
	}
	// The old app may still occupy the extraction slot after atomic activation,
	// before it could be parked at .previous. Never accept an arbitrary path.
	relative, e := filepath.Rel(filepath.Dir(p.AppPath), a.Candidate)
	parts := strings.Split(relative, string(os.PathSeparator))
	if e != nil || len(parts) < 3 || !strings.HasPrefix(parts[0], ".metis-update-") || a.OldSHA != p.OldSHA {
		return errors.New("unsafe parked Desktop recovery path")
	}
	stage := filepath.Join(filepath.Dir(p.AppPath), parts[0])
	info, e := os.Lstat(stage)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("unsafe parked Desktop stage")
	}
	sha, err = desktopFileSHA256(desktopNativePath(a.Candidate, p.GOOS))
	if err != nil || sha != p.OldSHA {
		return errors.New("cannot locate the preserved previous Desktop")
	}
	if err = desktopAtomicSwap(a.Candidate, p.AppPath); err != nil {
		return err
	}
	if err = desktopSyncDirectory(filepath.Dir(p.AppPath)); err != nil {
		return err
	}
	_ = os.RemoveAll(stage)
	return nil
}

// Ordinary reopen is the recovery entry point for every interrupted phase.
// Only dead owners can be recovered, and every signal is bound to a birth ID.
func (a *App) recoverStoppedDesktopTrial(appPath string) error {
	marker, p, err := desktopPreparation(appPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(marker.Path)
	selfSHA := ""
	if executable, e := os.Executable(); e == nil {
		selfSHA, _ = desktopFileSHA256(executable)
	}
	if desktopTrialOwnerAlive(p.OwnerPID, p.OwnerIdentity) {
		return errors.New("Desktop update is still running; wait for it to finish")
	}
	var owner desktopTrialOwner
	if desktopTrialRead(filepath.Join(dir, "supervisor.json"), &owner) == nil && owner.ID == p.ID && desktopTrialOwnerAlive(owner.PID, owner.Identity) {
		return errors.New("Desktop is finishing startup verification or recovery; wait for it to finish")
	}
	var result desktopTrialResult
	_ = desktopTrialRead(filepath.Join(dir, "result.json"), &result)
	var durable desktopTrialResult
	if desktopTrialRead(appPath+".metis-update-result", &durable) == nil && durable.ID == p.ID && durable.State == "committed" {
		result = durable
	}
	if result.ID == p.ID && result.State == "committed" {
		if err = desktopTrialWrite(appPath+".metis-update-result", result); err != nil {
			return err
		}
		_ = os.Remove(desktopTrialMarker(appPath))
		_ = os.RemoveAll(dir)
		return nil // Commit is durable; no late/stale ACK can undo it.
	}
	// Prevent a launcher which has not reached native creation from starting
	// after this recovery's process snapshot. Its proof files precede this gate.
	if err = desktopTrialWrite(filepath.Join(dir, "recovery-abort"), p.ID); err != nil {
		return err
	}
	if desktopTrialRead(filepath.Join(dir, "supervisor.json"), &owner) == nil && owner.ID == p.ID && desktopTrialOwnerAlive(owner.PID, owner.Identity) {
		return errors.New("Desktop recovery is handing over; retry after the previous helper exits")
	}
	var group int
	var identity string
	groupErr := desktopTrialRead(filepath.Join(dir, "group-pid.json"), &group)
	identityErr := desktopTrialRead(filepath.Join(dir, "group-identity.json"), &identity)
	if groupErr == nil && group > 0 && desktopTrialGroupAlive(group) {
		if identityErr != nil || identity == "" {
			return errors.New("Cannot verify the interrupted update process; close METIS and retry after it stops")
		}
		var tx desktopUpdateTransaction
		var claim desktopTrialClaim
		if loadErr := desktopTrialRead(marker.Path, &tx); loadErr == nil && tx.ID == p.ID && desktopTrialRead(filepath.Join(dir, "claim.json"), &claim) == nil && claim.ID == tx.ID && claim.Token == tx.Token && desktopTrialPIDInGroup(claim.BackendPID, group) {
			ctx, cancel := context.WithTimeout(context.Background(), webUIShutdownGrace)
			_, _ = requestBackendUpdateShutdown(ctx, claim.Port, claim.ShutdownToken)
			for desktopProcessAlive(claim.BackendPID) && ctx.Err() == nil {
				time.Sleep(50 * time.Millisecond)
			}
			cancel()
		}
		if err = stopDesktopTrialGroup(group, webUIShutdownGrace, map[int]string{group: identity}); err != nil {
			return fmt.Errorf("interrupted update is still stopping; close METIS and retry: %w", err)
		}
	} else if groupErr != nil && !os.IsNotExist(groupErr) {
		return errors.New("Interrupted update identity is incomplete; close METIS and retry")
	}
	if err = restorePreparedDesktop(marker, p); err != nil {
		return fmt.Errorf("restore preserved Desktop: %w", err)
	}
	result = desktopTrialResult{ID: p.ID, State: "rolled-back", Error: "An interrupted update was restored to the previous version"}
	_ = desktopTrialWrite(appPath+".metis-update-result", result)
	_ = os.Remove(desktopTrialMarker(appPath))
	// A crash can leave the original install lock, but no other updater's lock
	// may be removed merely because this transaction expired.
	lock := filepath.Join(filepath.Dir(appPath), "."+filepath.Base(appPath)+".metis-update.lock")
	if data, e := os.ReadFile(lock); e == nil && len(data) < 4096 && strings.HasPrefix(string(data), fmt.Sprintf("pid=%d\n", p.OwnerPID)) {
		_ = os.Remove(lock)
	}
	_ = os.RemoveAll(dir)
	if selfSHA == p.OldSHA {
		return nil
	}
	if a.restartDesktop == nil || a.scheduleRestart == nil || a.quit == nil {
		return errors.New("Previous Desktop restored; reopen the application")
	}
	if err = a.restartDesktop(appPath, p.Workspace, p.MetisBin); err != nil {
		return fmt.Errorf("previous Desktop restored; reopen the application: %w", err)
	}
	a.scheduleRestart(func() { a.quit(a.ctx) })
	return errors.New("The previous Desktop was restored and is restarting")
}
