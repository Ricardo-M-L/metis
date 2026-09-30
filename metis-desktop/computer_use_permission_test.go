package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRequestComputerUsePermission(t *testing.T) {
	for _, tc := range []struct {
		name             string
		kind             string
		granted          bool
		requestError     error
		wantRequestCount int
		wantOpenCount    int
		wantError        bool
	}{
		{name: "accessibility opens settings", kind: "accessibility", wantRequestCount: 1, wantOpenCount: 1},
		{name: "screen recording opens settings", kind: "screen-recording", wantRequestCount: 1, wantOpenCount: 1},
		{name: "already granted does not open settings", kind: "accessibility", granted: true, wantRequestCount: 1},
		{name: "unsupported kind rejected before native call", kind: "input-monitoring", wantError: true},
		{name: "native request failure does not open settings", kind: "accessibility", requestError: errors.New("request failed"), wantRequestCount: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestCount := 0
			openCount := 0
			app := &App{
				requestComputerUsePermission: func(kind string) (bool, error) {
					requestCount++
					if kind != tc.kind {
						t.Errorf("requested kind = %q, want %q", kind, tc.kind)
					}
					return tc.granted, tc.requestError
				},
				openComputerUsePermissionSettings: func(_ context.Context, kind string) error {
					openCount++
					if kind != tc.kind {
						t.Errorf("opened kind = %q, want %q", kind, tc.kind)
					}
					return nil
				},
			}
			result, err := app.RequestComputerUsePermission(tc.kind)
			if (err != nil) != tc.wantError {
				t.Fatalf("RequestComputerUsePermission(%q) error = %v, wantError %t", tc.kind, err, tc.wantError)
			}
			if requestCount != tc.wantRequestCount || openCount != tc.wantOpenCount {
				t.Fatalf("request/open counts = %d/%d, want %d/%d", requestCount, openCount, tc.wantRequestCount, tc.wantOpenCount)
			}
			if err == nil && (result.Kind != tc.kind || result.Granted != tc.granted || result.SettingsOpened != (tc.wantOpenCount == 1)) {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestComputerUsePermissionBridgeUsesNativeGUI(t *testing.T) {
	source, err := os.ReadFile("frontend/src/main.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"'request-computer-use-permission'",
		"kind !== 'accessibility' && kind !== 'screen-recording'",
		"['RequestComputerUsePermission'](kind)",
		"'get-computer-use-permission-status'",
		"['GetComputerUsePermissionStatus']()",
	} {
		if !strings.Contains(string(source), text) {
			t.Fatalf("native Desktop bridge missing %q", text)
		}
	}
}

func TestGetComputerUsePermissionStatusHasNoRequestSideEffect(t *testing.T) {
	statusCalls := 0
	app := &App{
		computerUsePermissionStatus: func() DesktopComputerUsePermissionStatus {
			statusCalls++
			return DesktopComputerUsePermissionStatus{Accessibility: "granted", ScreenRecording: "notGranted"}
		},
		requestComputerUsePermission: func(string) (bool, error) {
			t.Fatal("status query requested OS permission")
			return false, nil
		},
	}
	got := app.GetComputerUsePermissionStatus()
	if got.Accessibility != "granted" || got.ScreenRecording != "notGranted" || statusCalls != 1 {
		t.Fatalf("status = %+v, calls = %d", got, statusCalls)
	}
}
