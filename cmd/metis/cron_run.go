package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/google/uuid"
)

var setupCronRuntime = setupRuntime

func parseCronRunArgs(args []string) (id, runID string, err error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", "", errors.New("usage: metis cron run <id> [--run-id <uuid>]")
	}
	id = args[0]
	for i := 1; i < len(args); i++ {
		if runID != "" {
			return "", "", errors.New("cron run: unexpected argument")
		}
		switch {
		case args[i] == "--run-id" && i+1 < len(args):
			i++
			runID = args[i]
		case strings.HasPrefix(args[i], "--run-id="):
			runID = strings.TrimPrefix(args[i], "--run-id=")
		default:
			return "", "", fmt.Errorf("cron run: unknown or incomplete option %q", args[i])
		}
		if parsed, parseErr := uuid.Parse(runID); parseErr != nil || parsed.String() != strings.ToLower(runID) {
			return "", "", errors.New("cron run: --run-id must be a UUID")
		}
	}
	return id, runID, nil
}

// runRecordedCronJob owns admission, startup, conversation persistence, and
// teardown. A startup failure is still a run; a busy manual invocation never
// changes the job's RunCount/NextRun. Runtime creation per fire prevents every
// job in a daemon from sharing one session, MCP state, and recovery pointer.
func runRecordedCronJob(ctx context.Context, svc *agent.CronService, job *agent.CronJob, runID, trigger string,
	persistentHist, mainHist map[string][]llm.Message) (resultErr error) {
	run, err := agent.BeginCronRun(svc.Root(), job.ID, runID, trigger)
	if err != nil {
		return err
	}
	var rt *runtime
	defer func() {
		if ctx.Err() != nil {
			resultErr = errors.Join(resultErr, ctx.Err())
		}
		status := "succeeded"
		if resultErr != nil {
			status = "failed"
		}
		if errors.Is(resultErr, context.Canceled) || errors.Is(resultErr, context.DeadlineExceeded) {
			status = "cancelled"
		}
		output := ""
		if rt != nil && rt.loop != nil {
			output = cronFinalOutput(rt.loop.History())
		}
		resultErr = errors.Join(resultErr, run.Finish(status, output, output, resultErr))
	}()
	if trigger == "manual" {
		job, err = advanceManualCronRun(svc, job.ID)
		if err != nil {
			return err
		}
	} else {
		// A schedule claim releases storage before entering this callback. A
		// deletion may win the execution lock in that gap; do not run its stale
		// callback snapshot after the job has been removed.
		fresh, err := agent.NewCronService(svc.Root())
		if err != nil {
			return err
		}
		if _, exists := fresh.Get(job.ID); !exists {
			return errors.New("cron job was removed before execution")
		}
	}
	if job.WorkDir != "" {
		if !filepath.IsAbs(job.WorkDir) {
			return errors.New("cron workspace must be an absolute path")
		}
		original, err := os.Getwd()
		if err != nil {
			return err
		}
		if err := os.Chdir(job.WorkDir); err != nil {
			return fmt.Errorf("open cron workspace: %w", err)
		}
		// This is the dedicated CLI/daemon process. onFire is serial and this
		// restore executes after runtime Cleanup has joined its background work.
		defer func() { resultErr = errors.Join(resultErr, os.Chdir(original)) }()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Force default regardless of the workspace's ambient fullAccess mode.
	// EvaluateCronPermission remains the pre-authorization hard boundary.
	rt, err = setupCronRuntime(ctx, &cliFlags{
		mode: string(permission.ModeDefault), cronSessionDir: filepath.Dir(svc.Root()),
		sessionName: "Cron · " + job.Name,
	})
	if err != nil {
		return err
	}
	defer rt.Cleanup()
	if rt.store == nil || rt.sessionID == "" {
		return errors.New("cron runtime has no durable session")
	}
	if _, _, err := rt.store.LoadHeader(rt.sessionID); err != nil {
		return fmt.Errorf("cron session was not saved: %w", err)
	}
	if err := run.SetSessionID(rt.sessionID); err != nil {
		return err
	}
	return executeCronJob(ctx, rt, job, persistentHist, mainHist, run)
}

func cronFinalOutput(history []llm.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == llm.RoleUser {
			break
		}
		if history[i].Role != llm.RoleAssistant {
			continue
		}
		var parts []string
		for _, block := range history[i].Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

func cmdCronHistory(svc *agent.CronService, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis cron history <id> [run-id] [--json] [--limit N]")
	}
	limit, runID, asJSON := 20, "", false
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--json":
			asJSON = true
		case args[i] == "--limit" && i+1 < len(args):
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				return errors.New("cron history: invalid limit")
			}
			limit = n
		case !strings.HasPrefix(args[i], "-") && runID == "":
			runID = args[i]
		default:
			return fmt.Errorf("cron history: unknown option %q", args[i])
		}
	}
	if runID != "" {
		record, err := agent.ReadCronRun(svc.Root(), args[0], runID)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(record)
		}
		fmt.Printf("%s  %s  %s\nSession: %s\n%s\n%s\n", record.ID, record.Status, record.StartedAt.Format("2006-01-02 15:04:05"), record.SessionID, record.Output, record.Error)
		return nil
	}
	records, err := agent.ListCronRuns(svc.Root(), args[0], limit)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(records)
	}
	for _, r := range records {
		fmt.Printf("%s  %-11s %-9s %s  %s\n", r.ID, r.Status, r.Trigger, r.StartedAt.Format("2006-01-02 15:04:05"), r.Summary)
	}
	return nil
}

// advanceManualCronRun records the manual fire before the prompt is executed
// and returns the immutable snapshot saved by that same storage transaction.
func advanceManualCronRun(svc *agent.CronService, id string) (*agent.CronJob, error) {
	return svc.RunNow(id)
}

func reportCronFireError(w io.Writer, job *agent.CronJob, err error) error {
	if err == nil {
		return nil
	}
	id := "unknown"
	if job != nil && job.ID != "" {
		id = job.ID
	}
	_, _ = fmt.Fprintf(w, "[cron] job %s failed: %v\n", id, err)
	return err
}
