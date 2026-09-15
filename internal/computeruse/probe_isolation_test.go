package computeruse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

func probeShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func TestProbeExplicitManagerHomeProtectsInstallAndStatus(t *testing.T) {
	home := t.TempDir()
	control := t.TempDir()
	decoy := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", decoy)
	var checks strings.Builder
	for _, name := range []string{"config.toml", "auth.json"} {
		path := filepath.Join(control, name)
		if err := os.WriteFile(path, []byte("temporary manager-specific fake credential\n"), 0600); err != nil {
			t.Fatal(err)
		}
		checks.WriteString("if /bin/cat " + probeShellQuote(path) + " >/dev/null 2>&1; then exit 51; fi\n")
	}
	m := New(control)
	status, err := m.InstallLocal(context.Background(), isolationProbeScript(t, checks.String()))
	if err != nil || !status.Installed {
		t.Fatalf("install did not preserve explicit credential root: %+v %v", status, err)
	}
	status, err = m.Status(context.Background())
	if err != nil || !status.Installed {
		t.Fatalf("status did not preserve explicit credential root: %+v %v", status, err)
	}
}

func TestProbeNetworkIsBlocked(t *testing.T) {
	if _, err := os.Stat("/usr/bin/curl"); err != nil {
		t.Skip("system curl is unavailable for loopback-only fixture")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("METIS_HOME", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	filename := isolationProbeScript(t, "if /usr/bin/curl --noproxy '*' --silent --fail --max-time 1 "+probeShellQuote(server.URL)+" >/dev/null 2>&1; then exit 61; fi\n")
	if _, err := Probe(context.Background(), filename); err != nil {
		t.Fatalf("probe loopback network boundary failed: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("isolated probe reached loopback fixture %d times", got)
	}
}

func TestProbePrivateCommandEnvironmentAndCleanup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("METIS_HOME", t.TempDir())
	filename := isolationProbeScript(t, "")
	command, cleanup, err := newDescriptionProbeCommand(context.Background(), filename, os.Getenv("METIS_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if command.Path == filename || command.Dir == "" || command.Env == nil {
		t.Fatal("probe was not wrapped or privately initialized")
	}
	env := map[string]string{}
	for _, entry := range command.Env {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	if env["HOME"] != command.Dir || env["PWD"] != command.Dir || env["TMPDIR"] != command.Dir {
		t.Fatal("probe temporary paths do not share one private directory")
	}
	if _, ok := env["METIS_HOME"]; ok {
		t.Fatal("control-root variable was exposed to the child")
	}
	if _, ok := env["METIS_INTERNAL_SANDBOX_PROFILE"]; ok {
		t.Fatal("internal sandbox marker leaked to the child")
	}
	privateDir := command.Dir
	cleanup()
	if _, err := os.Stat(privateDir); !os.IsNotExist(err) {
		t.Fatalf("probe private directory was not removed: %v", err)
	}
}

func isolationProbeScript(t *testing.T, checks string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture")
	}
	filename := filepath.Join(t.TempDir(), "isolated-helper")
	body := "#!/bin/sh\n" + checks + "\n" + strings.TrimPrefix(string(descriptionScript(t, fixtureDescription("isolation-fixture"))), "#!/bin/sh\n")
	if err := os.WriteFile(filename, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestProbeMinimalEnvironment(t *testing.T) {
	home := t.TempDir()
	control := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", control)
	for _, key := range []string{"OPENAI_API_KEY", "CUSTOM_OPAQUE_VALUE", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "BASH_ENV", "PYTHONPATH"} {
		t.Setenv(key, "fake-probe-canary")
	}
	filename := isolationProbeScript(t, `
for key in OPENAI_API_KEY CUSTOM_OPAQUE_VALUE LD_PRELOAD DYLD_INSERT_LIBRARIES DISPLAY XAUTHORITY DBUS_SESSION_BUS_ADDRESS BASH_ENV PYTHONPATH METIS_HOME; do
    /usr/bin/printenv "$key" >/dev/null 2>&1 && exit 31
done
[ "$HOME" = "$PWD" ] && [ "$HOME" = "$TMPDIR" ] || exit 32
[ "$PATH" = /usr/bin:/bin:/usr/sbin:/sbin ] || exit 33
[ "$AGENT" = metis ] && [ "$AI_AGENT" = metis ] && [ "$METIS" = 1 ] || exit 34
[ "$HOME" != `+probeShellQuote(home)+` ] || exit 35
`)
	if _, err := Probe(context.Background(), filename); err != nil {
		t.Fatalf("minimal probe environment not enforced: %v", err)
	}
}

func TestProbeDeniesTemporaryCredentialCanaries(t *testing.T) {
	home := t.TempDir()
	control := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", control)
	var checks strings.Builder
	for _, path := range []string{filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(control, "config.toml"), filepath.Join(control, "auth.json"), filepath.Join(control, auth.CredentialDirectoryName, "fixture.json")} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("temporary fake credential canary\n"), 0600); err != nil {
			t.Fatal(err)
		}
		checks.WriteString("if /bin/cat " + probeShellQuote(path) + " >/dev/null 2>&1; then exit 41; fi\n")
	}
	outside := filepath.Join(t.TempDir(), "outside-write-canary")
	checks.WriteString("if ( : > " + probeShellQuote(outside) + " ) 2>/dev/null; then exit 42; fi\n")
	filename := isolationProbeScript(t, checks.String())
	if _, err := Probe(context.Background(), filename); err != nil {
		t.Fatalf("probe credential/write boundary not enforced: %v", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("probe wrote outside its private temp directory: %v", err)
	}
}
