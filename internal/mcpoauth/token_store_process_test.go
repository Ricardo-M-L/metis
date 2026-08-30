package mcpoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

func TestTokenStoreHelperProcess(t *testing.T) {
	if os.Getenv("METIS_MCP_OAUTH_HELPER") != "1" {
		return
	}
	home := os.Getenv("METIS_HOME")
	mode := os.Getenv("METIS_MCP_OAUTH_HELPER_MODE")
	ready := os.Getenv("METIS_MCP_OAUTH_READY")
	start := os.Getenv("METIS_MCP_OAUTH_START")
	markReady := func() {
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(path string) {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", path)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	switch mode {
	case "put":
		markReady()
		waitFor(start)
		key := os.Getenv("METIS_MCP_OAUTH_KEY")
		if err := NewTokenStore().Put(key, &auth.Token{AccessToken: "token-" + key}); err != nil {
			t.Fatal(err)
		}
	case "hold":
		if err := ensurePrivateTokenStoreDir(home); err != nil {
			t.Fatal(err)
		}
		lock, err := acquireTokenStoreFileLock(home, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.release()
		markReady()
		waitFor(start)
	case "ensure":
		markReady()
		waitFor(start)
		serverURL := os.Getenv("METIS_MCP_OAUTH_SERVER_URL")
		token, err := NewTokenStore().EnsureToken(context.Background(), os.Getenv("METIS_MCP_OAUTH_KEY"), serverURL, false)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("TOKEN=" + token)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func helperCommand(home, mode, key, ready, start string, serverURL ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestTokenStoreHelperProcess$")
	cmd.Env = append(os.Environ(),
		"METIS_MCP_OAUTH_HELPER=1",
		"METIS_HOME="+home,
		"METIS_MCP_OAUTH_HELPER_MODE="+mode,
		"METIS_MCP_OAUTH_KEY="+key,
		"METIS_MCP_OAUTH_READY="+ready,
		"METIS_MCP_OAUTH_START="+start,
	)
	if len(serverURL) > 0 {
		cmd.Env = append(cmd.Env, "METIS_MCP_OAUTH_SERVER_URL="+serverURL[0])
	}
	return cmd
}

func waitForFiles(t *testing.T, paths []string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		all := true
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				all = false
				break
			}
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for helper readiness: %v", paths)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTokenStoreConcurrentProcessesPreserveAllEntries(t *testing.T) {
	home := t.TempDir()
	start := filepath.Join(home, "start")
	const processCount = 12
	commands := make([]*exec.Cmd, 0, processCount)
	outputs := make([]bytes.Buffer, processCount)
	ready := make([]string, 0, processCount)
	for i := 0; i < processCount; i++ {
		key := "process-" + strconv.Itoa(i)
		readyPath := filepath.Join(home, key+".ready")
		cmd := helperCommand(home, "put", key, readyPath, start)
		cmd.Stdout = &outputs[i]
		cmd.Stderr = &outputs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		ready = append(ready, readyPath)
	}
	waitForFiles(t, ready)
	if err := os.WriteFile(start, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", i, err, outputs[i].String())
		}
	}

	t.Setenv("METIS_HOME", home)
	for i := 0; i < processCount; i++ {
		key := "process-" + strconv.Itoa(i)
		token, ok, err := NewTokenStore().GetWithError(key)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || token.AccessToken != "token-"+key {
			t.Fatalf("lost cross-process entry %q: token=%+v present=%v", key, token, ok)
		}
	}
}

func TestEnsureTokenConcurrentProcessesRefreshOnlyOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "cross-process-fresh", "refresh_token": "rotated", "expires_in": 3600,
		})
	}))
	defer server.Close()

	entry := boundEntry(server.URL+"/mcp", &auth.Token{
		AccessToken: "expired", RefreshToken: "refresh-once", ExpiresAt: time.Now().Add(-time.Hour),
	})
	entry.Issuer = server.URL
	entry.AuthURL = server.URL + "/authorize"
	entry.TokenURL = server.URL
	if err := NewTokenStore().PutEntry("srv", entry); err != nil {
		t.Fatal(err)
	}

	start := filepath.Join(home, "refresh.start")
	const processCount = 6
	commands := make([]*exec.Cmd, 0, processCount)
	outputs := make([]bytes.Buffer, processCount)
	ready := make([]string, 0, processCount)
	for i := 0; i < processCount; i++ {
		readyPath := filepath.Join(home, fmt.Sprintf("refresh-%d.ready", i))
		cmd := helperCommand(home, "ensure", "srv", readyPath, start, server.URL+"/mcp")
		cmd.Stdout = &outputs[i]
		cmd.Stderr = &outputs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		ready = append(ready, readyPath)
	}
	waitForFiles(t, ready)
	if err := os.WriteFile(start, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("refresh helper %d: %v\n%s", i, err, outputs[i].String())
		}
		if !strings.Contains(outputs[i].String(), "TOKEN=cross-process-fresh") {
			t.Fatalf("refresh helper %d did not return fresh token:\n%s", i, outputs[i].String())
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("cross-process refresh requests = %d, want exactly one", got)
	}
}

func TestTokenStoreLockTimeoutIsDiagnosticAndDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	ready := filepath.Join(home, "holder.ready")
	release := filepath.Join(home, "release")
	cmd := helperCommand(home, "hold", "", ready, release)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFiles(t, []string{ready})

	store := &TokenStore{path: filepath.Join(home, "mcp-oauth.json")}
	err := withTokenStoreLock(store.path, 50*time.Millisecond, func() error {
		return fmt.Errorf("callback unexpectedly ran")
	})
	if !errors.Is(err, errTokenStoreLockTimeout) {
		t.Fatalf("lock contention error = %v, want timeout sentinel", err)
	}
	if _, statErr := os.Stat(store.path); !os.IsNotExist(statErr) {
		t.Fatalf("contended transaction modified store: stat error=%v", statErr)
	}

	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock holder: %v\n%s", err, output.String())
	}
	if err := store.Put("after-release", &auth.Token{AccessToken: "ok"}); err != nil {
		t.Fatalf("lock remained held after helper exit: %v", err)
	}
}

func TestTokenStoreRejectsSymlinkedLockFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	target := filepath.Join(home, "outside-lock")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, tokenStoreLockFilename)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := NewTokenStore().Put("srv", &auth.Token{AccessToken: "secret"})
	if err == nil || !strings.Contains(err.Error(), "symlinked") {
		t.Fatalf("symlinked lock error = %v", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "unchanged" {
		t.Fatalf("symlink target changed: %q", got)
	}
}
