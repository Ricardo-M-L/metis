package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/exitcode"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	"github.com/Ricardo-M-L/metis/internal/verification"
)

type runVerification struct {
	client       *verification.Client
	required     []string
	sourceSHA256 string // trusted startup health result; not a completion verdict
}

// These settings are host startup policy, not model-editable project config.
// METIS_* credentials are stripped from sandboxed Bash child environments.
func runVerificationFromEnv() (*runVerification, error) {
	endpoint, token, required := os.Getenv("METIS_VERIFICATION_URL"), os.Getenv("METIS_VERIFICATION_TOKEN"), os.Getenv("METIS_VERIFICATION_REQUIRED_CHECKS")
	if endpoint == "" && token == "" && required == "" {
		return nil, nil
	}
	if endpoint == "" || token == "" {
		return nil, fmt.Errorf("METIS_VERIFICATION_URL and METIS_VERIFICATION_TOKEN must both be configured")
	}
	client, err := verification.NewClient(endpoint, token)
	if err != nil {
		return nil, fmt.Errorf("METIS_VERIFICATION configuration: %w", err)
	}
	if required == "" {
		required = "build,typecheck,smoke,controls,endurance"
	}
	checks := strings.Split(required, ",")
	seen := map[string]bool{}
	valid := regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	for i, id := range checks {
		id = strings.TrimSpace(id)
		if !valid.MatchString(id) || seen[id] {
			return nil, fmt.Errorf("METIS_VERIFICATION_REQUIRED_CHECKS must contain unique, nonempty check IDs")
		}
		checks[i] = id
		seen[id] = true
	}
	return &runVerification{client: client, required: checks}, nil
}

func wireRunVerification(ctx context.Context, rt *runtime, config *runVerification, started time.Time) error {
	if config == nil {
		return nil
	}
	blocked := func(detail string) error {
		return &exitcode.IncompleteError{Reason: "environment_blocked", Detail: detail}
	}
	if rt == nil || rt.registry == nil || rt.loop == nil {
		return blocked("Trusted verification requires a live tool registry and loop.")
	}
	rt.registry.Register(builtin.NewBrowserTest(func(ctx context.Context, suite string) (string, bool, error) {
		job, err := config.client.Run(ctx, suite)
		if err != nil {
			return "", false, err
		}
		data, err := json.Marshal(job)
		passed := job.Status == "completed" && !job.SourceChanged && len(job.Result.Checks) > 0
		for _, check := range job.Result.Checks {
			converted := verificationCheck(check)
			passed = passed && converted.Status == "pass" && converted.ExitCode != nil && *converted.ExitCode == 0 && converted.DurationMS >= 0 && converted.ArtifactSHA256 != ""
		}
		return string(data), passed, err
	}, rt.gate))
	runDeadline, _ := ctx.Deadline()
	rt.registry.Register(builtin.NewTaskClock(started, runDeadline))
	if _, ok := rt.registry.GetForModel("BrowserTest"); !ok {
		return blocked("BrowserTest is excluded by the effective tool policy; explicitly allow it before running machine verification.")
	}
	if browser, ok := rt.registry.GetForModel("BrowserTest"); ok {
		for _, suite := range []string{"smoke", "controls", "endurance"} {
			if allowed, _ := browser.CanUse(ctx, map[string]any{"suite": suite}); allowed != tools.PermissionAllow {
				return blocked("BrowserTest is not pre-authorized by the effective permission policy; allow its fixed suites before headless verification.")
			}
		}
	}
	// Check availability before the first model request. Failure is explicit,
	// never a hidden downgrade to a self-authored results.json or HTTP fallback.
	sourceHash, err := config.client.SourceHash(ctx)
	if err != nil {
		return blocked("Trusted browser broker preflight failed: " + err.Error())
	}
	config.sourceSHA256 = sourceHash
	rt.loop.MachineVerification = &agent.MachineVerificationPolicy{
		RequiredChecks:    append([]string(nil), config.required...),
		CurrentSourceHash: config.client.SourceHash,
		Verify: func(ctx context.Context) (agent.VerificationEvidence, error) {
			evidence, err := config.client.Acceptance(ctx)
			if err != nil {
				return agent.VerificationEvidence{}, err
			}
			result := agent.VerificationEvidence{SourceHash: evidence.SourceSHA256}
			for _, check := range evidence.Checks {
				result.Checks = append(result.Checks, verificationCheck(check))
			}
			return result, nil
		},
	}
	rt.loop.System += "\n\nHost machine-verification policy is enabled. Required current-source evidence: " + strings.Join(config.required, ", ") + ". Use BrowserTest for the fixed suites and inspect actual failures. Source changes invalidate earlier evidence. An override, your own PASS report, compilation alone, or a still-running test cannot satisfy this policy. If blocked, report ACCEPTANCE_INCOMPLETE: with the reason. Environment dates are startup snapshots, not live clocks."
	if _, ok := rt.registry.GetForModel("TaskClock"); ok {
		rt.loop.System += " Use TaskClock for current time and this invocation's elapsed time; external continuations do not count as uninterrupted execution."
	}
	return nil
}

func verificationCheck(check verification.Check) agent.VerificationCheck {
	status := "incomplete"
	switch check.Status {
	case "passed":
		status = "pass"
	case "failed":
		status = "fail"
	case "error":
		status = "environment_blocked"
	}
	duration := int64(-1)
	if check.DurationMS != nil && !math.IsNaN(*check.DurationMS) && !math.IsInf(*check.DurationMS, 0) && *check.DurationMS >= 0 && *check.DurationMS < float64(math.MaxInt64) {
		duration = int64(*check.DurationMS)
	}
	if status == "pass" && check.ID == "endurance" && duration < 900000 {
		status = "incomplete"
	}
	return agent.VerificationCheck{ID: check.ID, Status: status, ExitCode: check.ExitCode, DurationMS: duration, ArtifactSHA256: check.ArtifactSHA256, Details: string(check.Details)}
}

func runPreflightReport(rt *runtime, config *runVerification) map[string]any {
	names := []string{}
	if rt != nil && rt.registry != nil {
		for _, entry := range rt.registry.ModelEntriesForCache() {
			names = append(names, entry.Tool.Name())
		}
	}
	sort.Strings(names)
	required := []string{}
	var sourceHash any
	if config != nil {
		required = append(required, config.required...)
		sourceHash = config.sourceSHA256
	}
	return map[string]any{"preflight_only": true, "model_requests": 0, "verification_enabled": rt != nil && rt.loop != nil && rt.loop.MachineVerification != nil, "tools": names, "required_checks": required, "source_sha256": sourceHash}
}
