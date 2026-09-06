package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestMCPServerShutdownJoinsRequestCleanup(t *testing.T) {
	t.Setenv("METIS_RUN_MAX_SECONDS", "0")
	for _, trigger := range []string{"parent cancellation", "input EOF"} {
		t.Run(trigger, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				input, client := io.Pipe()
				defer input.Close()
				defer client.Close()
				started := make(chan struct{}, 2)
				cancelled := make(chan struct{}, 2)
				cleaned := make(chan struct{}, 2)
				releaseCleanup := make(chan struct{})
				runTask := func(ctx context.Context, _ *cliFlags, _ string) (string, error) {
					// Model the runner's checkpoint/Cleanup defer: cancellation
					// has happened, but the request is not yet safe to abandon.
					defer func() { cleaned <- struct{}{} }()
					started <- struct{}{}
					<-ctx.Done()
					cancelled <- struct{}{}
					<-releaseCleanup
					return "", ctx.Err()
				}
				serverDone := make(chan error, 1)
				go func() { serverDone <- serveMCP(ctx, &cliFlags{}, input, io.Discard, runTask) }()
				for id := 1; id <= 2; id++ {
					if _, err := fmt.Fprintf(client, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"tools/call\",\"params\":{\"name\":\"run_task\",\"arguments\":{\"prompt\":\"test\"}}}\n", id); err != nil {
						t.Fatal(err)
					}
					<-started
				}
				if trigger == "input EOF" {
					if err := client.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
				synctest.Wait()
				if len(cancelled) != 2 {
					t.Fatalf("cancelled %d requests, want both", len(cancelled))
				}
				select {
				case err := <-serverDone:
					t.Fatalf("server returned before request cleanup: %v", err)
				default:
				}
				close(releaseCleanup)
				if err := <-serverDone; err != nil {
					t.Fatal(err)
				}
				if len(cleaned) != 2 {
					t.Fatalf("server returned with only %d requests cleaned up", len(cleaned))
				}
			})
		})
	}
}

func TestMCPServerCompletedRequestDoesNotBlockShutdown(t *testing.T) {
	t.Setenv("METIS_RUN_MAX_SECONDS", "0")
	synctest.Test(t, func(t *testing.T) {
		input, client := io.Pipe()
		defer input.Close()
		defer client.Close()
		responses, output := io.Pipe()
		defer responses.Close()
		defer output.Close()
		serverDone := make(chan error, 1)
		go func() {
			serverDone <- serveMCP(context.Background(), &cliFlags{}, input, output,
				func(_ context.Context, _ *cliFlags, prompt string) (string, error) { return prompt, nil })
		}()
		if _, err := io.WriteString(client, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"run_task\",\"arguments\":{\"prompt\":\"finished\"}}}\n"); err != nil {
			t.Fatal(err)
		}
		var response struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		if err := json.NewDecoder(responses).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Result.IsError || len(response.Result.Content) != 1 || response.Result.Content[0].Text != "finished" {
			t.Fatalf("unexpected task response: %+v", response)
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-serverDone; err != nil {
			t.Fatal(err)
		}
	})
}

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
