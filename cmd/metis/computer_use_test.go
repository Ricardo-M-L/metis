package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/computeruse"
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

func TestComputerUseStatusUsesListedResourceHandle(t *testing.T) {
	for _, state := range []string{"idle", "running", "stopping", "stopped"} {
		t.Run(state, func(t *testing.T) {
			home := cuTestHome(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			manager := installComputerUseStatusFixture(t, ctx, home)
			server, err := mcptools.NewServerWithEnv(ctx, "computer-use", os.Args[0], []string{
				"METIS_CU_STATUS_FIXTURE=1", "METIS_CU_STATUS_STATE=" + state,
			}, "-test.run=^TestComputerUseStatusFixture$")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			server.MarkManagedComputerUse()
			rt := &runtime{mcpServers: []*mcptools.Server{server}}
			status, err := rt.computerUseStatus(ctx, manager)
			if err != nil {
				t.Fatal(err)
			}
			wantRunning := state == "idle" || state == "running"
			if status.Running != wantRunning || status.Description == nil {
				t.Fatalf("native state %s: running=%t description=%v message=%s", state, status.Running, status.Description, status.Message)
			}
		})
	}
}

func installComputerUseStatusFixture(t *testing.T, ctx context.Context, home string) *computeruse.Manager {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("status installer fixture uses a POSIX shell script")
	}
	description, err := json.Marshal(computeruse.Description{
		Name:            "metis-cu",
		Version:         "test",
		ProtocolVersion: computeruse.ProtocolVersion,
		Platform:        goruntime.GOOS,
		Arch:            goruntime.GOARCH,
		Capabilities:    []string{"status", "stop", "end-turn", "serialized-input", "input-ownership"},
		Permissions:     map[string]string{"accessibility": "notGranted", "screenRecording": "notGranted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "metis-cu")
	script := "#!/bin/sh\n[ \"$1\" = --describe ] && [ \"$2\" = --json ] || exit 2\nprintf '%s\\n' '" + string(description) + "'\n"
	if err := os.WriteFile(filename, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := computeruse.New(home)
	if _, err := manager.InstallLocal(ctx, filename); err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestComputerUseStatusWithoutResourceRequiresVerifiedLiveConnection(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		managed     bool
		installed   bool
		wantRunning bool
		wantMessage string
	}{
		{"official-0.0.2-method-not-found", "unsupported", true, true, true, "does not expose detailed lifecycle status"},
		{"unmanaged-same-name", "unsupported", false, true, false, ""},
		{"no-verified-installation", "unsupported", true, false, false, ""},
		{"legacy-prompt-list-missing", "unsupported-no-prompts", true, true, false, "not healthy"},
		{"legacy-prompt-list-incomplete", "unsupported-incomplete-prompts", true, true, false, "not healthy"},
		{"legacy-prompt-list-error", "unsupported-prompt-error", true, true, false, "not healthy"},
		{"legacy-prompt-list-timeout", "unsupported-no-response", true, true, false, "not healthy"},
		{"legacy-transport-disconnected", "unsupported-disconnect", true, true, false, "not healthy"},
		{"other-rpc-error", "rpc-error", true, true, false, "not healthy"},
		{"empty-successful-catalog", "empty", true, true, false, "not healthy"},
		{"null-catalog-without-legacy-prompts", "null", true, true, false, "not healthy"},
		{"malformed-status-resource", "malformed", true, true, false, "not healthy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := cuTestHome(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			manager := computeruse.New(home)
			if test.installed {
				manager = installComputerUseStatusFixture(t, ctx, home)
			}
			server, err := mcptools.NewServerWithEnv(ctx, "computer-use", os.Args[0], []string{
				"METIS_CU_STATUS_FIXTURE=1", "METIS_CU_STATUS_MODE=" + test.mode,
			}, "-test.run=^TestComputerUseStatusFixture$")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			if test.managed {
				server.MarkManagedComputerUse()
			}
			status, err := (&runtime{mcpServers: []*mcptools.Server{server}}).computerUseStatus(ctx, manager)
			if err != nil {
				t.Fatal(err)
			}
			if status.Running != test.wantRunning {
				t.Fatalf("running=%t, want %t; message=%q", status.Running, test.wantRunning, status.Message)
			}
			if test.wantMessage != "" && !strings.Contains(status.Message, test.wantMessage) {
				t.Fatalf("message=%q, want substring %q", status.Message, test.wantMessage)
			}
			if test.wantRunning && (status.Description == nil || status.Description.Version != "test") {
				t.Fatalf("verified installation description missing: %+v", status.Description)
			}
		})
	}
}

func TestComputerUseStatusFixture(t *testing.T) {
	if os.Getenv("METIS_CU_STATUS_FIXTURE") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	writer := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || request.ID == nil {
			continue
		}
		var result any = map[string]any{}
		var rpcError any
		mode := os.Getenv("METIS_CU_STATUS_MODE")
		switch request.Method {
		case "initialize":
			capabilities := map[string]any{"tools": map[string]any{}}
			if strings.HasPrefix(mode, "unsupported") {
				capabilities["prompts"] = map[string]any{}
			} else {
				capabilities["resources"] = map[string]any{}
			}
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": capabilities, "serverInfo": map[string]any{"name": "metis-cu", "version": "test"}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		case "resources/list":
			switch {
			case strings.HasPrefix(mode, "unsupported"):
				rpcError = map[string]any{"code": -32601, "message": "Method not found"}
			case mode == "rpc-error":
				rpcError = map[string]any{"code": -32000, "message": "status probe failed"}
			case mode == "empty":
				result = map[string]any{"resources": []any{}}
			case mode == "null":
				result = map[string]any{"resources": nil}
			default:
				result = map[string]any{"resources": []any{map[string]any{"uri": "metis-cu://status", "name": "Computer use status", "mimeType": "application/json"}}}
			}
		case "prompts/list":
			switch mode {
			case "unsupported-prompt-error":
				rpcError = map[string]any{"code": -32000, "message": "prompt probe failed"}
			case "unsupported-no-prompts", "null":
				rpcError = map[string]any{"code": -32601, "message": "Method not found"}
			case "unsupported-incomplete-prompts":
				result = map[string]any{"prompts": []any{map[string]any{"name": "computer_use_minimal"}}}
			case "unsupported-no-response":
				continue
			case "unsupported-disconnect":
				os.Exit(0)
			default:
				result = map[string]any{"prompts": []any{map[string]any{"name": "computer_use_minimal"}, map[string]any{"name": "tier_overview"}, map[string]any{"name": "safe_browse"}}}
			}
		case "resources/read":
			if request.Params["uri"] != "metis-cu://status" {
				os.Exit(2)
			}
			descriptor, _ := json.Marshal(map[string]any{"name": "metis-cu", "protocolVersion": 1, "version": "test", "lifecycle": map[string]any{"state": os.Getenv("METIS_CU_STATUS_STATE")}})
			if mode == "malformed" {
				descriptor = []byte(`{"name":"other","protocolVersion":1,"lifecycle":{"state":"running"}}`)
			}
			result = map[string]any{"contents": []any{map[string]any{"uri": "metis-cu://status", "mimeType": "application/json", "text": string(descriptor)}}}
		case "metis-cu/stop", "metis-cu/end-turn":
			result = map[string]any{"stopped": true, "cleaned": true}
		}
		if rpcError != nil {
			_ = writer.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": rpcError})
		} else {
			_ = writer.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		}
	}
	os.Exit(0)
}

func cuTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func cuTestLazyServer(name string, spawned *atomic.Int32) *mcptools.Server {
	return mcptools.NewLazyServer(name, []mcpsdk.Tool{{Name: "probe"}}, func(context.Context) (*mcpsdk.Client, error) {
		spawned.Add(1)
		return nil, errors.New("unexpected lazy spawn")
	})
}

func assertCUClosedWithoutSpawn(t *testing.T, server *mcptools.Server, spawned *atomic.Int32) {
	t.Helper()
	result, err := server.Tools()[0].Execute(context.Background(), nil)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("retained reference to stopped server was not rejected: result=%+v err=%v", result, err)
	}
	if got := spawned.Load(); got != 0 {
		t.Fatalf("stopping or rejecting an unused helper spawned it %d times", got)
	}
}

func TestComputerUseStopRemovesOnlyOwnedToolsAndPrompts(t *testing.T) {
	cuTestHome(t)
	var cuSpawns, otherSpawns atomic.Int32
	cu := cuTestLazyServer(mcp.ReservedComputerUseName, &cuSpawns)
	other := cuTestLazyServer("other", &otherSpawns)
	t.Cleanup(func() { _ = other.Close() })
	rt := &runtime{registry: tools.NewRegistry(), slashRegistry: slash.NewRegistry()}
	rt.adoptMCPServer(cu, cu.Tools(), false)
	rt.adoptMCPServer(other, other.Tools(), false)
	rt.slashRegistry.Register(slash.Cmd{Name: "cu-prompt", Source: "mcp:computer-use"})
	rt.slashRegistry.Register(slash.Cmd{Name: "other-prompt", Source: "mcp:other"})
	canceled := make(chan struct{})
	rt.computerUseCancel = func() { close(canceled) }

	if err := rt.stopComputerUse(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("stop did not cancel the in-flight Computer Use launch")
	}
	if _, ok := rt.registry.Get("mcp__computer-use__probe"); ok {
		t.Fatal("Computer Use tool remained callable after stop")
	}
	if _, ok := rt.registry.Get("mcp__other__probe"); !ok {
		t.Fatal("stop removed an unrelated MCP tool")
	}
	if _, ok := rt.slashRegistry.Resolve("cu-prompt"); ok {
		t.Fatal("Computer Use prompt retained a stopped server closure")
	}
	if _, ok := rt.slashRegistry.Resolve("other-prompt"); !ok {
		t.Fatal("stop removed an unrelated MCP prompt")
	}
	if len(rt.mcpServers) != 1 || rt.mcpServers[0] != other {
		t.Fatalf("unrelated server ownership changed: %#v", rt.mcpServers)
	}
	assertCUClosedWithoutSpawn(t, cu, &cuSpawns)
	if err := rt.stopComputerUse(); err != nil {
		t.Fatalf("repeated stop failed: %v", err)
	}
	if otherSpawns.Load() != 0 {
		t.Fatal("stop spawned an unrelated lazy server")
	}
}

func TestComputerUseStopRejectsLateStartupAndEnable(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "enable"}[explicit], func(t *testing.T) {
			var spawns atomic.Int32
			late := cuTestLazyServer(mcp.ReservedComputerUseName, &spawns)
			rt := &runtime{registry: tools.NewRegistry(), computerUseGeneration: 7}
			epoch, generation := rt.currentMCPLaunchEpoch(), rt.computerUseGeneration
			started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				close(started)
				<-release // a handshake that completes only after Stop returns
				if explicit {
					rt.adoptMCPServerAtGeneration(late, late.Tools(), true, epoch, &generation)
				} else {
					rt.adoptMCPServerAtEpoch(late, late.Tools(), false, epoch)
				}
				close(finished)
			}()
			<-started
			err := rt.stopComputerUse()
			close(release)
			<-finished
			if err != nil {
				t.Fatal(err)
			}
			if len(rt.computerUseServers()) != 0 || len(rt.registry.All()) != 0 {
				t.Fatal("late handshake republished Computer Use after stop")
			}
			assertCUClosedWithoutSpawn(t, late, &spawns)
		})
	}
}

