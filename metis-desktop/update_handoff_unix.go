//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func desktopProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil || p.Signal(syscall.Signal(0)) != nil {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	return err == nil && strings.TrimSpace(string(out)) != "" && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
func desktopTrialProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func desktopTrialGroupAlive(pid int) bool {
	out, err := exec.Command("ps", "-ax", "-o", "pgid=,stat=").Output()
	if err != nil {
		return syscall.Kill(-pid, 0) == nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == strconv.Itoa(pid) && !strings.HasPrefix(f[1], "Z") {
			return true
		}
	}
	return false
}

// Birth/command identity is captured while the native is our direct child,
// or while its authenticated backend claim belongs to that same group.
func desktopTrialProcessIdentity(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=,comm=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func desktopTrialGroupSnapshot(group int) map[int]string {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,pgid=,lstart=,comm=").Output()
	if err != nil {
		return nil
	}
	records := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != strconv.Itoa(group) {
			continue
		}
		pid, e := strconv.Atoi(f[0])
		if e == nil {
			records[pid] = strings.Join(f[2:], " ")
		}
	}
	return records
}
func stopDesktopTrialGroup(pid int, grace time.Duration, known map[int]string) error {
	if !desktopTrialGroupAlive(pid) {
		return nil
	}
	before := desktopTrialGroupSnapshot(pid)
	anchored := false
	for member, identity := range known {
		if identity != "" && strings.Join(strings.Fields(identity), " ") == before[member] {
			anchored = true
		}
	}
	if !anchored {
		return errors.New("cannot prove candidate group ownership before signaling")
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(grace)
	for desktopTrialGroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !desktopTrialGroupAlive(pid) {
		return nil
	}
	// Hard termination is limited to the exact continuously-owned trial group.
	// An unrecognized member or reused PID/birth identity keeps recovery closed.
	after := desktopTrialGroupSnapshot(pid)
	if !anchored || len(after) == 0 {
		return errors.New("cannot prove ownership of remaining candidate processes")
	}
	for member, identity := range after {
		if before[member] != identity {
			return errors.New("candidate process identity changed during shutdown")
		}
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline = time.Now().Add(5 * time.Second)
	for desktopTrialGroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if desktopTrialGroupAlive(pid) {
		return errors.New("candidate process group did not exit after bounded termination")
	}
	return nil
}

func desktopTrialPIDInGroup(pid, group int) bool {
	if !desktopProcessAlive(pid) {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pgid=").Output()
	return err == nil && strings.TrimSpace(string(out)) == strconv.Itoa(group)
}
