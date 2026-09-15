package mcp

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// A full stdio pipe blocks Write until transport shutdown closes the endpoint.
// This fake adds a deterministic signal so the deadline test needs no sleeps
// to establish that an unrelated request already owns writeMu.
type computerUseBlockedWriter struct {
	entered chan struct{}
	closed  chan struct{}
	enter   sync.Once
	close   sync.Once
}

func (w *computerUseBlockedWriter) Write([]byte) (int, error) {
	w.enter.Do(func() { close(w.entered) })
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *computerUseBlockedWriter) Close() error {
	w.close.Do(func() { close(w.closed) })
	return nil
}

func TestComputerUseControlDeadlineUnblocksStdioWriter(t *testing.T) {
	for _, action := range []string{"stop", "end-turn", "notification"} {
		t.Run(action, func(t *testing.T) {
			writer := &computerUseBlockedWriter{entered: make(chan struct{}), closed: make(chan struct{})}
			client := NewClient(context.Background(), &StdioTransport{stdin: writer})
			defer client.Close()
			ordinaryDone := make(chan error, 1)
			go func() {
				_, err := client.send(context.Background(), "tools/call", nil)
				ordinaryDone <- err
			}()
			<-writer.entered
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if action == "notification" {
					done <- client.Notify(ctx, "metis-cu/end-turn", nil)
				} else {
					done <- client.ComputerUseControl(ctx, action)
				}
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(300 * time.Millisecond):
				t.Error("lifecycle deadline did not unblock the stdio writer")
				_ = client.Close()
				err = <-done
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("lifecycle error = %v, want deadline exceeded", err)
			}
			if action != "notification" && !strings.Contains(err.Error(), "unconfirmed") {
				t.Errorf("cleanup timeout must be explicit: %v", err)
			}
			select {
			case <-ordinaryDone:
			case <-time.After(time.Second):
				t.Fatal("transport close leaked the original blocked sender")
			}
			client.mu.RLock()
			pending := len(client.pending)
			client.mu.RUnlock()
			if pending != 0 {
				t.Fatalf("lifecycle call returned with %d pending senders", pending)
			}
		})
	}
}

func TestComputerUseControlDeadlineInterruptsOwnStdinWrite(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	client := NewClient(context.Background(), &StdioTransport{stdin: writer})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.ComputerUseControl(ctx, "stop") }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "unconfirmed") {
			t.Fatalf("blocked cleanup error = %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		_ = client.Close()
		<-done
		t.Fatal("cleanup deadline did not interrupt its own pipe write")
	}
	client.mu.RLock()
	defer client.mu.RUnlock()
	if len(client.pending) != 0 {
		t.Fatal("cleanup returned before its blocked sender exited")
	}
}
