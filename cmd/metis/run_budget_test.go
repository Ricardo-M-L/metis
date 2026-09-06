package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPRunCapDefaultsToUnlimited(t *testing.T) {
	for _, value := range []string{"", "0"} {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv("METIS_RUN_MAX_SECONDS", value)
			if got, err := mcpRunCap(); got != 0 || err != nil {
				t.Fatalf("run cap = %v, %v; want unlimited (0)", got, err)
			}
		})
	}
}

func TestHeadlessRejectsMetricsPathBeforeSetup(t *testing.T) {
	t.Setenv("METIS_RUN_MAX_SECONDS", "0")
	badPath := filepath.Join(t.TempDir(), "missing", "metrics.jsonl")
	err := cmdRun(context.Background(), []string{"--bare", "--no-auth-wizard", "--metrics-log", badPath, "test"})
	if err == nil || !strings.Contains(err.Error(), "--metrics-log:") {
		t.Fatalf("error = %v, want invalid metrics path before runtime setup", err)
	}
}

func TestMCPRunCapExplicitSixHours(t *testing.T) {
	t.Setenv("METIS_RUN_MAX_SECONDS", "21600")
	if got, err := mcpRunCap(); got != 6*time.Hour || err != nil {
		t.Fatalf("cap = %v, %v; want six hours", got, err)
	}
}

func TestHeadlessRejectsInvalidRunBudgetBeforeSetup(t *testing.T) {
	for _, value := range []string{"-1", "invalid", "9223372037"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("METIS_RUN_MAX_SECONDS", value)
			err := cmdRun(context.Background(), []string{"--bare", "--no-auth-wizard", "test"})
			if err == nil || !strings.Contains(err.Error(), "invalid METIS_RUN_MAX_SECONDS") {
				t.Fatalf("cmdRun error = %v, want invalid budget before runtime setup", err)
			}
			if err := cmdMCPServe(context.Background(), []string{"--bare"}); err == nil || !strings.Contains(err.Error(), "invalid METIS_RUN_MAX_SECONDS") {
				t.Fatalf("cmdMCPServe error = %v, want invalid budget before scanner", err)
			}
		})
	}
}