func TestComputerUseOldEnableCannotReplaceNewGeneration(t *testing.T) {
	var staleSpawns, freshSpawns atomic.Int32
	stale := cuTestLazyServer(mcp.ReservedComputerUseName, &staleSpawns)
	fresh := cuTestLazyServer(mcp.ReservedComputerUseName, &freshSpawns)
	rt := &runtime{registry: tools.NewRegistry(), computerUseGeneration: 3}
	t.Cleanup(func() { _ = rt.stopComputerUse() })
	oldGeneration := uint64(2)
	currentGeneration := uint64(3)
	// A newer explicit enable has reopened admission. The old enable must be
	// rejected by its captured generation, not merely a global stopped bit.
	if !rt.adoptMCPServerAtGeneration(fresh, fresh.Tools(), true, rt.currentMCPLaunchEpoch(), &currentGeneration) {
		t.Fatal("current-generation enable was not adopted")
	}
	if !rt.adoptMCPServerAtGeneration(stale, stale.Tools(), true, rt.currentMCPLaunchEpoch(), &oldGeneration) {
		t.Fatal("stale enable result was not consumed")
	}
	if servers := rt.computerUseServers(); len(servers) != 1 || servers[0] != fresh {
		t.Fatalf("stale enable replaced fresh server: %#v", servers)
	}
	assertCUClosedWithoutSpawn(t, stale, &staleSpawns)
	if freshSpawns.Load() != 0 {
		t.Fatal("generation comparison unnecessarily spawned the fresh helper")
	}
}

