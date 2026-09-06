package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

func TestNormalizeSearchKeyInput(t *testing.T) {
	got, err := normalizeSearchKeyInput([]byte("  opaque-search-key\n"))
	if err != nil || got != "opaque-search-key" {
		t.Fatalf("normalize key = %q, %v", got, err)
	}
	if _, err := normalizeSearchKeyInput([]byte("line-one\nline-two")); err == nil {
		t.Fatal("multiline key was accepted")
	}
	if _, err := normalizeSearchKeyInput(make([]byte, maxSearchKeyInputBytes+1)); err == nil {
		t.Fatal("oversize key was accepted")
	}
}

func TestAuthKeysPutAcquiresKeyWithoutArgvExposure(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const secret = "stdin-only-secret-value"
	originalAcquire := acquireSearchBackendKey
	originalStderr := authKeysStderr
	var output bytes.Buffer
	acquireSearchBackendKey = func(context.Context) (string, error) { return secret, nil }
	authKeysStderr = func() io.Writer { return &output }
	t.Cleanup(func() {
		acquireSearchBackendKey = originalAcquire
		authKeysStderr = originalStderr
	})

	if err := cmdAuthKeysPut(context.Background(), []string{"tavily"}); err != nil {
		t.Fatalf("keys put: %v", err)
	}
	got, err := auth.GetSearchKey("tavily")
	if err != nil || got != secret {
		t.Fatalf("stored key = %q, %v", got, err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("command output leaked key: %q", output.String())
	}
	if strings.Contains(output.String(), "warning:") {
		t.Fatalf("secure no-argv form emitted deprecation warning: %q", output.String())
	}
}

func TestAuthKeysPutLegacyArgvFormWarnsWithoutEchoingSecret(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const secret = "legacy-argv-secret-value"
	originalStderr := authKeysStderr
	var output bytes.Buffer
	authKeysStderr = func() io.Writer { return &output }
	t.Cleanup(func() { authKeysStderr = originalStderr })

	if err := cmdAuthKeysPut(context.Background(), []string{"brave", secret}); err != nil {
		t.Fatalf("legacy keys put: %v", err)
	}
	if !strings.Contains(output.String(), "warning:") {
		t.Fatalf("legacy form did not warn: %q", output.String())
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("legacy form echoed key: %q", output.String())
	}
}

func TestAuthKeysListNeverPrintsCredentialBytes(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const secret = "UNIQUE-LIST-SECRET-0123456789"
	if err := auth.SetSearchKey("tavily", secret); err != nil {
		t.Fatal(err)
	}

	file, err := os.CreateTemp(t.TempDir(), "keys-list-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = file
	if err := cmdAuthKeysList(); err != nil {
		os.Stdout = original
		t.Fatal(err)
	}
	os.Stdout = original
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output, []byte(secret)) || bytes.Contains(output, []byte(secret[:6])) {
		t.Fatalf("keys list leaked credential bytes: %q", output)
	}
	if !bytes.Contains(output, []byte(fmt.Sprintf("tavily (configured, %d chars)", len(secret)))) {
		t.Fatalf("keys list lost safe metadata: %q", output)
	}
}
