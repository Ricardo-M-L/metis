//go:build !darwin && !linux

package main

import "errors"

func coordinatorProcessBirthIdentity(pid int) (string, error) {
	return "", errors.New("coordinator parent lifetime guard is unsupported on this host")
}
