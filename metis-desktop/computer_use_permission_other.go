//go:build !darwin

package main

import (
	"context"
	"errors"
)

func requestDesktopComputerUsePermission(string) (bool, error) {
	return false, errors.New("Computer Use permission requests are available only on macOS")
}

func desktopComputerUsePermissionStatus() DesktopComputerUsePermissionStatus {
	return DesktopComputerUsePermissionStatus{Accessibility: "unavailable", ScreenRecording: "unavailable"}
}

func openDesktopComputerUsePermissionSettings(context.Context, string) error {
	return errors.New("Computer Use permission requests are available only on macOS")
}
