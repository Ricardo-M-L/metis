//go:build windows

package main

import (
	"errors"
	"os/exec"
	"time"
)

func desktopProcessAlive(pid int) bool       { return false }
func desktopTrialProcessGroup(cmd *exec.Cmd) {}
func stopDesktopTrialGroup(pid int, grace time.Duration, known map[int]string) error {
	return errors.New("Windows update supervisor is unavailable")
}

func desktopTrialPIDInGroup(pid, group int) bool { return false }

func desktopTrialProcessIdentity(pid int) string { return "" }
func desktopTrialGroupAlive(pid int) bool        { return false }
