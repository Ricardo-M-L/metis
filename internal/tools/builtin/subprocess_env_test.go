package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestRunCodeCanUseSilentlyDeniesCredentialReadAfterLongPrefix(t *testing.T) {
	runner := NewRunCode(bypassGate())
	code := strings.Repeat("# harmless padding\n", 20) + `print(open("/Users/x/.metis/auth.json").read())`
	got, reason := runner.CanUse(context.Background(), map[string]any{"code": code})
	if got != tools.PermissionDeny || reason != "secret_read:bypass_immune" {
		t.Fatalf("RunCode.CanUse = %v (%s), want silent credential-read deny", got, reason)
	}
}

func TestRunCodeDoesNotInheritProviderSecretsAndRedactsStderr(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	t.Setenv("OPENAI_API_KEY", secret)

	runner := NewRunCode(bypassGate())
	res, err := runner.Execute(context.Background(), map[string]any{
		"language": "python",
		"code": `import os, sys
print("provider_env=" + ("present" if "OPENAI_API_KEY" in os.environ else "absent"))
print("PATH=" + os.environ.get("PATH", "missing"))
print("child-error ghp_abcdefghijklmnopqrstuvwxyz1234567890", file=sys.stderr)`,
	})
	if err != nil {
		t.Fatalf("RunCode.Execute: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("RunCode result = %#v", res)
	}
	if strings.Contains(res.Output, secret) {
		t.Fatalf("RunCode leaked provider secret: %s", res.Output)
	}
	for _, want := range []string{"provider_env=absent", "PATH=", "[REDACTED]"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("RunCode output missing %q: %s", want, res.Output)
		}
	}
}

func TestRunCodeHonorsCallerCancellation(t *testing.T) {
	runner := NewRunCode(bypassGate())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	res, err := runner.Execute(ctx, map[string]any{
		"language": "bash",
		"code":     "sleep 10",
		"timeout":  float64(30),
	})
	if err != nil {
		t.Fatalf("RunCode.Execute: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "cancelled") {
		t.Fatalf("caller cancellation result = %#v", res)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("caller cancellation took %s", elapsed)
	}
}

func TestCappedRunCodeBufferBoundsMemory(t *testing.T) {
	var b cappedRunCodeBuffer
	payload := strings.Repeat("x", maxRunCodeOutputBytes+4096)
	if n, err := b.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	if b.Len() != maxRunCodeOutputBytes || !b.Truncated() {
		t.Fatalf("buffer len=%d truncated=%v", b.Len(), b.Truncated())
	}
	if !strings.Contains(b.String(), "[RunCode output truncated]") {
		t.Fatal("truncation marker missing")
	}
}

func TestStdioLSPDoesNotInheritProviderSecrets(t *testing.T) {
	secret := "lsp-provider-secret"
	t.Setenv("OPENAI_API_KEY", secret)
	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-env")
	server := filepath.Join(dir, "fake-lsp")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"${OPENAI_API_KEY-unset}\" > %q\nexit 0\n", captured)
	if err := os.WriteFile(server, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "main.fake")
	if err := os.WriteFile(source, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _ = runStdioLSPQuery(context.Background(), lspServer{cmd: server}, "hover", source, 1, 1)
	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("fake LSP did not run: %v", err)
	}
	if string(got) != "unset" {
		t.Fatalf("LSP inherited provider secret: %q", got)
	}
}

func TestWebBrowseChromiumDoesNotInheritProviderSecrets(t *testing.T) {
	secret := "browser-provider-secret"
	t.Setenv("OPENAI_API_KEY", secret)
	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-env")
	browser := filepath.Join(dir, "chromium")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"${OPENAI_API_KEY-unset}\" > %q\nprintf '<html><body>safe child</body></html>'\n", captured)
	if err := os.WriteFile(browser, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := (WebBrowse{}).Execute(context.Background(), map[string]any{"url": "https://example.test"})
	if err != nil {
		t.Fatalf("WebBrowse.Execute: %v", err)
	}
	if res == nil || res.IsError || !strings.Contains(res.Output, "safe child") {
		t.Fatalf("WebBrowse result = %#v", res)
	}
	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("fake chromium did not run: %v", err)
	}
	if string(got) != "unset" {
		t.Fatalf("WebBrowse inherited provider secret: %q", got)
	}
}

