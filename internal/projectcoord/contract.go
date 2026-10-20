package projectcoord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/security"
)

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type AttemptUsage struct {
	Token           string  `json:"token"`
	Worker          string  `json:"worker"`
	SpentUSD        float64 `json:"spent_usd"`
	LegacySpentUSD  float64 `json:"legacy_spent_usd,omitempty"`
	RequestSpentUSD float64 `json:"request_spent_usd,omitempty"`
}

type Acceptance struct {
	Status    string        `json:"status"` // recorded, passed, failed
	Summary   string        `json:"summary,omitempty"`
	CheckedAt time.Time     `json:"checked_at"`
	Checks    []CheckResult `json:"checks,omitempty"`
}

type CheckResult struct {
	Command      string `json:"command"`
	ExitCode     int    `json:"exit_code"`
	DurationMS   int64  `json:"duration_ms"`
	Output       string `json:"output,omitempty"`
	OutputSHA256 string `json:"output_sha256"`
}

func validateContract(maxUSD float64, commands, deliverables []string) error {
	if math.IsNaN(maxUSD) || math.IsInf(maxUSD, 0) || maxUSD < 0 {
		return errors.New("maximum budget must be a finite, non-negative USD amount")
	}
	if len(commands) > 16 || len(deliverables) > 64 {
		return errors.New("contract permits at most 16 checks and 64 deliverables")
	}
	for _, command := range commands {
		if strings.TrimSpace(command) == "" || len(command) > 8192 || strings.ContainsRune(command, 0) {
			return errors.New("acceptance check must be a non-empty command of at most 8192 bytes")
		}
	}
	seen := map[string]bool{}
	for _, path := range deliverables {
		clean := filepath.Clean(path)
		if path == "" || filepath.IsAbs(path) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || seen[clean] {
			return errors.New("deliverables must be unique file paths inside the workspace")
		}
		seen[clean] = true
	}
	return nil
}

func sameContract(before Run, old WorkItem, after Run, current WorkItem) bool {
	snapshot := func(run Run, item WorkItem) []byte {
		deps := make([]any, 0, len(item.DependsOn))
		for _, id := range item.DependsOn {
			dep, err := run.item(id)
			if err != nil {
				return nil
			}
			deps = append(deps, []any{dep.ID, dep.Phase, dep.Status, dep.Deliverables, dep.Artifacts, dep.Handoff, dep.PrincipalBinding})
		}
		data, _ := json.Marshal([]any{run.CheckCommands, item.Phase, item.Deliverables, item.Handoff, item.PrincipalBinding, item.DependsOn, item.Prompt, item.AgentProfile, deps})
		return data
	}
	return bytes.Equal(snapshot(before, old), snapshot(after, current))
}

