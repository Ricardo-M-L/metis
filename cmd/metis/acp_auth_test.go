package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

type blockingACPServerLifecycle struct {
	closed   chan struct{}
	waitDone chan struct{}
	once     sync.Once
}

func newBlockingACPServerLifecycle() *blockingACPServerLifecycle {
	return &blockingACPServerLifecycle{closed: make(chan struct{}), waitDone: make(chan struct{})}
}

func (s *blockingACPServerLifecycle) Wait() { <-s.waitDone }

func (s *blockingACPServerLifecycle) Close() error {
	s.once.Do(func() {
		close(s.closed)
		close(s.waitDone)
	})
	return nil
}

func TestWaitForACPServerClosesOnStdioSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := newBlockingACPServerLifecycle()
	done := make(chan error, 1)
	go func() { done <- waitForACPServer(ctx, srv) }()

	cancel()
	select {
	case <-srv.closed:
	case <-time.After(time.Second):
		t.Fatal("stdio context cancellation did not close ACP server")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitForACPServer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stdio signal path did not return after closing ACP server")
	}
}

func TestPrepareACPLoopDefersMissingCredentialUntilPrompt(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")

	loop, sessionID, cleanup, err := prepareACPLoop(context.Background(), &cliFlags{bare: true, noAuthWizard: true})
	if err != nil {
		t.Fatalf("ACP bootstrap should allow credential-free initialize: %v", err)
	}
	defer cleanup()
	if sessionID != "" {
		t.Fatalf("auth-required fallback sessionID = %q, want empty", sessionID)
	}
	if loop == nil || loop.Provider == nil {
		t.Fatal("ACP bootstrap returned no loop/provider")
	}
	if _, err := loop.Provider.Stream(context.Background(), llm.Request{}); err == nil {
		t.Fatal("model request unexpectedly bypassed missing credentials")
	} else if !errors.Is(err, config.ErrMissingAPIKey) || !strings.Contains(err.Error(), "metis auth login") {
		t.Fatalf("model request error = %v, want actionable missing-key error", err)
	}
}

func TestPrepareACPLoopDoesNotHideInvalidProvider(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	_, _, _, err := prepareACPLoop(context.Background(), &cliFlags{
		bare: true, noAuthWizard: true, provider: "not-a-provider", providerSet: true,
	})
	if err == nil || errors.Is(err, config.ErrMissingAPIKey) || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("invalid provider error = %v", err)
	}
}
