//go:build !windows

package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestRegisterWithSandboxInjectsManagerIntoLSP(t *testing.T) {
	dir := t.TempDir()
	writeExecutableForLSPTest(t, filepath.Join(dir, "gopls"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	registry := tools.NewRegistry()
	RegisterWithDirsAndSandbox(
		registry,
		&config.Config{},
		permission.New(permission.ModeBypass),
		filepath.Join(dir, "skills"),
		filepath.Join(dir, "sessions"),
		manager,
	)
	registered, ok := registry.Get("LSP")
	if !ok {
		t.Fatal("LSP was not registered with fake gopls on PATH")
	}
	lsp, ok := registered.(LSP)
	if !ok {
		t.Fatalf("registered LSP type = %T", registered)
	}
	if lsp.SandboxManager() != manager {
		t.Fatal("registered LSP did not receive the shared Manager")
	}
}

func TestLSPClosedSandboxManagerFailsClosedBeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "must-not-run")
	gopls := filepath.Join(dir, "gopls")
	writeExecutableForLSPTest(t, gopls, fmt.Sprintf("#!/bin/sh\nprintf ran > %q\n", sentinel))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	tool := NewLSPWithSandbox(permission.New(permission.ModeBypass), manager)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "definition",
		"path":   filepath.Join(dir, "main.go"),
		"line":   1,
		"column": 1,
	})
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "sandbox wrap failed") || !strings.Contains(res.Output, sandbox.ErrManagerClosed.Error()) {
		t.Fatalf("closed-manager result = %#v", res)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gopls ran after sandbox failure: stat error = %v", err)
	}

	source := filepath.Join(dir, "main.py")
	if err := os.WriteFile(source, []byte("value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = runStdioLSPQueryWithSandbox(
		context.Background(),
		lspServer{cmd: gopls, languageID: "python"},
		"hover",
		source,
		1,
		1,
		manager,
	)
	if err != nil {
		t.Fatalf("runStdioLSPQueryWithSandbox returned Go error: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "sandbox wrap failed") {
		t.Fatalf("stdio closed-manager result = %#v", res)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stdio server ran after sandbox failure: stat error = %v", err)
	}
}

func TestLSPCancellationKillsGoplsAndStdioDescendants(t *testing.T) {
	t.Run("gopls", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "child.pid")
		writeForkingLSPServer(t, filepath.Join(dir, "gopls"), pidFile)
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		source := filepath.Join(dir, "main.go")
		if err := os.WriteFile(source, []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			res, err := runGoplsQuery(ctx, "definition", source, 1, 1)
			if err == nil && (res == nil || !res.IsError) {
				err = fmt.Errorf("unexpected gopls cancellation result: %#v", res)
			}
			result <- err
		}()
		childPID := waitForLSPHelperReady(t, pidFile, result)
		cancel()
		waitForLSPQueryReturn(t, result)
		assertLSPProcessGone(t, childPID)
	})

	t.Run("stdio", func(t *testing.T) {
		dir := t.TempDir()
		pidFile := filepath.Join(dir, "child.pid")
		server := filepath.Join(dir, "fake-lsp")
		writeForkingLSPServer(t, server, pidFile)
		source := filepath.Join(dir, "main.py")
		if err := os.WriteFile(source, []byte("value = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			res, err := runStdioLSPQuery(ctx, lspServer{cmd: server, languageID: "python"}, "hover", source, 1, 1)
			if err == nil && (res == nil || !res.IsError) {
				err = fmt.Errorf("unexpected stdio cancellation result: %#v", res)
			}
			result <- err
		}()
		childPID := waitForLSPHelperReady(t, pidFile, result)
		cancel()
		waitForLSPQueryReturn(t, result)
		assertLSPProcessGone(t, childPID)
	})
}

func writeForkingLSPServer(t *testing.T, path, pidFile string) {
	t.Helper()
	writeExecutableForLSPTest(t, path, fmt.Sprintf(
		"#!/bin/sh\nsleep 30 &\nchild=$!\nprintf 'ready:%%s\\n' \"$child\" > %q\nwait\n",
		pidFile,
	))
}

func writeExecutableForLSPTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

const lspHelperReadyDeadline = 10 * time.Second

// waitForLSPHelperReady distinguishes a helper that failed before spawning its
// descendant from a healthy helper whose first scheduling slice was delayed by
// a loaded race/CI host. Cancellation and tree reaping are still verified by
// their independent, much shorter deadlines below.
func waitForLSPHelperReady(t *testing.T, path string, queryDone <-chan error) int {
	t.Helper()
	timer := time.NewTimer(lspHelperReadyDeadline)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			ready := strings.TrimSpace(string(raw))
			pidText, markedReady := strings.CutPrefix(ready, "ready:")
			pid, parseErr := strconv.Atoi(pidText)
			if markedReady && parseErr == nil && pid > 0 {
				t.Cleanup(func() {
					if process, findErr := os.FindProcess(pid); findErr == nil {
						_ = process.Kill()
					}
				})
				return pid
			}
		}
		select {
		case err := <-queryDone:
			t.Fatalf("LSP query returned before helper ready marker: %v", err)
		case <-timer.C:
			t.Fatalf("LSP helper did not publish ready marker to %s within %s", path, lspHelperReadyDeadline)
		case <-ticker.C:
		}
	}
}

func waitForLSPQueryReturn(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("LSP cancellation did not return within the bounded wait")
	}
}

func assertLSPProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("LSP descendant pid %d is still alive after cancellation (probe: %v)", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
