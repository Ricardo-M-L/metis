package main

import (
	goruntime "runtime"
	"testing"
)

// Keep platform-neutral CLI, JSON, and ledger tests running on Windows. Only
// fixtures which require POSIX persistence or macOS host services use this guard.
func requireCoordinatorPOSIXTest(t *testing.T, contract string) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip(contract + "; Windows native support is not implemented")
	}
}
