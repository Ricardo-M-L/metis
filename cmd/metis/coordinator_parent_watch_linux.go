//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func coordinatorProcessBirthIdentity(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", errors.New("owning coordinator process is unavailable")
	}
	// The command field is parenthesized and can itself contain spaces and ')'.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errors.New("owning coordinator process identity is invalid")
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return "", errors.New("owning coordinator process is unavailable")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", errors.New("owning coordinator boot identity is unavailable")
	}
	return fmt.Sprintf("linux:%s:%d:%s", strings.TrimSpace(string(boot)), pid, fields[19]), nil
}
