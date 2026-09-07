package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/exitcode"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/verification"
)

func TestHeadlessRejectsVerificationBeforeSetup(t *testing.T) {
	t.Setenv("METIS_VERIFICATION_URL", "https://example.com")
	t.Setenv("METIS_VERIFICATION_TOKEN", strings.Repeat("a", 40))
	err := cmdRun(context.Background(), []string{"--bare", "--no-auth-wizard", "--metrics-log", filepath.Join(t.TempDir(), "missing", "metrics"), "test"})
	if err == nil || !strings.Contains(err.Error(), "METIS_VERIFICATION") {
		t.Fatalf("want verification configuration error before metrics/runtime, got %v", err)
	}
}

func TestRunVerificationConfiguration(t *testing.T) {
	t.Setenv("METIS_VERIFICATION_URL", "")
	t.Setenv("METIS_VERIFICATION_TOKEN", "")
	t.Setenv("METIS_VERIFICATION_REQUIRED_CHECKS", "")
	config, err := runVerificationFromEnv()
	if err != nil || config != nil {
		t.Fatalf("default must be disabled: %v %v", config, err)
	}
	t.Setenv("METIS_VERIFICATION_URL", "http://127.0.0.1:9999")
	if _, err = runVerificationFromEnv(); err == nil {
		t.Fatal("accepted missing token")
	}
	t.Setenv("METIS_VERIFICATION_TOKEN", strings.Repeat("b", 40))
	config, err = runVerificationFromEnv()
	if err != nil || strings.Join(config.required, ",") != "build,typecheck,smoke,controls,endurance" {
		t.Fatalf("default checks = %v %v", config, err)
	}
	for _, value := range []string{"smoke,smoke", "smoke,", "../command", " ", "smoke;id"} {
		t.Setenv("METIS_VERIFICATION_REQUIRED_CHECKS", value)
		if _, err = runVerificationFromEnv(); err == nil {
			t.Fatalf("accepted check IDs %q", value)
		}
	}
}

func TestWireRunVerificationHonorsFilteredToolsBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprintf(w, `{"source_sha256":%q}`, strings.Repeat("a", 64))
	}))
	defer server.Close()
	t.Setenv("METIS_VERIFICATION_URL", server.URL)
	t.Setenv("METIS_VERIFICATION_TOKEN", strings.Repeat("b", 40))
	t.Setenv("METIS_VERIFICATION_REQUIRED_CHECKS", "")
	config, err := runVerificationFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	tools.ApplyToolVisibility(reg, []string{"Read"}, nil)
	rt := &runtime{registry: reg, loop: &agent.Loop{Registry: reg}}
	err = wireRunVerification(context.Background(), rt, config, time.Now())
	var incomplete *exitcode.IncompleteError
	if !errors.As(err, &incomplete) || incomplete.Reason != "environment_blocked" || requests != 0 || rt.loop.MachineVerification != nil {
		t.Fatalf("must fail before request/policy: %v requests=%d", err, requests)
	}
}

func TestWireRunVerificationRegistersTrustedGate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"source_sha256":%q}`, strings.Repeat("a", 64))
	}))
	defer server.Close()
	t.Setenv("METIS_VERIFICATION_URL", server.URL)
	t.Setenv("METIS_VERIFICATION_TOKEN", strings.Repeat("b", 40))
	t.Setenv("METIS_VERIFICATION_REQUIRED_CHECKS", "smoke,controls")
	config, err := runVerificationFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	rt := &runtime{registry: reg, loop: &agent.Loop{Registry: reg}}
	if err = wireRunVerification(context.Background(), rt, config, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rt.loop.MachineVerification == nil || len(rt.loop.MachineVerification.RequiredChecks) != 2 {
		t.Fatal("missing host gate")
	}
	for _, name := range []string{"BrowserTest", "TaskClock"} {
		if _, ok := reg.GetForModel(name); !ok {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestHeadlessRejectsRecoveryBeforeSetup(t *testing.T) {
	t.Setenv("METIS_VERIFICATION_URL", "")
	t.Setenv("METIS_VERIFICATION_TOKEN", "")
	t.Setenv("METIS_VERIFICATION_REQUIRED_CHECKS", "")
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "-1")
	err := cmdRun(context.Background(), []string{"--bare", "--no-auth-wizard", "--metrics-log", filepath.Join(t.TempDir(), "missing", "metrics"), "test"})
	if err == nil || !strings.Contains(err.Error(), "METIS_RECOVERY_MAX_SECONDS") {
		t.Fatalf("invalid recovery must fail before setup: %v", err)
	}
}

func TestVerificationPreflightAcceptsNoPromptAndValidatesBeforeRuntime(t *testing.T) {
	t.Setenv("METIS_VERIFICATION_URL", "https://example.com")
	t.Setenv("METIS_VERIFICATION_TOKEN", strings.Repeat("a", 40))
	err := cmdRun(context.Background(), []string{"--bare", "--preflight-only", "--no-auth-wizard"})
	if err == nil || !strings.Contains(err.Error(), "METIS_VERIFICATION") {
		t.Fatalf("preflight should validate broker without requiring a prompt: %v", err)
	}
}

func TestVerificationDurationCannotClaimShortEndurance(t *testing.T) {
	for _, ms := range []float64{-1, 0, 899999, 900000} {
		check := verificationCheck(verification.Check{ID: "endurance", Status: "passed", DurationMS: &ms})
		if (check.Status == "pass") != (ms >= 900000) {
			t.Fatalf("duration %v became %s", ms, check.Status)
		}
	}
	if check := verificationCheck(verification.Check{ID: "endurance", Status: "passed"}); check.Status == "pass" || check.DurationMS != -1 {
		t.Fatal("null duration became evidence")
	}
	if check := verificationCheck(verification.Check{ID: "endurance", Status: "error"}); check.Status != "environment_blocked" {
		t.Fatal("lost environment error")
	}
}

func TestPreflightDisablesBackgroundMemoryEvenWhenEnabled(t *testing.T) {
	if shouldEnableAutoMemory(&cliFlags{preflightOnly: true, autoMemory: true}, func(string) (string, bool) { return "1", true }) {
		t.Fatal("preflight must not schedule model-based memory work")
	}
}
