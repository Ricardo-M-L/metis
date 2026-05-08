package main

// debug_log.go — `--debug` / `-d` writes a verbose trace to
// ~/.metis/debug.log alongside the existing METIS_DEBUG=1 stderr path,
// so leaving the flag on doesn't pollute interactive output.
//
// We don't redirect stdout / stderr — `log.SetOutput` would steal logs
// from interactive surfaces (REPL renderers compute width from the same
// fd). Instead we tee everything Go's stdlib `log` produces into the
// file via an io.MultiWriter. Callers continue to use log.Printf and
// fmt.Fprintln(os.Stderr, ...); the latter still goes to stderr only.
//
// File is opened with O_APPEND so multiple metis sessions can share the
// same path without truncating each other.

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// debugLogFile is held package-global so deferred Close() in main() can
// reach it. The runtime cleanup chain doesn't have a hook here today;
// the OS reaps the descriptor on exit, but we still close cleanly when
// possible.
var debugLogFile *os.File

func openDebugLog() error {
	if debugLogFile != nil {
		return nil
	}
	dir := config.Home()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "debug.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	debugLogFile = f
	// Tee Go's stdlib log into the file alongside its previous output.
	// log.Default() writes to os.Stderr by default — the multi-writer
	// keeps that behavior so stderr-bound debug output is unchanged
	// AND newly persisted to disk.
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, f))
	// Mark the boundary so a follow-on session in the same file is
	// trivially distinguishable.
	fmt.Fprintf(f, "\n========== metis debug session started (pid=%d) ==========\n", os.Getpid())
	return nil
}

// closeDebugLog releases the file handle. Safe to call even when the
// log was never opened. main()'s defer chain calls this so the boundary
// marker isn't left half-flushed on graceful exit.
func closeDebugLog() {
	if debugLogFile == nil {
		return
	}
	_ = debugLogFile.Close()
	debugLogFile = nil
}
