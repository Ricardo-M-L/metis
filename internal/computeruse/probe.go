package computeruse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

// Description probes may need to create an OS sandbox before the helper can
// start. That setup is normally fast, but it becomes noticeably slower when
// several local installs run concurrently or when CI is under CPU pressure.
// Keep a finite upper bound while leaving enough budget for sandbox startup;
// callers can still cancel earlier through the parent context.
const probeTimeout = 10 * time.Second
const maxDescriptionBytes = 64 << 10

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

// Probe executes the description entry point inside the existing credential
// sandbox, with no network or inherited credentials. A helper's claim that
// --describe is side-effect-free is not trusted. The generic OS profile still
// permits ordinary noncredential reads and private temporary writes; it is not
// a dedicated no-GUI profile.
func Probe(ctx context.Context, filename string) (Description, error) {
	return probeWithHome(ctx, filename, os.Getenv("METIS_HOME"))
}

// Managers pass their actual control root even when an embedder did not set
// METIS_HOME. The value configures credential denials, never the child's env.
func probeWithHome(ctx context.Context, filename, metisHome string) (Description, error) {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return Description{}, err
	}
	if err := checkExecutable(absolute); err != nil {
		return Description{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	command, cleanup, err := newDescriptionProbeCommand(ctx, absolute, metisHome)
	if err != nil {
		return Description{}, err
	}
	defer cleanup()
	command.WaitDelay = 250 * time.Millisecond
	stdout := &boundedBuffer{limit: maxDescriptionBytes}
	stderr := &boundedBuffer{limit: 8 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return Description{}, fmt.Errorf("computer-use description probe timed out or was canceled: %w", ctx.Err())
		}
		return Description{}, fmt.Errorf("computer-use description probe failed: %w", err)
	}
	if stdout.overflow || stderr.overflow {
		return Description{}, errors.New("computer-use description probe exceeded output limit")
	}
	var description Description
	decoder := json.NewDecoder(&stdout.buffer)
	if err := decoder.Decode(&description); err != nil {
		return Description{}, fmt.Errorf("invalid computer-use description JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Description{}, errors.New("computer-use description contains trailing data")
	}
	if err := validateDescription(description); err != nil {
		return Description{}, err
	}
	return description, nil
}

func newDescriptionProbeCommand(ctx context.Context, absolute, metisHome string) (*exec.Cmd, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// This manager is independent from interactive permission overrides. Neither
	// ModeOff nor danger-full-access can turn a status/install probe unsandboxed.
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: string(sandbox.ModePermissions), Network: sandbox.NetworkBlock, MetisHome: metisHome})
	if err != nil {
		return nil, nil, fmt.Errorf("prepare computer-use description sandbox: %w", err)
	}
	cleanup := func() { _ = manager.Close() }
	command := exec.CommandContext(ctx, absolute, "--describe", "--json")
	command.Dir = manager.TempDir()
	// Construct an allowlist from fixed values, rather than filtering the full
	// parent environment. Unknown tokens, proxy credentials, runtime injection,
	// shell startup files and desktop handles are all absent by construction.
	command.Env = manager.FilterEnv([]string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + manager.TempDir(),
		"PWD=" + manager.TempDir(),
		"LANG=C", "LC_ALL=C", "TZ=UTC",
	}, false)
	if runtime.GOOS == "linux" {
		// Reuse the existing generic stdio MCP mount profile. It masks desktop
		// IPC; this internal marker is consumed by Wrap, not passed to the helper.
		command.Env = append(command.Env, "METIS_INTERNAL_SANDBOX_PROFILE=stdio-mcp")
	}
	wrapped, err := manager.Wrap(command, sandbox.Request{Cwd: manager.TempDir(), Network: sandbox.NetworkBlock, MinimumMode: sandbox.ModePermissions})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("isolate computer-use description probe: %w", err)
	}
	return wrapped, cleanup, nil
}

func validateDescription(description Description) error {
	if description.Name != "metis-cu" {
		return fmt.Errorf("unexpected computer-use helper name %q", description.Name)
	}
	if description.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("incompatible computer-use protocol %d; expected %d", description.ProtocolVersion, ProtocolVersion)
	}
	if description.Platform != runtime.GOOS || description.Arch != runtime.GOARCH {
		return fmt.Errorf("computer-use helper target %s-%s does not match host %s-%s", description.Platform, description.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if strings.TrimSpace(description.Version) == "" || len(description.Version) > 256 {
		return errors.New("computer-use helper reported an invalid version")
	}
	capabilities := make(map[string]bool, len(description.Capabilities))
	for _, capability := range description.Capabilities {
		capabilities[capability] = true
	}
	for _, required := range []string{"status", "stop", "end-turn", "serialized-input", "input-ownership"} {
		if !capabilities[required] {
			return fmt.Errorf("computer-use helper is missing required capability %q", required)
		}
	}
	return nil
}

func checkExecutable(filename string) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return fmt.Errorf("inspect computer-use executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("computer-use executable must be a regular file, not a symlink or directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
		return errors.New("computer-use helper is not executable")
	}
	return nil
}
