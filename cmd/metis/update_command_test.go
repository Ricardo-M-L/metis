package main

import (
	"strings"
	"testing"
)

func TestWindowsInstallCommandPinsRelease(t *testing.T) {
	got := windowsInstallCommand("v1.2.3")
	if strings.Count(got, "v1.2.3") != 2 {
		t.Fatalf("command should pin both requested version and installer source: %q", got)
	}
	if !strings.Contains(got, "install/install.ps1") {
		t.Fatalf("command does not reference the PowerShell installer: %q", got)
	}
}
