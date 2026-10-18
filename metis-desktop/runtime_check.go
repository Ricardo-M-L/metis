package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type desktopRuntimeCheckResult struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}
type desktopRuntimeVersionRunner func(context.Context, string) (string, error)

// desktopRuntimeCheckCommand is an intentionally narrow package acceptance
// command. It does not construct a Wails App, load provider configuration or
// resolve a user-supplied external CLI. The only child command is `version`.
func desktopRuntimeCheckCommand(args []string, out io.Writer, resolver *desktopRuntimeResolver, runner desktopRuntimeVersionRunner) (bool, error) {
	if len(args) < 2 {
		return false, nil
	}
	requested := false
	for _, arg := range args[1:] {
		if arg == "--runtime-check" {
			requested = true
		}
	}
	if !requested {
		return false, nil
	}
	if len(args) != 2 || args[1] != "--runtime-check" {
		return true, errors.New("--runtime-check must be used without other arguments")
	}
	if resolver == nil {
		resolver = newDesktopRuntimeResolver((&App{}).GetVersion())
	}
	selected, err := resolver.requireBundled()
	if err != nil {
		return true, err
	}
	if runner == nil {
		runner = func(ctx context.Context, binary string) (string, error) {
			return runDesktopRuntimeVersion(ctx, binary, resolver.executable)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := runner(ctx, selected.Path)
	if err != nil {
		return true, fmt.Errorf("bundled Metis runtime version check failed: %w", err)
	}
	fields := strings.Fields(output)
	if len(fields) == 0 || normalizeDesktopVersion(fields[0]) != normalizeDesktopVersion(selected.Manifest.Version) {
		return true, errors.New("bundled Metis runtime reported a version different from its signed package manifest")
	}
	if err = verifyDesktopRuntimeFile(selected.Path, selected.Manifest, resolver.goos); err != nil {
		return true, err
	}
	return true, json.NewEncoder(out).Encode(desktopRuntimeCheckResult{Source: "bundled", Version: normalizeDesktopVersion(selected.Manifest.Version), Path: selected.Path, SHA256: selected.Manifest.SHA256})
}
func runDesktopRuntimeVersion(ctx context.Context, binary, host string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, "version")
	// Version needs no provider credentials. Pass only OS location variables
	// needed by the Go runtime and the bundled-update ownership guard.
	env := []string{}
	for _, key := range []string{"PATH", "HOME", "USERPROFILE", "LOCALAPPDATA", "XDG_CACHE_HOME", "TMPDIR", "TEMP", "TMP", "SYSTEMROOT", "WINDIR"} {
		if value, exists := os.LookupEnv(key); exists {
			env = append(env, key+"="+value)
		}
	}
	cmd.Env = desktopRuntimeChildEnvironment(env, host, true)
	cmd.WaitDelay = time.Second
	var output runtimeVersionOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if output.exceeded {
		return "", errors.New("bundled runtime version output exceeded its capacity")
	}
	return output.String(), nil
}

type runtimeVersionOutput struct {
	bytes.Buffer
	exceeded bool
}

func (b *runtimeVersionOutput) Write(data []byte) (int, error) {
	n := len(data)
	remaining := (64 << 10) - b.Len()
	if n > remaining {
		data = data[:max(0, remaining)]
		b.exceeded = true
	}
	_, _ = b.Buffer.Write(data)
	return n, nil
}
