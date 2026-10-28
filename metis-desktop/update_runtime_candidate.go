package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Resolve the candidate's CLI identity before handing rollback ownership to
// the trusted supervisor. The native identity is retained by the transaction.
func desktopUpdateRuntimeSHA(ctx context.Context, appPath, binary, version, goos, goarch, nativeSHA string) (string, error) {
	if binary != "" {
		return desktopFileSHA256(binary)
	}
	if goos == "linux" {
		return desktopLinuxCandidateRuntimeSHA(ctx, appPath, version, nativeSHA)
	}
	if goos != "darwin" {
		return "", errors.New("candidate bundled runtime verification is unsupported on this platform")
	}
	selected, err := verifyMacDesktopRuntime(appPath, version, goarch)
	if err != nil {
		return "", err
	}
	return selected.Manifest.SHA256, nil
}

// Linux carries its CLI inside the new ELF, rather than beside the executable.
// The running old process's embedded FS therefore cannot inspect that runtime.
// Invoke only the candidate's narrow pre-GUI check, which verifies the bundled
// manifest, extracts its CLI and checks the CLI version without provider auth.
func desktopLinuxCandidateRuntimeSHA(ctx context.Context, native, version, nativeSHA string) (string, error) {
	if !validDesktopVersion(version) || !filepath.IsAbs(native) || !runtimeDigest(nativeSHA) {
		return "", errors.New("invalid candidate native runtime identity")
	}
	if err := desktopCandidateExecutableIdentity(native, nativeSHA, desktopUpdateMaxExpand); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, native, "--runtime-check")
	cmd.Env = desktopCandidateRuntimeCheckEnvironment(os.Environ())
	cmd.WaitDelay = time.Second
	var output runtimeVersionOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", fmt.Errorf("candidate bundled runtime check failed: %w", err)
	}
	if output.exceeded {
		return "", errors.New("candidate bundled runtime check output exceeded its capacity")
	}
	var result desktopRuntimeCheckResult
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return "", errors.New("invalid candidate bundled runtime check response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", errors.New("candidate bundled runtime check must return one JSON object")
	}
	if result.Source != "bundled" || !validDesktopVersion(result.Version) || normalizeDesktopVersion(result.Version) != normalizeDesktopVersion(version) || !filepath.IsAbs(result.Path) || filepath.Clean(result.Path) != result.Path || !runtimeDigest(result.SHA256) {
		return "", errors.New("candidate bundled runtime response does not match its identity")
	}
	if err := desktopCandidateExecutableIdentity(result.Path, result.SHA256, desktopRuntimeMaxBytes); err != nil {
		return "", err
	}
	if err := desktopCandidateExecutableIdentity(native, nativeSHA, desktopUpdateMaxExpand); err != nil {
		return "", err
	}
	return result.SHA256, nil
}

func desktopCandidateExecutableIdentity(path, expectedSHA string, maxBytes int64) error {
	f, err := openRegularRuntime(path)
	if err != nil {
		return errors.New("candidate runtime is not a regular executable file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > maxBytes || !executableMetisFile(path, info, "linux") {
		return errors.New("candidate runtime is not an executable within the size limit")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, info.Size()+1))
	if err != nil || n != info.Size() || hex.EncodeToString(hash.Sum(nil)) != expectedSHA {
		return errors.New("candidate runtime checksum changed during verification")
	}
	return nil
}

func desktopCandidateRuntimeCheckEnvironment(environ []string) []string {
	var result []string
	for _, value := range environ {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "PATH", "HOME", "USERPROFILE", "LOCALAPPDATA", "XDG_CACHE_HOME", "TMPDIR", "TEMP", "TMP", "SYSTEMROOT", "WINDIR":
			result = append(result, value)
		}
	}
	return result
}
