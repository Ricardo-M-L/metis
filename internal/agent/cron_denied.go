package agent

// cron_denied.go — the "review before you approve" surface for
// unattended fires. When EvaluateCronPermission denies a tool call that
// wasn't pre-authorized, the fire records it here so the user can later
// see exactly what the job tried to do (`cron denied <id>`) and, if they
// agree, copy the Suggest rule straight into `cron allow <id> <rule>`.
//
// One append-only JSONL per job under <cronRoot>/denied/<id>.jsonl. Kept
// separate from the audit transcript (which only exists for --silent
// jobs) so denial review works for loud jobs too, and so `cron allow`
// can clear the file once the user has acted on it.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CronDenial is one blocked tool call from an unattended fire.
type CronDenial struct {
	At      time.Time `json:"at"`
	Tool    string    `json:"tool"`
	Input   string    `json:"input"`   // flattened (command / path / …), truncated
	Reason  string    `json:"reason"`  // EvaluateCronPermission tag
	Suggest string    `json:"suggest"` // ready-to-paste allow rule
}

// cronDeniedPath is the per-job denial log location. Centralized so the
// writer (executeCronJob) and reader (`cron denied`) agree.
func cronDeniedPath(cronRoot, jobID string) string {
	return filepath.Join(cronRoot, "denied", jobID+".jsonl")
}

// SuggestCronRule turns a denied tool call into a copy-pasteable
// `Tool(content)` allow rule. For Bash it scopes to the leading command
// word (`echo cron-fired` → `Bash(echo:*)`) so approving doesn't open the
// whole shell; for everything else it's the bare tool name, which the
// user can narrow by hand if they want.
func SuggestCronRule(tool string, input map[string]any) string {
	if tool == "Bash" {
		cmd := strings.TrimSpace(stringifyToolInput(input))
		if cmd != "" {
			if i := strings.IndexAny(cmd, " \t\n"); i > 0 {
				cmd = cmd[:i]
			}
			return "Bash(" + cmd + ":*)"
		}
	}
	return tool
}

// RecordCronDenial appends one denial to the job's log. Best-effort: a
// write failure must not derail the fire, so the error is returned for
// the caller to log-and-ignore.
//
// Deduplicated by suggested rule: a durable job firing every minute that
// the user never authorizes would otherwise append the same denial forever
// (unbounded file + a slower `cron denied` scan). Since the only actionable
// unit is the distinct rule to approve, recording the first occurrence of
// each Suggest is sufficient — re-denials of an already-logged rule are
// dropped.
func RecordCronDenial(cronRoot, jobID string, d CronDenial) error {
	if cronRoot == "" || jobID == "" {
		return nil
	}
	if existing, err := ListCronDenials(cronRoot, jobID); err == nil {
		for _, e := range existing {
			if e.Suggest == d.Suggest {
				return nil // this rule is already pending approval
			}
		}
	}
	if d.At.IsZero() {
		d.At = time.Now()
	}
	if len(d.Input) > 300 {
		d.Input = d.Input[:300] + "…"
	}
	path := cronDeniedPath(cronRoot, jobID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(&d)
}

// ListCronDenials reads a job's denial log, oldest first. A missing file
// means "nothing blocked" → empty slice, nil error.
func ListCronDenials(cronRoot, jobID string) ([]CronDenial, error) {
	f, err := os.Open(cronDeniedPath(cronRoot, jobID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []CronDenial
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var d CronDenial
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			continue // skip a corrupt line rather than fail the whole read
		}
		out = append(out, d)
	}
	return out, sc.Err()
}

// ClearCronDenials removes a job's denial log — called after `cron allow`
// acts on the listed denials so the next `cron denied` starts clean. A
// missing file is not an error.
func ClearCronDenials(cronRoot, jobID string) error {
	err := os.Remove(cronDeniedPath(cronRoot, jobID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
