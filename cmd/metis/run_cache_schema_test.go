package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/exitcode"
)

func TestShouldUseRunCacheDisablesSchemaConstrainedRuns(t *testing.T) {
	if shouldUseRunCache(true, "/tmp/result.schema.json") {
		t.Fatal("--cache + --output-schema must not read or write the legacy run cache")
	}
	if shouldUseRunCache(true, "  schema.json  ") {
		t.Fatal("whitespace around schema path bypassed the cache safety boundary")
	}
	if !shouldUseRunCache(true, "") {
		t.Fatal("ordinary explicitly cached runs should remain enabled")
	}
	if shouldUseRunCache(false, "") {
		t.Fatal("cache was enabled without an opt-in")
	}
}

func TestRunTerminalErrorPreservesContentFilterExitClass(t *testing.T) {
	if got := exitcode.Classify(runTerminalError("content_filter", "")); got != exitcode.ContentFilter {
		t.Fatalf("content-filter exit = %d, want %d", got, exitcode.ContentFilter)
	}
	if got := exitcode.Classify(runTerminalError("max_tokens", "")); got != exitcode.Incomplete {
		t.Fatalf("max-token exit = %d, want %d", got, exitcode.Incomplete)
	}
}
