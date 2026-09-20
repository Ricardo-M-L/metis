package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/execution"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

// projectCoordinatorStore is intentionally shared by CLI, Desktop and
// LLM-facing tool construction: the project graph and Agent's execution
// recovery profiles live under the same METIS home, but in separate private
// directories with separate schemas.
func projectCoordinatorStore() *projectcoord.Store {
	home := config.Home()
	return projectcoord.NewStore(
		filepath.Join(home, "project-coordinator"),
		projectcoord.WithExecutionMemory(execution.NewStore(filepath.Join(home, "execution"))),
	)
}

func projectCoordinatorWorkspace(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		value = cwd
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return execution.CanonicalWorkspace(abs)
}

func cmdProjectCoordinatorCreate(args []string) error {
	fs := flag.NewFlagSet("coordinator create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	goal := fs.String("goal", "", "project objective")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*goal) == "" && len(fs.Args()) > 0 {
		*goal = strings.Join(fs.Args(), " ")
	}
	if strings.TrimSpace(*goal) == "" {
		return errors.New("usage: metis coordinator create --goal \"<goal>\" [--cwd DIR]")
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	run, err := projectCoordinatorStore().Create(projectcoord.CreateInput{Workspace: workspace, Goal: *goal})
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(run)
	}
	fmt.Fprintf(os.Stdout, "Created project run %s\nWorkspace: %s\nGoal: %s\n\n", run.ID, run.Workspace, run.Goal)
	printCoordinatorRun(os.Stdout, run)
	fmt.Fprintf(os.Stdout, "\nStart the first worker with:\n  metis coordinator run %s --cwd %q\n", run.ID, run.Workspace)
	return nil
}

func cmdProjectCoordinatorList(args []string) error {
	fs := flag.NewFlagSet("coordinator list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	runs, err := projectCoordinatorStore().List(workspace)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(map[string]any{"workspace": workspace, "runs": runs})
	}
	if len(runs) == 0 {
		fmt.Fprintln(os.Stdout, "(no durable project runs for this workspace)")
		return nil
	}
	fmt.Fprintln(os.Stdout, "RUN ID                              STATUS      UPDATED       GOAL")
	for _, run := range runs {
		goal := oneLine(run.Goal, 60)
		fmt.Fprintf(os.Stdout, "%-34s  %-10s  %-12s  %s\n", run.ID, run.Status, coordinatorAge(time.Since(run.UpdatedAt)), goal)
	}
	return nil
}

func cmdProjectCoordinatorStatus(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis coordinator status <runID> [--cwd DIR] [--json]")
	}
	runID := args[0]
	fs := flag.NewFlagSet("coordinator status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	run, err := projectCoordinatorStore().Get(workspace, runID)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(run)
	}
	printCoordinatorRun(os.Stdout, run)
	return nil
}

func cmdProjectCoordinatorAdd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis coordinator add <runID> --subject \"<title>\" --prompt \"<work>\" [--phase custom] [--depends-on id,id]")
	}
	runID := args[0]
	fs := flag.NewFlagSet("coordinator add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	id := fs.String("id", "", "optional stable work-item identifier")
	phase := fs.String("phase", string(projectcoord.PhaseCustom), "research|synthesis|implementation|verification|custom")
	subject := fs.String("subject", "", "work-item title")
	prompt := fs.String("prompt", "", "worker instruction")
	dependsOn := fs.String("depends-on", "", "comma-separated prerequisite work-item IDs")
	maxAttempts := fs.Int("max-attempts", 2, "maximum attempts, including the initial attempt")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	item, err := projectCoordinatorStore().AddWorkItem(workspace, runID, projectcoord.WorkItemInput{
		ID:          *id,
		Phase:       projectcoord.Phase(*phase),
		Subject:     *subject,
		Prompt:      *prompt,
		DependsOn:   splitCoordinatorIDs(*dependsOn),
		MaxAttempts: *maxAttempts,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(item)
	}
	fmt.Fprintf(os.Stdout, "Added %s (%s, %s)\n", item.ID, item.Phase, item.Status)
	return nil
}

func cmdProjectCoordinatorClaim(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis coordinator claim <runID> --worker NAME [--lease 15m] [--prompt]")
	}
	runID := args[0]
	fs := flag.NewFlagSet("coordinator claim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	worker := fs.String("worker", "", "worker identity")
	lease := fs.Duration("lease", 15*time.Minute, "claim lease duration")
	includePrompt := fs.Bool("prompt", false, "include the assembled worker prompt")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	store := projectCoordinatorStore()
	item, err := store.ClaimNext(workspace, runID, projectcoord.ClaimOptions{Worker: *worker, Lease: *lease})
	if err != nil {
		return err
	}
	if *jsonOut {
		payload := map[string]any{"item": item}
		if *includePrompt {
			run, err := store.Get(workspace, runID)
			if err != nil {
				return err
			}
			payload["worker_prompt"] = projectcoord.BuildWorkerPrompt(run, item)
		}
		return writeCoordinatorJSON(payload)
	}
	fmt.Fprintf(os.Stdout, "Claimed %s: %s\n", item.ID, item.Subject)
	if *includePrompt {
		run, err := store.Get(workspace, runID)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "\n"+projectcoord.BuildWorkerPrompt(run, item))
	}
	return nil
}

