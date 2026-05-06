package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Runner spawns `metis run <prompt>` for a scenario and parses the
// captured output into a RunResult. The MetisBinary defaults to
// "metis" on $PATH if empty.
type Runner struct {
	MetisBinary string        // path or name on $PATH
	WorkDir     string        // cwd for the subprocess; empty → caller's cwd
	ExtraArgs   []string      // appended after `run`, before the prompt
	Env         []string      // appended to os.Environ for the subprocess
	StreamLogs  io.Writer     // optional: tee stderr here for debugging
	GlobalGrace time.Duration // added to Scenario.Timeout to give shutdown time
}

// Run executes one scenario.
//
// Stdout from `metis run` is treated as the agent's final response.
// Tool calls aren't emitted on stdout in non-interactive mode, so we
// scrape them from stderr (which carries the runtime log lines like
// "tool=Read"). This is best-effort; a scenario that asserts on
// used_tool and gets back tool_calls = nil still scores the response
// on its other assertions.
func (rn *Runner) Run(ctx context.Context, s Scenario) RunResult {
	bin := rn.MetisBinary
	if bin == "" {
		bin = "metis"
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	timeout += rn.GlobalGrace
	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append([]string{"run"}, rn.ExtraArgs...)
	args = append(args, s.Prompt)
	cmd := exec.CommandContext(subCtx, bin, args...)
	if rn.WorkDir != "" {
		cmd.Dir = rn.WorkDir
	}
	if len(rn.Env) > 0 {
		cmd.Env = append(os.Environ(), rn.Env...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if rn.StreamLogs != nil {
		cmd.Stderr = io.MultiWriter(&stderr, rn.StreamLogs)
	} else {
		cmd.Stderr = &stderr
	}

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	res := RunResult{
		ScenarioID: s.ID,
		Response:   strings.TrimSpace(stdout.String()),
		ToolCalls:  scrapeToolCalls(stderr.String()),
		Duration:   dur,
		ExitCode:   cmd.ProcessState.ExitCode(),
	}
	if err != nil {
		if subCtx.Err() == context.DeadlineExceeded {
			res.Err = fmt.Errorf("timeout after %s: %w", timeout, err)
		} else {
			res.Err = err
		}
	}
	return res
}

// toolCallPattern matches the runtime log line metis emits when a tool
// fires in non-interactive mode. The format is stable and deliberate
// for the eval runner.
var toolCallPattern = regexp.MustCompile(`(?m)^.*\btool\s*[=:]\s*"?([A-Za-z][A-Za-z0-9_-]*)"?`)

// scrapeToolCalls pulls tool names out of metis's stderr log. Returns
// in invocation order with no dedup — a scenario can assert
// used_tool: Read even if Read fired three times.
func scrapeToolCalls(stderr string) []string {
	matches := toolCallPattern.FindAllStringSubmatch(stderr, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) > 1 && m[1] != "" {
			out = append(out, m[1])
		}
	}
	return out
}

// RunAll iterates scenarios serially and aggregates a Report. ctx
// applies to the whole batch; per-scenario timeout is the Scenario's
// Timeout. Set rn.StreamLogs to follow progress live.
func (rn *Runner) RunAll(ctx context.Context, scenarios []Scenario) Report {
	rep := Report{
		Started:     time.Now(),
		MetisBinary: rn.MetisBinary,
	}
	for _, s := range scenarios {
		if ctx.Err() != nil {
			break
		}
		r := rn.Run(ctx, s)
		rep.Scores = append(rep.Scores, ComputeReward(s, r))
	}
	rep.Finished = time.Now()
	return rep
}

// WriteJSONL streams a Report as one JSON object per scenario followed
// by a final summary object. This format works as input for any tool
// that reads JSONL (jq, datasette, etc).
func WriteJSONL(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	for _, s := range rep.Scores {
		if err := enc.Encode(scoreLine(s)); err != nil {
			return err
		}
	}
	summary := map[string]any{
		"type":       "summary",
		"pass_rate":  rep.PassRate(),
		"avg_score":  rep.AvgScore(),
		"total":      len(rep.Scores),
		"passed":     countPassed(rep),
		"started":   rep.Started.Format(time.RFC3339),
		"finished":  rep.Finished.Format(time.RFC3339),
		"duration_s": rep.Finished.Sub(rep.Started).Seconds(),
		"binary":    rep.MetisBinary,
	}
	return enc.Encode(summary)
}

func countPassed(rep Report) int {
	n := 0
	for _, s := range rep.Scores {
		if s.Passed {
			n++
		}
	}
	return n
}

func scoreLine(s Score) map[string]any {
	out := map[string]any{
		"type":     "score",
		"id":       s.ScenarioID,
		"total":    s.Total,
		"passed":   s.Passed,
		"duration_ms": s.Duration.Milliseconds(),
	}
	if s.Err != nil {
		out["error"] = s.Err.Error()
	}
	if len(s.Breakdown) > 0 {
		bd := make([]map[string]any, 0, len(s.Breakdown))
		for _, a := range s.Breakdown {
			bd = append(bd, map[string]any{
				"type":   string(a.Type),
				"weight": a.Weight,
				"earned": a.Earned,
				"note":   a.Note,
			})
		}
		out["breakdown"] = bd
	}
	return out
}
