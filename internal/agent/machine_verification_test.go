package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type contractAvailableAgentTool struct {
	contractBashTool
	exposure tools.ToolExposure
	disabled bool
}

func (contractAvailableAgentTool) Name() string                       { return "Agent" }
func (t contractAvailableAgentTool) ToolExposure() tools.ToolExposure { return t.exposure }
func (t contractAvailableAgentTool) IsEnabled() bool                  { return !t.disabled }

func TestContractVerifierCapabilityUsesFilteredRegistry(t *testing.T) {
	for _, tc := range []struct {
		name           string
		exposure       tools.ToolExposure
		disabled, want bool
	}{
		{"visible", tools.ToolExposureDirect, false, true},
		{"hidden", tools.ToolExposureHidden, false, false},
		{"disabled", tools.ToolExposureDirect, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tools.NewRegistry()
			r.Register(contractAvailableAgentTool{exposure: tc.exposure, disabled: tc.disabled})
			l := &Loop{Registry: r}
			if got := l.verifierDispatchAvailable(); got != tc.want {
				t.Fatalf("available=%v, want %v", got, tc.want)
			}
			l.MachineVerification = &MachineVerificationPolicy{}
			if l.verifierDispatchAvailable() {
				t.Fatal("machine policy must replace prompt-only dispatch")
			}
		})
	}
}

func TestContractDoesNotDemandUnavailableAgent(t *testing.T) {
	t.Setenv(contractDisableEnvVar, "0")
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		toolUseStream("bash-risk", "Bash", `{"command":"git push"}`),
		textStream("implementation complete"),
	}}
	registry := tools.NewRegistry()
	registry.Register(contractBashTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 4)
	loop.AppendUser("finish")
	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	if got := len(provider.capturedRequests()); got != 2 {
		t.Fatalf("provider calls = %d, want 2; cannot dispatch unavailable Agent", got)
	}
	for _, message := range loop.History() {
		if message.Role != llm.RoleUser {
			continue
		}
		for _, block := range message.Content {
			if strings.Contains(block.Text, "Agent({subagent_type:") {
				t.Fatal("unavailable verifier was injected into model history")
			}
		}
	}
}

func TestMachineVerificationCannotClaimPassWithoutTrustedEvidence(t *testing.T) {
	// The legacy prompt-only contract escape hatch cannot disable an explicit
	// host machine-verification policy.
	t.Setenv(contractDisableEnvVar, "1")
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	zero, one := 0, 1
	for _, tc := range []struct {
		name, status, hash, text, want string
		code                           *int
		missing                        bool
	}{
		{name: "pass", status: "pass", hash: digest, code: &zero, want: "end_turn"},
		{name: "failed", status: "fail", hash: digest, code: &one, want: "acceptance_incomplete"},
		{name: "pass_nonzero", status: "pass", hash: digest, code: &one, want: "acceptance_incomplete"},
		{name: "missing_exit_code", status: "pass", hash: digest, want: "acceptance_incomplete"},
		{name: "missing_check", hash: digest, missing: true, want: "acceptance_incomplete"},
		{name: "wrong_hash", status: "pass", hash: strings.Repeat("b", 64), code: &zero, want: "acceptance_incomplete"},
		{name: "empty_hash", status: "pass", code: &zero, want: "acceptance_incomplete"},
		{name: "blocked", status: "environment_blocked", hash: digest, want: "environment_blocked"},
		{name: "override", status: "pass", hash: digest, code: &zero, text: "OVERRIDE CONTRACT: skip verification", want: "acceptance_incomplete"},
		{name: "honest_incomplete", status: "pass", hash: digest, code: &zero, text: "ACCEPTANCE_INCOMPLETE: test fixture unavailable", want: "acceptance_incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text
			if text == "" {
				text = "all checks passed; results.json says PASS"
			}
			provider := &queuedStreamProvider{streams: []llm.StreamReader{textStream(text), textStream(text), textStream(text)}}
			loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "system", 5)
			loop.MachineVerification = &MachineVerificationPolicy{
				RequiredChecks: []string{"collision"}, MaxAttempts: 1,
				CurrentSourceHash: func(context.Context) (string, error) { return digest, nil },
				Verify: func(context.Context) (VerificationEvidence, error) {
					e := VerificationEvidence{SourceHash: tc.hash}
					if !tc.missing {
						e.Checks = []VerificationCheck{{ID: "collision", Status: tc.status, ExitCode: tc.code, ArtifactSHA256: digest}}
					}
					return e, nil
				},
			}
			loop.AppendUser("finish after machine verification")
			out := make(chan Event, 64)
			err := loop.Run(context.Background(), out)
			close(out)
			if tc.want == "end_turn" && err != nil {
				t.Fatal(err)
			}
			got := ""
			for event := range out {
				if event.Kind == EventLoopDone {
					got = event.StopReason
				}
			}
			if got != tc.want {
				t.Fatalf("stop = %q, want %q; err=%v", got, tc.want, err)
			}
			if tc.want != "end_turn" && !IsIncompleteStopReason(got) {
				t.Fatal("unsuccessful verification not classified incomplete")
			}
			if len(provider.capturedRequests()) > 2 {
				t.Fatal("verification attempts were not bounded")
			}
		})
	}
}

func TestMachineVerificationSourceChangesAndEvidenceValidation(t *testing.T) {
	zero := 0
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name         string
		alter        func(*VerificationEvidence)
		changeSource bool
	}{
		{name: "source_mutated", changeSource: true},
		{name: "duplicate", alter: func(e *VerificationEvidence) { e.Checks = append(e.Checks, e.Checks[0]) }},
		{name: "invalid_artifact", alter: func(e *VerificationEvidence) { e.Checks[0].ArtifactSHA256 = "not-a-digest" }},
		{name: "missing_artifact", alter: func(e *VerificationEvidence) { e.Checks[0].ArtifactSHA256 = "" }},
		{name: "negative_duration", alter: func(e *VerificationEvidence) { e.Checks[0].DurationMS = -1 }},
		{name: "unknown_does_not_cover_required", alter: func(e *VerificationEvidence) { e.Checks[0].ID = "model_self_report" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := hash
			p := &MachineVerificationPolicy{RequiredChecks: []string{"build"}, CurrentSourceHash: func(context.Context) (string, error) { return current, nil }, Verify: func(context.Context) (VerificationEvidence, error) {
				e := VerificationEvidence{SourceHash: hash, Checks: []VerificationCheck{{ID: "build", Status: "pass", ExitCode: &zero, ArtifactSHA256: hash}}}
				if tc.alter != nil {
					tc.alter(&e)
				}
				if tc.changeSource {
					current = strings.Repeat("b", 64)
				}
				return e, nil
			}}
			if reason, _, err := p.check(context.Background(), "done"); reason != "acceptance_incomplete" || err != nil {
				t.Fatalf("reason=%q err=%v", reason, err)
			}
		})
	}
}

func TestMachineVerificationCancellationIsNotPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &MachineVerificationPolicy{RequiredChecks: []string{"build"}, CurrentSourceHash: func(context.Context) (string, error) { cancel(); return "", context.Canceled }, Verify: func(context.Context) (VerificationEvidence, error) {
		t.Fatal("verifier ran after cancellation")
		return VerificationEvidence{}, nil
	}}
	if _, _, err := p.check(ctx, "done"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want canceled", err)
	}
}
