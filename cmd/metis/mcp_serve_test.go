package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestCollectMCPTaskEventsRejectsIncompleteTerminal(t *testing.T) {
	events := make(chan agent.Event, 2)
	events <- agent.Event{Kind: agent.EventTextDelta, TextDelta: "partial"}
	events <- agent.Event{Kind: agent.EventLoopDone, StopReason: "max_tokens"}
	close(events)
	done := make(chan error, 1)
	done <- nil

	result, err := collectMCPTaskEvents(events, done)
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("error = %v, want max_tokens incomplete", err)
	}
	if result != "" {
		t.Fatalf("incomplete MCP result = %q, want no successful payload", result)
	}
}

func TestMCPRuntimeRunGatePreventsTraceAdapterReplacement(t *testing.T) {
	gate := newMCPRuntimeRunGate()

	// currentAdapter models the process-wide trace adapter installed by
	// setupRuntime. A concurrent entry would replace adapter 1 with adapter 2.
	var currentAdapter atomic.Int32
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := gate.run(context.Background(), func() (string, error) {
			if !currentAdapter.CompareAndSwap(0, 1) {
				return "", fmt.Errorf("first runtime replaced adapter %d", currentAdapter.Load())
			}
			close(firstEntered)
			<-releaseFirst
			if got := currentAdapter.Load(); got != 1 {
				return "", fmt.Errorf("adapter replaced while first runtime active: %d", got)
			}
			currentAdapter.Store(0) // model Cleanup after the run completes
			return "first", nil
		})
		firstDone <- err
	}()
	<-firstEntered

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondStarted := make(chan struct{})
	secondEntered := make(chan struct{}, 1)
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		_, err := gate.run(secondCtx, func() (string, error) {
			secondEntered <- struct{}{}
			currentAdapter.Store(2)
			return "second", nil
		})
		secondDone <- err
	}()
	<-secondStarted
	cancelSecond()

	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued call error = %v, want context.Canceled", err)
	}
	select {
	case <-secondEntered:
		t.Fatal("second runtime entered while the first runtime owned the trace adapter")
	default:
	}
	if got := currentAdapter.Load(); got != 1 {
		t.Fatalf("active trace adapter = %d, want first adapter 1", got)
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first runtime: %v", err)
	}

	// The gate remains reusable after Cleanup releases the singleton window.
	got, err := gate.run(context.Background(), func() (string, error) {
		if !currentAdapter.CompareAndSwap(0, 2) {
			return "", fmt.Errorf("trace adapter still owned by %d", currentAdapter.Load())
		}
		currentAdapter.Store(0)
		return "second-after-cleanup", nil
	})
	if err != nil {
		t.Fatalf("runtime after cleanup: %v", err)
	}
	if got != "second-after-cleanup" {
		t.Fatalf("result = %q, want second-after-cleanup", got)
	}
}