func TestComputerUseStopAndDisableHaveDistinctPersistence(t *testing.T) {
	for _, action := range []string{"stop", "disable"} {
		t.Run(action, func(t *testing.T) {
			cuTestHome(t)
			other := mcp.ServerEntry{Name: "other", Command: "custom-other", Args: []string{"--keep"}}
			reg := &mcp.Registry{Servers: []mcp.ServerEntry{
				{Name: mcp.ReservedComputerUseName, Command: mcp.ManagedComputerUseCommand}, other,
			}}
			if err := mcp.Save(reg); err != nil {
				t.Fatal(err)
			}
			var spawns atomic.Int32
			cu := cuTestLazyServer(mcp.ReservedComputerUseName, &spawns)
			rt := &runtime{registry: tools.NewRegistry()}
			rt.adoptMCPServer(cu, cu.Tools(), false)
			status, err := rt.computerUseAction(context.Background(), action)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := mcp.Load()
			if err != nil {
				t.Fatal(err)
			}
			entry := mcp.FindServer(loaded, mcp.ReservedComputerUseName)
			wantDisabled := action == "disable"
			if entry == nil || entry.Disabled != wantDisabled || status.Enabled == wantDisabled || status.Running {
				t.Fatalf("%s persistence/readiness mismatch: entry=%+v status=%+v", action, entry, status)
			}
			if preserved := mcp.FindServer(loaded, "other"); preserved == nil || !reflect.DeepEqual(*preserved, other) {
				t.Fatalf("%s altered unrelated config: %+v", action, preserved)
			}
			assertCUClosedWithoutSpawn(t, cu, &spawns)
		})
	}
}