func verifyCompletion(ctx context.Context, run Run, item WorkItem) (Acceptance, []Artifact, error) {
	result := Acceptance{Status: "recorded", CheckedAt: time.Now().UTC()}
	reject := func(err error) (Acceptance, []Artifact, error) {
		result.Status, result.Summary = "failed", security.RedactSubprocessText(err.Error())
		return result, nil, fmt.Errorf("project acceptance failed: %s", result.Summary)
	}
	if err := ctx.Err(); err != nil {
		return reject(err)
	}
	if err := validateHandoffInputs(ctx, run.Workspace, item.Handoff); err != nil {
		return reject(err)
	}
	paths := append([]string(nil), item.Deliverables...)
	var inherited []Artifact
	for _, depID := range item.DependsOn {
		dep, err := run.item(depID)
		if err != nil {
			return reject(err)
		}
		if err := validateHandoffInputs(ctx, run.Workspace, dep.Handoff); err != nil {
			return reject(err)
		}
		inherited = append(inherited, dep.Artifacts...)
		for _, artifact := range dep.Artifacts {
			paths = append(paths, artifact.Path)
		}
	}
	before, err := captureArtifacts(ctx, run.Workspace, paths)
	if err != nil {
		return reject(err)
	}
	for _, expected := range inherited {
		ownedOutput := false
		for _, path := range item.Deliverables {
			ownedOutput = ownedOutput || filepath.Clean(path) == expected.Path
		}
		if ownedOutput && item.Phase != PhaseVerification {
			continue
		}
		for _, actual := range before {
			if actual.Path == expected.Path && actual.SHA256 != expected.SHA256 {
				return reject(fmt.Errorf("prerequisite deliverable changed: %s", expected.Path))
			}
		}
	}
	if item.Phase == PhaseVerification {
		if len(run.CheckCommands) == 0 {
			return reject(errors.New("verification needs host acceptance checks; configure coordinator create --check COMMAND"))
		}
		for _, command := range run.CheckCommands {
			check, err := runAcceptanceCheck(ctx, run.Workspace, command)
			result.Checks = append(result.Checks, check)
			if err != nil {
				return reject(err)
			}
		}
		result.Status = "passed"
	}
	if err := validateHandoffInputs(ctx, run.Workspace, item.Handoff); err != nil {
		return reject(err)
	}
	for _, depID := range item.DependsOn {
		dep, _ := run.item(depID)
		if err := validateHandoffInputs(ctx, run.Workspace, dep.Handoff); err != nil {
			return reject(err)
		}
	}
	after, err := captureArtifacts(ctx, run.Workspace, paths)
	if err != nil {
		return reject(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		return reject(errors.New("deliverables changed during acceptance; rerun checks on the current files"))
	}
	return result, after, nil
}

func captureArtifacts(ctx context.Context, workspace string, paths []string) ([]Artifact, error) {
	result := make([]Artifact, 0, len(paths))
	seen := map[string]bool{}
	for _, relative := range paths {
		if err := validateContract(0, nil, []string{relative}); err != nil {
			return nil, err
		}
		relative = filepath.Clean(relative)
		if seen[relative] {
			continue
		}
		seen[relative] = true
		resolved, err := filepath.EvalSymlinks(filepath.Join(workspace, relative))
		if err != nil {
			return nil, fmt.Errorf("deliverable %s: %w", relative, err)
		}
		rel, err := filepath.Rel(workspace, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("deliverable leaves workspace: %s", relative)
		}
		file, err := os.Open(resolved)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			return nil, fmt.Errorf("deliverable must be a regular file: %s", relative)
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, &contextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		result = append(result, Artifact{Path: relative, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type boundedOutput struct{ bytes.Buffer }

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := (64 << 10) - w.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = w.Buffer.Write(p)
	}
	return n, nil
}

func runAcceptanceCheck(ctx context.Context, workspace, command string) (CheckResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	name, args := "sh", []string{"-c", command}
	if runtime.GOOS == "windows" {
		name, args = "cmd", []string{"/C", command}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	jobs.ApplyProcessGroup(cmd)
	cmd.Cancel = func() error { jobs.KillProcessGroup(cmd.Process); return nil }
	cmd.Dir = workspace
	cmd.Env = security.RestrictedSubprocessEnv(os.Environ())
	cmd.WaitDelay = time.Second
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	started := time.Now()
	err := cmd.Run()
	hash := sha256.Sum256(output.Bytes())
	result := CheckResult{Command: security.RedactSubprocessText(command), ExitCode: -1, DurationMS: time.Since(started).Milliseconds(), Output: security.RedactSubprocessText(output.String()), OutputSHA256: hex.EncodeToString(hash[:])}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return result, fmt.Errorf("check %q did not pass (exit=%d): %w", result.Command, result.ExitCode, err)
	}
	return result, nil
}

func (s *Store) SetChecks(workspace, runID string, commands []string) (Run, error) {
	if err := validateContract(0, commands, nil); err != nil {
		return Run{}, err
	}
	if len(commands) == 0 {
		return Run{}, errors.New("at least one host acceptance check is required")
	}
	return s.withRun(workspace, runID, func(run *Run) (bool, error) {
		if run.Status == RunCompleted || run.Status == RunCancelled {
			return false, errors.New("cannot change acceptance checks on a finished project")
		}
		run.CheckCommands = append([]string(nil), commands...)
		run.UpdatedAt = s.now()
		run.addEvent(s.now(), "acceptance_configured", "", "Updated host acceptance commands", "")
		return true, nil
	})
}

// RecordSpend persists cumulative attempt usage, including late usage after cancellation.
func (s *Store) RecordSpend(workspace, runID, itemID, worker, token string, totalUSD float64) error {
	if math.IsNaN(totalUSD) || math.IsInf(totalUSD, 0) || totalUSD < 0 {
		return errors.New("spend must be a finite, non-negative USD amount")
	}
	_, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		item, err := run.item(itemID)
		if err != nil {
			return false, err
		}
		for i := range item.Usage {
			usage := &item.Usage[i]
			if usage.Token != token || usage.Worker != worker {
				continue
			}
			if usage.LegacySpentUSD == 0 && usage.RequestSpentUSD == 0 && usage.SpentUSD > 0 {
				usage.LegacySpentUSD = usage.SpentUSD
			}
			if totalUSD < usage.LegacySpentUSD {
				return false, errors.New("cumulative attempt spend cannot decrease")
			}
			legacyChanged := totalUSD != usage.LegacySpentUSD
			usage.LegacySpentUSD = totalUSD
			effective := math.Max(totalUSD, usage.RequestSpentUSD)
			delta := effective - usage.SpentUSD
			usage.SpentUSD = effective
			item.SpentUSD += delta
			run.SpentUSD += delta
			item.UpdatedAt, run.UpdatedAt = s.now(), s.now()
			return legacyChanged || delta > 0, nil
		}
		return false, errors.New("spend claim token does not belong to this worker attempt")
	})
	return err
}

func (r *Run) reconcileLimits(now time.Time) bool {
	changed := false
	for i := range r.Items {
		item := &r.Items[i]
		if item.Status == ItemCompleted || item.Status == ItemFailed || item.Status == ItemCancelled {
			continue
		}
		code, summary := "", ""
		if (!r.DeadlineAt.IsZero() && !now.Before(r.DeadlineAt)) || (!item.DeadlineAt.IsZero() && !now.Before(item.DeadlineAt)) {
			code, summary = "task_deadline_exceeded", "The persisted task deadline has been reached."
		} else if (r.MaxBudgetUSD > 0 && r.SpentUSD >= r.MaxBudgetUSD) || (item.MaxBudgetUSD > 0 && item.SpentUSD >= item.MaxBudgetUSD) {
			code, summary = "task_budget_exceeded", "The persisted estimated USD budget has been reached."
		}
		if code == "" {
			continue
		}
		item.Status, item.Owner, item.Lease = ItemFailed, "", nil
		item.Failure = &Failure{Code: code, Summary: summary, ObservedAt: now}
		item.UpdatedAt = now
		r.addEvent(now, code, item.ID, summary, "")
		changed = true
	}
	return changed
}

func RemainingBudget(run Run, item WorkItem) float64 {
	remaining := 0.0
	for _, limit := range [][2]float64{{run.MaxBudgetUSD, run.SpentUSD}, {item.MaxBudgetUSD, item.SpentUSD}} {
		if limit[0] > 0 {
			value := math.Max(0, limit[0]-limit[1])
			if remaining == 0 || value < remaining {
				remaining = value
			}
		}
	}
	return remaining
}

func WorkDeadline(run Run, item WorkItem) time.Time {
	deadline := run.DeadlineAt
	if !item.DeadlineAt.IsZero() && (deadline.IsZero() || item.DeadlineAt.Before(deadline)) {
		deadline = item.DeadlineAt
	}
	return deadline
}
