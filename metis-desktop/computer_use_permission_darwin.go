//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreGraphics -framework CoreFoundation
#include <ApplicationServices/ApplicationServices.h>
#include <CoreGraphics/CoreGraphics.h>
#include <CoreFoundation/CoreFoundation.h>

static int metisRequestAccessibility(void) {
    CFMutableDictionaryRef options = CFDictionaryCreateMutable(kCFAllocatorDefault, 0,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    if (options == NULL) {
        return -1;
    }
    CFDictionarySetValue(options, kAXTrustedCheckOptionPrompt, kCFBooleanTrue);
    Boolean trusted = AXIsProcessTrustedWithOptions(options);
    CFRelease(options);
    return trusted ? 1 : 0;
}

static int metisRequestScreenRecording(void) {
    if (__builtin_available(macOS 10.15, *)) {
        return CGRequestScreenCaptureAccess() ? 1 : 0;
    }
    return -1;
}

static int metisPreflightScreenRecording(void) {
    if (__builtin_available(macOS 10.15, *)) {
        return CGPreflightScreenCaptureAccess() ? 1 : 0;
    }
    return -1;
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

func requestDesktopComputerUsePermission(kind string) (bool, error) {
	var result C.int
	switch kind {
	case "accessibility":
		result = C.metisRequestAccessibility()
	case "screen-recording":
		result = C.metisRequestScreenRecording()
	default:
		return false, errors.New("unsupported Computer Use permission")
	}
	if result < 0 {
		return false, fmt.Errorf("could not request macOS %s permission", kind)
	}
	return result == 1, nil
}

func desktopComputerUsePermissionStatus() DesktopComputerUsePermissionStatus {
	status := DesktopComputerUsePermissionStatus{Accessibility: "notGranted", ScreenRecording: "notGranted"}
	if C.AXIsProcessTrusted() != 0 {
		status.Accessibility = "granted"
	}
	switch C.metisPreflightScreenRecording() {
	case 1:
		status.ScreenRecording = "granted"
	case -1:
		status.ScreenRecording = "unavailable"
	}
	return status
}

func openDesktopComputerUsePermissionSettings(ctx context.Context, kind string) error {
	var pane string
	switch kind {
	case "accessibility":
		pane = "Privacy_Accessibility"
	case "screen-recording":
		pane = "Privacy_ScreenCapture"
	default:
		return errors.New("unsupported Computer Use permission")
	}
	return exec.CommandContext(ctx, "/usr/bin/open", "x-apple.systempreferences:com.apple.preference.security?"+pane).Run()
}
