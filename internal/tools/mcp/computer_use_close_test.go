package mcp_tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/mcp"
)

type computerUseCloseBarrierTransport struct {
	entered chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (t *computerUseCloseBarrierTransport) Close() error {
	t.once.Do(func() { close(t.entered) })
	<-t.release
	return t.err
}

func TestServerConcurrentCloseWaitsForTransportAndSharesError(t *testing.T) {
	want := errors.New("transport close failed")
	transport := &computerUseCloseBarrierTransport{entered: make(chan struct{}), release: make(chan struct{}), err: want}
	server := &Server{client: mcp.NewClient(context.Background(), transport)}
	first := make(chan error, 1)
	go func() { first <- server.Close() }()
	<-transport.entered
	second := make(chan error, 1)
	go func() { second <- server.Close() }()
	var early error
	earlyReturn := false
	select {
	case early = <-second:
		earlyReturn = true
	case <-time.After(30 * time.Millisecond):
	}
	close(transport.release)
	if err := <-first; !errors.Is(err, want) {
		t.Errorf("first close = %v", err)
	}
	if earlyReturn {
		t.Fatalf("concurrent Close returned before transport cleanup: %v", early)
	}
	if err := <-second; !errors.Is(err, want) {
		t.Errorf("concurrent close = %v, want same cleanup error", err)
	}
}

func newComputerUseCloseProcess(t *testing.T, mode string) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	trace := filepath.Join(dir, "cleanup.log")
	release := filepath.Join(dir, "release")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, err := NewServerWithEnv(ctx, "computer-use", os.Args[0], []string{
		"GO_WANT_CU_CLOSE_HELPER=1", "CU_CLOSE_TRACE=" + trace,
		"CU_CLOSE_RELEASE=" + release, "CU_CLOSE_MODE=" + mode,
	}, "-test.run=^TestComputerUseCloseProcessHelper$")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = server.Close()
	})
	return server, trace, release
}

func TestManagedComputerUseCloseAwaitsNativeCleanupBeforeKilling(t *testing.T) {
	server, trace, release := newComputerUseCloseProcess(t, "wait")
	server.MarkManagedComputerUse()
	first := make(chan error, 1)
	go func() { first <- server.Close() }()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(trace); err == nil && strings.Contains(string(data), "stop received") {
			break
		}
		select {
		case err := <-first:
			t.Fatalf("Close killed helper before its stop request: %v", err)
		case <-deadline:
			t.Fatal("helper never received stop")
		case <-ticker.C:
		}
	}
	second := make(chan error, 1)
	go func() { second <- server.Close() }()
	select {
	case err := <-first:
		t.Fatalf("Close did not await native cleanup: %v", err)
	case err := <-second:
		t.Fatalf("concurrent Close skipped native cleanup: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, done := range []chan error{first, second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Close did not finish after cleanup acknowledgement")
		}
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(trace)
	if err != nil || string(data) != "stop received\ninput released\n" {
		t.Fatalf("native cleanup must occur exactly once before termination: %q, %v", data, err)
	}
}

func TestManagedComputerUseCloseReturnsUnconfirmedCleanup(t *testing.T) {
	server, trace, _ := newComputerUseCloseProcess(t, "reject")
	server.MarkManagedComputerUse()
	err := server.Close()
	if err == nil || !strings.Contains(err.Error(), "unconfirmed") {
		t.Fatalf("rejected cleanup = %v", err)
	}
	if again := server.Close(); again == nil || again.Error() != err.Error() {
		t.Fatalf("later caller lost cleanup failure: %v, first %v", again, err)
	}
	data, readErr := os.ReadFile(trace)
	if readErr != nil || string(data) != "stop received\n" {
		t.Fatalf("rejected cleanup trace = %q, %v", data, readErr)
	}
}

func TestComputerUseNameDoesNotOptInToManagedCleanup(t *testing.T) {
	server, trace, _ := newComputerUseCloseProcess(t, "wait")
	if err := server.ComputerUseControl(context.Background(), "stop"); err == nil {
		t.Fatal("custom server acquired lifecycle role from its name")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("custom server received native cleanup control: %v", err)
	}
}

func TestManagedComputerUseCloseNeverSpawnsLazyServer(t *testing.T) {
	server := NewLazyServer("computer-use", nil, func(context.Context) (*mcp.Client, error) {
		t.Error("Close spawned unused Computer Use helper")
		return nil, errors.New("unexpected spawn")
	})
	server.MarkManagedComputerUse()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedComputerUseDirectServerProfileRequiresVerification(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	for _, command := range []string{"metis-cu", filepath.Join(t.TempDir(), "metis-cu")} {
		server, err := NewServerWithEnvAndDirAndSandboxProfile(context.Background(), "computer-use", command, nil, "", nil, mcp.StdioSandboxProfileManagedComputerUse)
		if server != nil || err == nil || !strings.Contains(err.Error(), "managed Computer Use") {
			t.Fatalf("direct server accepted an unverified managed profile: %v, %v", server, err)
		}
	}
}

// Run as a real stdio MCP child so the test observes cleanup before the
// process-group kill in StdioTransport.Close, rather than mocking that order.
func TestComputerUseCloseProcessHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CU_CLOSE_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request mcp.JSONRPCRequest
		if json.Unmarshal(scanner.Bytes(), &request) != nil || request.ID == nil {
			continue
		}
		var result any = map[string]any{}
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcp.MCPProtocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "metis-cu", "version": "test"}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		case "metis-cu/stop":
			trace := os.Getenv("CU_CLOSE_TRACE")
			if err := os.WriteFile(trace, []byte("stop received\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if os.Getenv("CU_CLOSE_MODE") == "reject" {
				result = map[string]bool{"stopped": true, "cleaned": false}
				break
			}
			for {
				if _, err := os.Stat(os.Getenv("CU_CLOSE_RELEASE")); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := os.WriteFile(trace, []byte("stop received\ninput released\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			result = map[string]bool{"stopped": true, "cleaned": true}
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			return
		}
	}
}
