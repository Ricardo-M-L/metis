package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type cleanupBlockingMemoryProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (*cleanupBlockingMemoryProvider) Name() string          { return "cleanup-blocking-memory" }
func (*cleanupBlockingMemoryProvider) ModelID() string       { return "test" }
func (*cleanupBlockingMemoryProvider) MaxContextTokens() int { return 200_000 }
func (p *cleanupBlockingMemoryProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (*cleanupBlockingMemoryProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("stream not used")
}

func TestRuntimeCleanupWaitsForAutoMemoryFilesystemLifecycle(t *testing.T) {
	provider := &cleanupBlockingMemoryProvider{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	loop := agent.NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Model = "test"
	loop.AutoMemory = true
	loop.AppendUser("remember this cleanup lifecycle fact")

	root := filepath.Join(t.TempDir(), "memory")
	extractor, err := agent.NewAutoMemoryExtractor(loop, root, filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatalf("NewAutoMemoryExtractor: %v", err)
	}
	sessions := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three", "four"} {
		if err := os.WriteFile(filepath.Join(sessions, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	extractor.SetSessionsDir(sessions)
	extractor.OnLoopEnd(context.Background(), "end_turn")
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Auto Memory provider did not start")
	}

	done := make(chan struct{})
	go func() {
		(&runtime{loop: loop}).Cleanup()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("runtime cleanup returned while Auto Memory was still active")
	case <-time.After(30 * time.Millisecond):
	}
	close(provider.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime cleanup did not join completed Auto Memory work")
	}
}