func TestComputerUseEndTurnDoesNotSpawnUnusedHelper(t *testing.T) {
	var spawns atomic.Int32
	cu := cuTestLazyServer(mcp.ReservedComputerUseName, &spawns)
	rt := &runtime{registry: tools.NewRegistry()}
	rt.adoptMCPServer(cu, cu.Tools(), false)
	t.Cleanup(func() { _ = rt.stopComputerUse() })
	rt.endComputerUseTurn()
	if spawns.Load() != 0 || len(rt.computerUseServers()) != 1 {
		t.Fatal("end-turn spawned or removed an unused helper")
	}
}

func TestComputerUseCLIDispatchWithoutProviderBootstrap(t *testing.T) {
	for _, action := range []string{"help", "status"} {
		t.Run(action, func(t *testing.T) {
			home := cuTestHome(t)
			// Any provider bootstrap would reject this configuration. CU help
			// and status must route without reading it or starting an LLM.
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("invalid = ["), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := os.CreateTemp(t.TempDir(), "cu-output-")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			original := os.Stdout
			os.Stdout = output
			t.Cleanup(func() { os.Stdout = original })
			args := []string{"cu", action}
			if action == "status" {
				args = append(args, "--json")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err = dispatch(ctx, args)
			os.Stdout = original
			if err != nil {
				t.Fatalf("CU dispatch entered provider/bootstrap path: %v", err)
			}
			if _, err := os.Stat(filepath.Join(home, "mcp.toml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read-only CU dispatch created MCP config: %v", err)
			}
			if action == "status" {
				data, err := os.ReadFile(output.Name())
				if err != nil {
					t.Fatal(err)
				}
				var status computeruse.Status
				if err := json.Unmarshal(data, &status); err != nil {
					t.Fatalf("status was not structured CU output: %s (%v)", data, err)
				}
				if status.Installed || status.Enabled || status.Running {
					t.Fatalf("fresh home reported ready: %+v", status)
				}
			}
		})
	}
}
