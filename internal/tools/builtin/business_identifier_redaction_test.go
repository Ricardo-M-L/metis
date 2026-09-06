package builtin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestBusinessAccountIDSurvivesToolOutputRedaction(t *testing.T) {
	const record = `{"event":"account_created","account_id":"fixture-account-123","access_token":"synthetic-secret-must-not-leak"}`
	dir := t.TempDir()
	t.Setenv("METIS_HOME", t.TempDir())
	path := filepath.Join(dir, "business-record.json")
	if err := os.WriteFile(path, []byte(record+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertOutput := func(t *testing.T, result *tools.Result, err error) {
		t.Helper()
		if err != nil || result == nil || result.IsError {
			t.Fatalf("tool result=%+v error=%v", result, err)
		}
		if !strings.Contains(result.Output, "fixture-account-123") || !strings.Contains(result.Output, "[REDACTED]") || strings.Contains(result.Output, "synthetic-secret-must-not-leak") {
			t.Fatalf("business ID lost or credential exposed: %s", result.Output)
		}
	}
	t.Run("Read", func(t *testing.T) {
		result, err := (Read{gate: permission.New(permission.ModeDefault)}).Execute(context.Background(), map[string]any{"path": path})
		assertOutput(t, result, err)
	})
	t.Run("Grep", func(t *testing.T) {
		result, err := (Grep{gate: permission.New(permission.ModeDefault)}).Execute(context.Background(), map[string]any{"root": dir, "pattern": "account_created"})
		assertOutput(t, result, err)
	})
	t.Run("RunCode", func(t *testing.T) {
		if _, err := exec.LookPath("bash"); err != nil {
			t.Skip("bash is not available")
		}
		manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: string(sandbox.ModeOff), TempRoot: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = manager.Close() })
		result, err := NewRunCodeWithSandbox(nil, manager).Execute(context.Background(), map[string]any{"language": "bash", "code": "printf '%s\\n' '" + record + "'"})
		assertOutput(t, result, err)
	})
}