func cmdProjectCoordinatorComplete(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: metis coordinator complete <runID> <itemID> [--worker NAME] [--output TEXT] [--cwd DIR]")
	}
	runID, itemID := args[0], args[1]
	fs := flag.NewFlagSet("coordinator complete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	worker := fs.String("worker", "", "claiming worker identity")
	output := fs.String("output", "", "durable result/evidence")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	item, err := projectCoordinatorStore().Complete(workspace, runID, itemID, *worker, *output)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(item)
	}
	fmt.Fprintf(os.Stdout, "Completed %s\n", item.ID)
	return nil
}

func cmdProjectCoordinatorFail(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: metis coordinator fail <runID> <itemID> --summary \"<reason>\" [--code CODE] [--worker NAME] [--cwd DIR]")
	}
	runID, itemID := args[0], args[1]
	fs := flag.NewFlagSet("coordinator fail", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	worker := fs.String("worker", "", "claiming worker identity")
	code := fs.String("code", projectcoord.FailureWorkerExecution, "stable failure code")
	summary := fs.String("summary", "", "why the work is blocked or failed")
	action := fs.String("suggested-action", "", "how a coordinator can recover")
	output := fs.String("output", "", "worker output/evidence")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if strings.TrimSpace(*summary) == "" {
		return errors.New("--summary is required")
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	item, err := projectCoordinatorStore().Fail(workspace, runID, itemID, *worker, projectcoord.Failure{
		Code:            *code,
		Summary:         *summary,
		SuggestedAction: *action,
	}, *output)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(item)
	}
	if item.Failure != nil && item.Failure.AutoRecovered {
		fmt.Fprintf(os.Stdout, "Recorded and automatically requeued %s: %s\n", item.ID, item.Failure.Summary)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Recorded failure for %s\n", item.ID)
	return nil
}

func cmdProjectCoordinatorRecover(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: metis coordinator recover <runID> <itemID> [--note TEXT] [--force] [--cwd DIR]")
	}
	runID, itemID := args[0], args[1]
	fs := flag.NewFlagSet("coordinator recover", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	note := fs.String("note", "", "why the underlying cause is now fixed")
	force := fs.Bool("force", false, "allow the one explicit retry after attempt budget")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	item, err := projectCoordinatorStore().Recover(workspace, runID, itemID, *note, *force)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(item)
	}
	fmt.Fprintf(os.Stdout, "Requeued %s (%s)\n", item.ID, item.Status)
	return nil
}

// cmdProjectCoordinatorRun is the autonomous bridge from the durable graph to
// a real METIS worker.  It claims an item before model work, writes either
// result evidence or a failure reason afterwards, and never leaves a silent
// successful result outside the project state file.
func cmdProjectCoordinatorRun(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis coordinator run <runID> [--cwd DIR] [--worker NAME] [--max-items N|--until-idle]")
	}
	runID := args[0]
	fs := flag.NewFlagSet("coordinator run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	worker := fs.String("worker", fmt.Sprintf("coordinator-%d", os.Getpid()), "worker identity")
	lease := fs.Duration("lease", 15*time.Minute, "claim lease duration")
	maxItems := fs.Int("max-items", 1, "maximum ready work items to execute")
	untilIdle := fs.Bool("until-idle", false, "continue until the graph has no ready work")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON outcomes")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *maxItems < 1 {
		return errors.New("--max-items must be at least 1")
	}
	if *untilIdle {
		*maxItems = 1000
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}

	previousCWD, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(workspace); err != nil {
		return err
	}
	defer func() { _ = os.Chdir(previousCWD) }()

	store := projectCoordinatorStore()
	var rt *runtime
	defer func() {
		if rt != nil {
			rt.Cleanup()
		}
	}()
	completed := 0
	outcomes := make([]coordinatorRunOutcome, 0, *maxItems)
	for completed < *maxItems {
		item, claimErr := store.ClaimNext(workspace, runID, projectcoord.ClaimOptions{Worker: *worker, Lease: *lease})
		if errors.Is(claimErr, projectcoord.ErrNoReadyWork) {
			break
		}
		if claimErr != nil {
			return claimErr
		}
		run, getErr := store.Get(workspace, runID)
		if getErr != nil {
			return getErr
		}
		if rt == nil {
			rt, err = setupRuntime(ctx, &cliFlags{})
			if err != nil {
				_, _ = store.Fail(workspace, runID, item.ID, *worker, projectcoord.Failure{
					Code:            projectcoord.FailureWorkerExecution,
					Summary:         "METIS worker runtime could not start: " + err.Error(),
					SuggestedAction: "Fix the runtime configuration or authentication, then requeue this work item.",
				}, "")
				return err
			}
		}
		prompt := projectcoord.BuildWorkerPrompt(run, item)
		output, workerErr := runOneShotForCoordinator(ctx, rt, prompt)
		outcome := coordinatorRunOutcome{ItemID: item.ID, Subject: item.Subject, Output: output}
		if workerErr != nil {
			failure := coordinatorFailureFromText(workerErr.Error())
			result, recordErr := store.Fail(workspace, runID, item.ID, *worker, failure, output)
			if recordErr != nil {
				return recordErr
			}
			outcome.Status = string(result.Status)
			outcome.Error = failure.Summary
		} else if workerReportedBlocked(output) {
			failure := coordinatorFailureFromText(output)
			failure.Code = projectcoord.FailureWorkerBlocked
			failure.Recoverable = false
			result, recordErr := store.Fail(workspace, runID, item.ID, *worker, failure, output)
			if recordErr != nil {
				return recordErr
			}
			outcome.Status = string(result.Status)
			outcome.Error = failure.Summary
		} else {
			result, recordErr := store.Complete(workspace, runID, item.ID, *worker, output)
			if recordErr != nil {
				return recordErr
			}
			outcome.Status = string(result.Status)
		}
		outcomes = append(outcomes, outcome)
		completed++
		if !*jsonOut {
			fmt.Fprintf(os.Stdout, "\n[%s] %s — %s\n", outcome.Status, outcome.ItemID, outcome.Subject)
			if strings.TrimSpace(outcome.Output) != "" {
				fmt.Fprintln(os.Stdout, outcome.Output)
			}
			if outcome.Error != "" {
				fmt.Fprintln(os.Stdout, "Reason:", outcome.Error)
			}
		}
	}
	finalRun, err := store.Get(workspace, runID)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(map[string]any{"outcomes": outcomes, "run": finalRun})
	}
	if completed == 0 {
		fmt.Fprintf(os.Stdout, "No ready project work. Run %s is %s.\n", finalRun.ID, finalRun.Status)
	} else {
		fmt.Fprintf(os.Stdout, "\nProject run %s is now %s.\n", finalRun.ID, finalRun.Status)
	}
	return nil
}