func TestWebBrowseChromiumUsesIsolatedProfileAndGuardedProxy(t *testing.T) {
	dir := t.TempDir()
	capturedHome := filepath.Join(dir, "captured-home")
	capturedMode := filepath.Join(dir, "captured-mode")
	capturedProfileMode := filepath.Join(dir, "captured-profile-mode")
	capturedProxy := filepath.Join(dir, "captured-proxy")
	capturedArgs := filepath.Join(dir, "captured-args")
	browser := filepath.Join(dir, "chromium")
	script := fmt.Sprintf(`#!/bin/sh
mode_of() {
  case "$(uname -s)" in
    Darwin) stat -f '%%Lp' "$1" ;;
    *) stat -c '%%a' "$1" ;;
  esac
}
printf '%%s' "$HOME" > %q
mode_of "$HOME" > %q
for arg in "$@"; do
  case "$arg" in
    --user-data-dir=*) profile="${arg#*=}" ;;
  esac
done
mode_of "$profile" > %q
printf '%%s|%%s' "${HTTP_PROXY-unset}" "${NO_PROXY-unset}" > %q
printf '%%s\n' "$@" > %q
printf '<html><body>isolated child</body></html>'
`, capturedHome, capturedMode, capturedProfileMode, capturedProxy, capturedArgs)
	if err := os.WriteFile(browser, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	hostHome := os.Getenv("HOME")
	t.Setenv("HTTP_PROXY", "http://ambient-proxy.example:8080")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := (WebBrowse{}).Execute(context.Background(), map[string]any{"url": "https://example.test"})
	if err != nil {
		t.Fatalf("WebBrowse.Execute: %v", err)
	}
	if res == nil || res.IsError || !strings.Contains(res.Output, "isolated child") {
		t.Fatalf("WebBrowse result = %#v", res)
	}
	homeBytes, err := os.ReadFile(capturedHome)
	if err != nil {
		t.Fatal(err)
	}
	isolatedHome := string(homeBytes)
	if isolatedHome == "" || isolatedHome == hostHome || !strings.Contains(filepath.Base(isolatedHome), "metis-webbrowse-") {
		t.Fatalf("chromium HOME was not isolated: host=%q child=%q", hostHome, isolatedHome)
	}
	if _, err := os.Stat(isolatedHome); !os.IsNotExist(err) {
		t.Fatalf("temporary browser HOME was not cleaned up: %q err=%v", isolatedHome, err)
	}
	modeBytes, err := os.ReadFile(capturedMode)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(modeBytes)); got != "700" {
		t.Fatalf("temporary browser HOME mode = %q, want 700", got)
	}
	profileModeBytes, err := os.ReadFile(capturedProfileMode)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(profileModeBytes)); got != "700" {
		t.Fatalf("temporary browser profile mode = %q, want 700", got)
	}
	proxyBytes, err := os.ReadFile(capturedProxy)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(proxyBytes); got != "unset|unset" {
		t.Fatalf("chromium inherited ambient proxy bypass configuration: %q", got)
	}
	argsBytes, err := os.ReadFile(capturedArgs)
	if err != nil {
		t.Fatal(err)
	}
	args := string(argsBytes)
	for _, want := range []string{
		"--user-data-dir=" + filepath.Join(isolatedHome, "profile"),
		"--proxy-server=http://127.0.0.1:",
		"--proxy-bypass-list=<-loopback>",
		"--disable-quic",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("chromium args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--no-sandbox") {
		t.Fatalf("chromium must not disable its OS sandbox:\n%s", args)
	}
}
