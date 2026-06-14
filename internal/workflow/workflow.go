// Package workflow runs an ordered sequence of shell steps as one
// structured unit: each step gets its own name + exit status, a failure
// stops the run (remaining steps are marked skipped), and the result is
// a per-step summary rather than one opaque blob.
//
// This is the metis answer to MiMo-Code's `workflow` tool. MiMo embeds
// a JS runtime to script multi-step flows; metis deliberately doesn't —
// a Go agent CLI shouldn't ship a JS engine. Instead a workflow is a
// declarative list of shell steps, which covers the real need (build →
// test → lint, run-then-commit) while staying observable: unlike a
// `cmd1 && cmd2 && cmd3` Bash chain — where a failure is one buried
// non-zero exit — every step here reports its own status, so the model
// sees exactly which one broke. Named workflows persist so common
// sequences are reusable.
package workflow

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/spill"
)

// Step is one shell command in a workflow.
type Step struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// Workflow is a named, ordered list of steps.
type Workflow struct {
	Name  string `json:"name"`
	Steps []Step `json:"steps"`
}

// Status values for a StepResult.
const (
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

// StepResult is the outcome of one step.
type StepResult struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Status   string `json:"status"`
	Output   string `json:"output"`
}

// RunOptions tunes a workflow run.
type RunOptions struct {
	Cwd            string        // working directory for every step
	StopOnError    bool          // mark remaining steps skipped after a failure
	PerStepTimeout time.Duration // 0 → DefaultStepTimeout
	MaxOutputBytes int           // 0 → DefaultMaxOutputBytes; per-step output cap
}

const (
	// DefaultStepTimeout bounds a single step so a hung command can't
	// stall the whole agent turn.
	DefaultStepTimeout = 5 * time.Minute
	// DefaultMaxOutputBytes caps each step's captured output, with the
	// same error-aware tail preservation spill uses.
	DefaultMaxOutputBytes = 16 * 1024
)

// Run executes wf's steps in order and returns a per-step summary. A
// failed step (non-zero exit, timeout, or spawn error) stops the run
// when StopOnError; the remaining steps are returned with StatusSkipped.
func Run(ctx context.Context, wf Workflow, opts RunOptions) []StepResult {
	timeout := opts.PerStepTimeout
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}
	maxBytes := opts.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxOutputBytes
	}

	results := make([]StepResult, 0, len(wf.Steps))
	stopped := false
	for _, s := range wf.Steps {
		if stopped {
			results = append(results, StepResult{Name: s.Name, Command: s.Command, Status: StatusSkipped})
			continue
		}
		r := runStep(ctx, s, opts.Cwd, timeout, maxBytes)
		results = append(results, r)
		if r.Status == StatusFailed && opts.StopOnError {
			stopped = true
		}
	}
	return results
}

func runStep(ctx context.Context, s Step, cwd string, timeout time.Duration, maxBytes int) StepResult {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", s.Command)
	if cwd != "" {
		cmd.Dir = cwd
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := capOutput(buf.String(), maxBytes)

	res := StepResult{Name: s.Name, Command: s.Command, Output: out, Status: StatusOK}
	if cctx.Err() == context.DeadlineExceeded {
		res.Status = StatusFailed
		res.ExitCode = -1
		res.Output = out + "\n[step timed out after " + timeout.String() + "]"
		return res
	}
	if err != nil {
		res.Status = StatusFailed
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Output = out + "\n[failed to start: " + err.Error() + "]"
		}
	}
	return res
}

// capOutput truncates step output to max bytes, preserving the tail
// when it carries error signal (reuses spill's error-aware logic so a
// failing step's verdict — which lands at the end — survives).
func capOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := s[:max*7/10]
	// snap head to a rune boundary
	for len(head) > 0 && head[len(head)-1]&0xC0 == 0x80 {
		head = head[:len(head)-1]
	}
	if spill.HasErrorMarker(s[len(head):]) {
		tail := s[len(s)-max*3/10:]
		for len(tail) > 0 && tail[0]&0xC0 == 0x80 {
			tail = tail[1:]
		}
		return head + "\n... [middle omitted; tail kept — error output] ...\n" + tail
	}
	return head + "\n... [output truncated] ..."
}

// Failed reports whether any step in the results failed.
func Failed(results []StepResult) bool {
	for _, r := range results {
		if r.Status == StatusFailed {
			return true
		}
	}
	return false
}

// Commands returns the step commands in order — used by the tool's
// permission check to gate every command before any of them runs.
func (wf Workflow) Commands() []string {
	out := make([]string, 0, len(wf.Steps))
	for _, s := range wf.Steps {
		out = append(out, strings.TrimSpace(s.Command))
	}
	return out
}