type coordinatorRunOutcome struct {
	ItemID  string `json:"item_id"`
	Subject string `json:"subject"`
	Status  string `json:"status"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
}

func workerReportedBlocked(output string) bool {
	// The worker prompt itself documents this marker. A provider that echoes
	// the prompt (or a worker quoting the instruction) must not turn a normal
	// completion into a durable failure merely because the marker appeared in
	// prose. Accept it only as a standalone status line in the final response.
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "`")
		if strings.HasPrefix(strings.ToLower(line), "metis_project_status: blocked") {
			return true
		}
	}
	return false
}

func coordinatorFailureFromText(text string) projectcoord.Failure {
	clean := strings.TrimSpace(text)
	lower := strings.ToLower(clean)
	if strings.Contains(lower, "worktree") && (strings.Contains(lower, "not a git repository") || strings.Contains(lower, "requires an existing git repository")) {
		return projectcoord.Failure{
			Code:            string(execution.FailureWorktreeRequiresGit),
			Summary:         "Git worktree isolation was requested in a non-Git workspace.",
			SuggestedAction: "METIS can continue once in the same directory with direct execution; the rule is retained for future Agent dispatches.",
		}
	}
	if clean == "" {
		clean = "Worker did not produce a completion result."
	}
	return projectcoord.Failure{
		Code:            projectcoord.FailureWorkerExecution,
		Summary:         oneLine(clean, 1024),
		SuggestedAction: "Inspect the recorded worker output, fix the cause, then requeue the work item.",
	}
}

func splitCoordinatorIDs(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func printCoordinatorRun(w *os.File, run projectcoord.Run) {
	fmt.Fprintf(w, "Run:       %s\n", run.ID)
	fmt.Fprintf(w, "Status:    %s\n", run.Status)
	fmt.Fprintf(w, "Workspace: %s\n", run.Workspace)
	fmt.Fprintf(w, "Goal:      %s\n", run.Goal)
	fmt.Fprintf(w, "Environment: git=%t", run.Environment.IsGitRepository)
	if run.Environment.IsLinkedWorktree {
		fmt.Fprint(w, ", linked-worktree=true")
	}
	fmt.Fprintln(w)
	if failure := run.Environment.LastFailure; failure != nil {
		fmt.Fprintf(w, "Remembered environment rule: %s\n", failure.Summary)
	}
	fmt.Fprintln(w, "\nWORK ITEM                         PHASE            STATUS        ATTEMPTS  OWNER")
	for _, item := range run.Items {
		owner := item.Owner
		if item.Failure != nil && item.Status == projectcoord.ItemFailed {
			owner = oneLine(item.Failure.Summary, 38)
		}
		fmt.Fprintf(w, "%-32s  %-15s  %-12s  %d/%d       %s\n", oneLine(item.Subject, 32), item.Phase, item.Status, item.Attempts, item.MaxAttempts, owner)
	}
	if len(run.Events) > 0 {
		last := run.Events[len(run.Events)-1]
		fmt.Fprintf(w, "\nLast event: %s — %s\n", last.Kind, last.Detail)
	}
}

func writeCoordinatorJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func oneLine(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	if max > 0 && len(value) > max {
		if max == 1 {
			return "…"
		}
		return value[:max-1] + "…"
	}
	return value
}

func coordinatorAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}
