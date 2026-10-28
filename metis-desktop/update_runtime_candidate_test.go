package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDesktopUpdateLinuxBundledRuntimeUsesCandidateIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable fixture; Windows supervisor remains unsupported")
	}
	t.Setenv("OPENAI_API_KEY", "candidate-parent-secret")
	t.Setenv("METIS_BIN", "/obsolete/runtime")
	t.Setenv("METIS_DESKTOP_APP", "/obsolete/app")
	root := t.TempDir()
	cli := filepath.Join(root, "metis")
	if err := os.WriteFile(cli, []byte("new candidate runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	cliSHA, err := desktopFileSHA256(cli)
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(desktopRuntimeCheckResult{Source: "bundled", Version: "1.2.3", Path: cli, SHA256: cliSHA})
	if err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(root, "metis-desktop")
	script := "#!/bin/sh\nset -eu\n[ \"$#\" -eq 1 ] && [ \"$1\" = --runtime-check ]\n[ -z \"${OPENAI_API_KEY+x}\" ] && [ -z \"${METIS_BIN+x}\" ] && [ -z \"${METIS_DESKTOP_APP+x}\" ]\nprintf '%s\\n' '" + string(result) + "'\n"
	if err := os.WriteFile(native, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	nativeSHA, err := desktopFileSHA256(native)
	if err != nil {
		t.Fatal(err)
	}
	got, err := desktopUpdateRuntimeSHA(context.Background(), native, "", "1.2.3", "linux", runtime.GOARCH, nativeSHA)
	if err != nil || got != cliSHA {
		t.Fatalf("candidate's bundled Linux CLI identity = %q, error=%v; want %s (no macOS bundle exists)", got, err, cliSHA)
	}
}

func TestDesktopUpdateLinuxBundledRuntimeRejectsInvalidCandidate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable fixtures; Windows supervisor remains unsupported")
	}
	for _, mode := range []string{"source", "version", "digest", "digest_format", "missing", "symlink", "nonexec", "relative", "extra_field", "extra_object", "nonzero", "overflow", "timeout", "native_changed", "cli_changed", "empty_cli", "oversized_cli", "native_digest", "native_symlink"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cli := filepath.Join(root, "metis")
			if err := os.WriteFile(cli, []byte("candidate CLI identity"), 0700); err != nil {
				t.Fatal(err)
			}
			cliSHA, err := desktopFileSHA256(cli)
			if err != nil {
				t.Fatal(err)
			}
			result := desktopRuntimeCheckResult{Source: "bundled", Version: "1.2.3", Path: cli, SHA256: cliSHA}
			switch mode {
			case "source":
				result.Source = "external"
			case "version":
				result.Version = "9.9.9"
			case "digest":
				result.SHA256 = strings.Repeat("a", 64)
			case "digest_format":
				result.SHA256 = "not-a-digest"
			case "missing":
				result.Path += ".missing"
			case "relative":
				result.Path = "metis"
			case "symlink":
				result.Path += ".link"
				if err = os.Symlink(cli, result.Path); err != nil {
					t.Fatal(err)
				}
			case "nonexec":
				err = os.Chmod(cli, 0600)
			case "empty_cli":
				err = os.Truncate(cli, 0)
			case "oversized_cli":
				err = os.Truncate(cli, desktopRuntimeMaxBytes+1)
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "extra_field" {
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"unexpected":true}`)
			}
			if mode == "extra_object" {
				raw = append(raw, []byte("\n{}")...)
			}
			quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'" }
			script := "#!/bin/sh\nset -eu\n[ \"$#\" -eq 1 ] && [ \"$1\" = --runtime-check ]\nprintf '%s\\n' " + quote(string(raw)) + "\n"
			switch mode {
			case "nonzero":
				script += "printf '%s' 'candidate-stderr-secret' >&2\nexit 7\n"
			case "overflow":
				script += "printf '%s' " + quote(strings.Repeat("x", 65<<10)) + "\n"
			case "timeout":
				script += "exec /bin/sleep 30\n"
			case "native_changed":
				script += "printf x >> \"$0\"\n"
			case "cli_changed":
				script += "printf changed > " + quote(cli) + "\n"
			}
			native := filepath.Join(root, "candidate desktop")
			if err = os.WriteFile(native, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			nativeSHA, err := desktopFileSHA256(native)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "native_digest" {
				nativeSHA = strings.Repeat("b", 64)
			}
			if mode == "native_symlink" {
				if err = os.Rename(native, native+".actual"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(native+".actual", native); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			if mode == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			started := time.Now()
			got, err := desktopUpdateRuntimeSHA(ctx, native, "", "1.2.3", "linux", runtime.GOARCH, nativeSHA)
			if err == nil || got != "" || strings.Contains(err.Error(), "candidate-stderr-secret") {
				t.Fatalf("unsafe %s candidate accepted or stderr leaked: hash=%q error=%v", mode, got, err)
			}
			if mode == "timeout" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second) {
				t.Fatalf("candidate check did not obey deadline: elapsed=%v error=%v", time.Since(started), err)
			}
		})
	}
}

func TestDesktopUpdateLinuxCandidateCheckStripsCredentialsAndOverrides(t *testing.T) {
	got := desktopCandidateRuntimeCheckEnvironment([]string{"PATH=/bin", "HOME=/home/fixture", "XDG_CACHE_HOME=/cache", "TMPDIR=/tmp", "METIS_BIN=/old/metis", "METIS_DESKTOP_APP=/old/app", "METIS_DESKTOP_BUNDLED_RUNTIME=1", "OPENAI_API_KEY=secret", "AWS_SECRET_ACCESS_KEY=secret", "LD_PRELOAD=/unrelated/library"})
	want := []string{"PATH=/bin", "HOME=/home/fixture", "XDG_CACHE_HOME=/cache", "TMPDIR=/tmp"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("runtime check environment includes credentials/old identity: %v", got)
	}
}

func TestDesktopUpdateRuntimeIdentityKeepsDarwinAndExternalPaths(t *testing.T) {
	host, cli, manifest := writeRuntimeTestBundle(t, "1.2.3", "arm64")
	sha, err := desktopFileSHA256(host)
	if err != nil {
		t.Fatal(err)
	}
	got, err := desktopUpdateRuntimeSHA(context.Background(), macDesktopBundlePath(host), "", "1.2.3", "darwin", "arm64", sha)
	if err != nil || got != manifest["sha256"] {
		t.Fatalf("Darwin candidate changed verification: %s %v", got, err)
	}
	if err = os.WriteFile(cli, []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = desktopUpdateRuntimeSHA(context.Background(), macDesktopBundlePath(host), "", "1.2.3", "darwin", "arm64", sha); err == nil {
		t.Fatal("tampered Darwin CLI accepted")
	}
	want, err := desktopFileSHA256(cli)
	if err != nil {
		t.Fatal(err)
	}
	got, err = desktopUpdateRuntimeSHA(context.Background(), "/unused", cli, "1.2.3", "linux", "arm64", "")
	if err != nil || got != want {
		t.Fatalf("explicit external CLI identity changed: %s %v", got, err)
	}
	if _, err = desktopUpdateRuntimeSHA(context.Background(), "/unused", "", "1.2.3", "windows", "amd64", ""); err == nil {
		t.Fatal("unsupported platform accepted bundled supervisor")
	}
}
