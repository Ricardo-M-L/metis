package tui

// exit_hook_test.go — package-init override of exitFunc so the 800 ms
// belt-and-braces hard-exit goroutine from the Ctrl-C double-tap path
// can fire harmlessly during tests. Without this, the goroutine would
// call os.Exit(0) and kill the whole `go test ./internal/tui/...`
// binary mid-suite.
//
// Why init() not TestMain: the tui package has many test files and no
// existing TestMain; init() runs before any test (or even any
// package-level test var init), single-writer, single-shot, no race
// with the goroutine's read.
//
// Tests that need to assert "the exit goroutine actually fired" can
// snapshot exitTestCallCount before the action and poll until it
// increments — see TestCtrlC_DoubleTapDuringTurnQuits.

import "sync/atomic"

var exitTestCallCount int64

func init() {
	exitFunc = func(int) {
		atomic.AddInt64(&exitTestCallCount, 1)
	}
}
