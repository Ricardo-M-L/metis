//go:build darwin

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func coordinatorProcessBirthIdentity(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info == nil || int(info.Proc.P_pid) != pid || info.Proc.P_stat == 5 { // SZOMB
		return "", errors.New("owning coordinator process is unavailable")
	}
	return fmt.Sprintf("darwin:%d:%d:%d", pid, info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), nil
}
