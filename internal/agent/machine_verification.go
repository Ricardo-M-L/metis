package agent

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/security"
)

// MachineVerificationPolicy is an opt-in host-owned completion policy. Callbacks
// must execute trusted checks against the current source, not read verdicts from
// model-writable files, and must honor the supplied context's cancellation and
// deadline. Install before Run and do not mutate while Run is active.
// Nil preserves ordinary interactive behavior. MaxAttempts bounds corrective
// re-entry (default 2, maximum 10); it never turns a failed check into a pass.
type MachineVerificationPolicy struct {
	RequiredChecks    []string
	CurrentSourceHash func(context.Context) (string, error)
	Verify            func(context.Context) (VerificationEvidence, error)
	MaxAttempts       int
}

// VerificationEvidence is returned only by the trusted host callback. SourceHash
// is the SHA-256 of the checked source snapshot. Every RequiredChecks entry must
// occur exactly once with Status=pass, a non-nil zero ExitCode and a valid
// ArtifactSHA256 identifying the trusted evidence artifact.
type VerificationEvidence struct {
	SourceHash string              `json:"source_hash"`
	Checks     []VerificationCheck `json:"checks"`
	Details    string              `json:"details,omitempty"`
}

type VerificationCheck struct {
	ID             string `json:"id"`
	Status         string `json:"status"` // pass, fail, incomplete, environment_blocked
	ExitCode       *int   `json:"exit_code"`
	DurationMS     int64  `json:"duration_ms"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	Details        string `json:"details,omitempty"`
}

func (p *MachineVerificationPolicy) maxAttempts() int {
	if p.MaxAttempts <= 0 {
		return 2
	}
	if p.MaxAttempts > 10 {
		return 10
	}
	return p.MaxAttempts
}

// check never accepts model prose (including an override) as proof. Hash before
// and after the callback binds evidence to one source epoch, including mutations
// made outside Write/Edit tools or by already-running background commands.
func (p *MachineVerificationPolicy) check(ctx context.Context, text string) (reason, detail string, err error) {
	defer func() { detail = security.RedactSubprocessText(detail) }()
	if err = ctx.Err(); err != nil {
		return "", "", context.Cause(ctx)
	}
	if strings.Contains(text, contractOverridePhrase) {
		return "acceptance_incomplete", "A model override is not machine-verification evidence.", nil
	}
	if machineVerificationIncompleteRequested(text) {
		return "acceptance_incomplete", "The model explicitly reported acceptance incomplete; no pass is claimed.", nil
	}
	if p.Verify == nil || p.CurrentSourceHash == nil || len(p.RequiredChecks) == 0 {
		return "environment_blocked", "Machine-verification policy is missing trusted callbacks or required check IDs.", nil
	}
	required := make(map[string]bool, len(p.RequiredChecks))
	for _, id := range p.RequiredChecks {
		if strings.TrimSpace(id) == "" || required[id] {
			return "environment_blocked", "Required check IDs must be non-empty and unique.", nil
		}
		required[id] = true
	}
	before, hashErr := p.CurrentSourceHash(ctx)
	if ctx.Err() != nil {
		return "", "", context.Cause(ctx)
	}
	if hashErr != nil {
		return "environment_blocked", fmt.Sprintf("Cannot hash source: %v", hashErr), nil
	}
	if !isSHA256(before) {
		return "environment_blocked", "Trusted source hash must be a SHA-256 hex digest.", nil
	}
	evidence, verifyErr := p.Verify(ctx)
	if ctx.Err() != nil {
		return "", "", context.Cause(ctx)
	}
	if verifyErr != nil {
		return "environment_blocked", fmt.Sprintf("Trusted verifier could not run: %v", verifyErr), nil
	}
	after, hashErr := p.CurrentSourceHash(ctx)
	if ctx.Err() != nil {
		return "", "", context.Cause(ctx)
	}
	if hashErr != nil {
		return "environment_blocked", fmt.Sprintf("Cannot re-hash source: %v", hashErr), nil
	}
	if evidence.SourceHash != before || after != before {
		return "acceptance_incomplete", "Verification evidence is missing, stale, or the source changed during verification; rerun checks on the current source.", nil
	}
	seen := make(map[string]bool, len(evidence.Checks))
	for _, check := range evidence.Checks {
		if !required[check.ID] {
			continue
		}
		if seen[check.ID] {
			return "acceptance_incomplete", "Duplicate machine check: " + check.ID, nil
		}
		seen[check.ID] = true
		if check.Status == "environment_blocked" {
			return "environment_blocked", fmt.Sprintf("Check %s is environment_blocked: %s", check.ID, check.Details), nil
		}
		if check.Status != "pass" || check.ExitCode == nil || *check.ExitCode != 0 || check.DurationMS < 0 || !isSHA256(check.ArtifactSHA256) {
			return "acceptance_incomplete", fmt.Sprintf("Check %s has no valid PASS/exit-zero evidence: %s. %s", check.ID, check.Status, check.Details), nil
		}
	}
	for _, id := range p.RequiredChecks {
		if !seen[id] {
			return "acceptance_incomplete", "Missing required machine check: " + id, nil
		}
	}
	return "", "", nil
}

func machineVerificationIncompleteRequested(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ACCEPTANCE_INCOMPLETE:") {
			return true
		}
	}
	return false
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (l *Loop) verifierDispatchAvailable() bool {
	if l.MachineVerification != nil || l.Registry == nil {
		return false
	}
	for _, entry := range l.Registry.ModelEntriesForCache() {
		if entry.Tool.Name() == "Agent" {
			return true
		}
	}
	return false
}
