//go:build darwin && !cgo

package main

import (
	"context"
	"errors"
)

func requestDesktopComputerUsePermission(string) (bool, error) {
	return false, errors.New("macOS Computer Use permission requests require cgo")
}

func desktopComputerUsePermissionStatus() DesktopComputerUsePermissionStatus {
	return DesktopComputerUsePermissionStatus{Accessibility: "unavailable", ScreenRecording: "unavailable"}
}

func openDesktopComputerUsePermissionSettings(context.Context, string) error {
	return errors.New("macOS Computer Use permission requests require cgo")
}
