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
	"github.com/Ricardo-M-L/metis/internal/exitcode"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// projectCoordinatorStore is intentionally shared by CLI, Desktop and
// LLM-facing tool construction: the project graph and Agent's execution
// recovery profiles live under the same METIS home, but in separate private
// directories with separate schemas.
func projectCoordinatorStore(options ...projectcoord.Option) *projectcoord.Store {
	home := config.Home()
	storeOptions := append([]projectcoord.Option{
		projectcoord.WithExecutionMemory(execution.NewStore(filepath.Join(home, "execution"))),
		projectcoord.WithPrincipalCheck(checkPersistentJobReady),
	}, options...)
	return projectcoord.NewStore(
		filepath.Join(home, "project-coordinator"),
		storeOptions...,
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
	deadline := fs.String("deadline", "", "absolute deadline in RFC3339")
	maxBudget := fs.Float64("max-budget-usd", 0, "persistent estimated USD cap across worker attempts")
	var checks coordinatorStrings
	fs.Var(&checks, "check", "host acceptance command for verification; repeat for multiple checks")
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
	deadlineAt, err := coordinatorDeadline(*deadline)
	if err != nil {
		return err
	}
	run, err := projectCoordinatorStore().Create(projectcoord.CreateInput{Workspace: workspace, Goal: *goal, DeadlineAt: deadlineAt, MaxBudgetUSD: *maxBudget, CheckCommands: checks})
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
	profile := fs.String("agent", "", "agent profile to use for this work item")
	jobID := fs.String("job", "", "persistent job whose frozen definition owns this work item")
	deadline := fs.String("deadline", "", "absolute deadline in RFC3339")
	maxBudget := fs.Float64("max-budget-usd", 0, "persistent estimated USD cap for this work item")
	var deliverables coordinatorStrings
	fs.Var(&deliverables, "deliverable", "required workspace-relative output file; repeat for multiple files")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	deadlineAt, err := coordinatorDeadline(*deadline)
	if err != nil {
		return err
	}
	var binding *principal.Binding
	if *jobID != "" {
		j, err := persistentJobStore().Get(*jobID)
		if err != nil {
			return err
		}
		if workspace != j.Definition.Workspace {
			return errors.New("persistent job workspace differs from project")
		}
		if *profile != "" && *profile != j.Definition.Profile.Name {
			return errors.New("--agent conflicts with the persistent job's frozen profile")
		}
		snapshot := j.Binding()
		binding = &snapshot
		*profile = j.Definition.Profile.Name
		if err := checkPersistentJobReady(binding); err != nil {
			return err
		}
	}
	item, err := projectCoordinatorStore().AddWorkItem(workspace, runID, projectcoord.WorkItemInput{
		ID:               *id,
		Phase:            projectcoord.Phase(*phase),
		Subject:          *subject,
		Prompt:           *prompt,
		DependsOn:        splitCoordinatorIDs(*dependsOn),
		MaxAttempts:      *maxAttempts,
		AgentProfile:     *profile,
		PrincipalBinding: binding,
		DeadlineAt:       deadlineAt,
		MaxBudgetUSD:     *maxBudget,
		Deliverables:     deliverables,
	})
	if err != nil {
		return err
	}
	if binding != nil {
		if _, err := persistentJobStore().RegisterRun(*binding, runID); err != nil {
			return err
		}
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
	fmt.Fprintf(os.Stdout, "Claimed %s: %s\nLease token: %s\n", item.ID, item.Subject, item.Lease.Token)
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
	token := fs.String("lease-token", "", "claim token returned by claim")
	output := fs.String("output", "", "durable result/evidence")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	item, err := projectCoordinatorStore().Complete(context.Background(), workspace, runID, itemID, projectcoord.CompletionInput{Worker: *worker, Token: *token, Output: *output})
	if err != nil {
		return err
	}
	if err := reconcileCoordinatorLearning(context.Background(), projectCoordinatorStore(), workspace, runID); err != nil {
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
	token := fs.String("lease-token", "", "claim token returned by claim")
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
	item, err := projectCoordinatorStore().Fail(workspace, runID, itemID, *worker, *token, projectcoord.Failure{
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
	ctx, stopParentWatch, parentErr := coordinatorChildContext(ctx)
	if parentErr != nil {
		return parentErr
	}
	defer stopParentWatch()
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
	summaryJSON := fs.Bool("summary-json", false, "emit only outcome count, run ID and status for a resident service")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *maxItems < 1 {
		return errors.New("--max-items must be at least 1")
	}
	if *jsonOut && *summaryJSON {
		return errors.New("--json and --summary-json are mutually exclusive")
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
		if ctx.Err() != nil {
			break
		}
		item, claimErr := store.ClaimNext(workspace, runID, projectcoord.ClaimOptions{Worker: *worker, Lease: *lease})
		if errors.Is(claimErr, projectcoord.ErrNoReadyWork) {
			break
		}
		if claimErr != nil {
			return claimErr
		}
		workCtx, stopLease, leaseErr := store.KeepLease(ctx, workspace, runID, item, *lease)
		if leaseErr != nil {
			return leaseErr
		}
		run, getErr := store.Get(workspace, runID)
		if getErr != nil {
			_ = stopLease()
			return getErr
		}
		if inputErr := projectcoord.ValidateHandoffInputs(workCtx, workspace, item); inputErr != nil {
			// Admission may precede execution by hours. Recheck immutable input
			// hashes before runtime startup or any paid provider dispatch.
			failure := projectcoord.Failure{Code: "handoff_input_invalid", Summary: inputErr.Error(), SuggestedAction: "Restore the declared input artifact and explicitly recover the assigned task.", Recoverable: false}
			if workCtx.Err() != nil {
				failure = coordinatorWorkerFailure(workCtx, inputErr)
			}
			_, recordErr := recordCoordinatorFailure(store, workspace, runID, item, *worker, failure, "")
			_ = stopLease()
			if recordErr != nil {
				return recordErr
			}
			return inputErr
		}
		workCtx, job, principalLease, stopJob, jobErr := preparePersistentJobWorker(workCtx, store, run, item, *worker, *lease)
		if jobErr != nil {
			_, _ = store.ReleaseUnstartedClaim(workspace, runID, item.ID, *worker, item.Lease.Token)
			_ = stopLease()
			return jobErr
		}
		stopWorkerLease := stopLease
		stopLease = func() error { stopJob(); return stopWorkerLease() }
		cancelDeadline := func() {}
		if deadline := projectcoord.WorkDeadline(run, item); !deadline.IsZero() {
			workCtx, cancelDeadline = context.WithDeadline(workCtx, deadline)
		}
		tracker := rtpkg.NewBudgetTracker(persistentJobRemainingBudget(run, item, job), job.Definition.Profile.Model)
		tracker.SetPersistence(func(spent float64) error {
			return store.RecordSpend(workspace, runID, item.ID, *worker, item.Lease.Token, spent)
		})
		var ordinaryObserver *coordinatorUsageObserver
		if item.PrincipalBinding != nil {
			workCtx = coordinatorPersistentJobUsageContext(workCtx, store, workspace, runID, run, item, *worker, item.PrincipalBinding.Definition.Provider, tracker, principalLease)
		} else {
			workCtx, ordinaryObserver = coordinatorExecutionUsageContext(workCtx, store, workspace, runID, run, item, *worker, tracker)
		}
		// Every assignment starts from its own transcript and immutable role.
		// Reusing a runtime would leak the previous item's history and permissions.
		{
			if rt != nil {
				rt.Cleanup()
			}
			runtimeFlags, flagErr := persistentJobRuntimeFlags(item, job)
			if flagErr != nil {
				_ = stopLease()
				cancelDeadline()
				return flagErr
			}
			runtimeFlags.jobExecutionContext = workCtx
			rt, err = setupRuntime(workCtx, runtimeFlags)
			if err != nil {
				failure := coordinatorWorkerFailure(ctx, err)
				_, _ = recordCoordinatorFailure(store, workspace, runID, item, *worker, failure, "")
				_ = stopLease()
				cancelDeadline()
				return err
			}
			if ordinaryObserver != nil {
				ordinaryObserver.bindExecutionIdentity(rt.providerName, rt.loop.Model)
			}
			applyCoordinatorDurableSystem(rt.loop)
			if bindErr := configureCoordinatorHandoff(rt, run, item); bindErr != nil {
				// Reject an incorrect assignment or missing durable checkpoint
				// before the worker's model can see or act on another task.
				rt.Cleanup()
				rt = nil
				failure := coordinatorWorkerFailure(workCtx, bindErr)
				_, recordErr := recordCoordinatorFailure(store, workspace, runID, item, *worker, failure, "")
				_ = stopLease()
				cancelDeadline()
				if recordErr != nil {
					return recordErr
				}
				return bindErr
			}
		}
		rt.loop.Budget = tracker
		workCtx, stopOperations := coordinatorOperationContext(workCtx, store, workspace, runID, item, *worker)
		defer stopOperations(nil)
		workCtx = coordinatorLearningContext(workCtx, store, workspace, runID, item)
		prompt := projectcoord.BuildWorkerPrompt(run, item)
		output, workerErr := runOneShotForCoordinator(workCtx, rt, prompt)
		// Cleanup settles pending distillation/auto-memory while both leases and
		// the physical-request observer still authorize this assignment. Only
		// after that may completion clear the task lease or release the job.
		rt.Cleanup()
		rt = nil
		if cause := context.Cause(workCtx); errors.Is(cause, projectcoord.ErrOperationUncertain) {
			workerErr = cause
		}
		if cause := context.Cause(workCtx); cause != nil && item.PrincipalBinding != nil {
			workerErr = cause
		}
		outcome := coordinatorRunOutcome{ItemID: item.ID, Subject: item.Subject, Output: output}
		if workerErr != nil {
			failure := coordinatorWorkerFailure(ctx, workerErr)
			result, recordErr := recordCoordinatorFailure(store, workspace, runID, item, *worker, failure, output)
			if recordErr != nil {
				_ = stopLease()
				cancelDeadline()
				return recordErr
			}
			outcome.Status = string(result.Status)
			outcome.Error = failure.Summary
		} else if workerReportedBlocked(output) {
			failure := coordinatorFailureFromText(output)
			failure.Code = projectcoord.FailureWorkerBlocked
			failure.Recoverable = false
			result, recordErr := recordCoordinatorFailure(store, workspace, runID, item, *worker, failure, output)
			if recordErr != nil {
				_ = stopLease()
				cancelDeadline()
				return recordErr
			}
			outcome.Status = string(result.Status)
			outcome.Error = failure.Summary
		} else {
			result, recordErr := store.Complete(workCtx, workspace, runID, item.ID, projectcoord.CompletionInput{Worker: *worker, Token: item.Lease.Token, Output: output})
			if recordErr != nil {
				failure := projectcoord.Failure{Code: "acceptance_failed", Summary: recordErr.Error(), SuggestedAction: "Fix the failing check or missing deliverable, then recover this work item."}
				if errors.Is(ctx.Err(), context.Canceled) {
					failure = coordinatorWorkerFailure(ctx, recordErr)
				}
				result, recordErr = recordCoordinatorFailure(store, workspace, runID, item, *worker, failure, output)
				if recordErr != nil {
					_ = stopLease()
					cancelDeadline()
					return recordErr
				}
				outcome.Error = failure.Summary
			}
			outcome.Status = string(result.Status)
		}
		_ = stopLease()
		stopOperations(nil)
		cancelDeadline()
		if item.PrincipalBinding != nil {
			if err := syncPersistentJobUsage(store, *item.PrincipalBinding); err != nil {
				return err
			}
		}
		if err := reconcileCoordinatorLearning(ctx, store, workspace, runID); err != nil {
			return err
		}
		outcomes = append(outcomes, outcome)
		completed++
		if !*jsonOut && !*summaryJSON {
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
	if *jsonOut || *summaryJSON {
		var payload any = map[string]any{"outcomes": outcomes, "run": finalRun}
		if *summaryJSON {
			payload = coordinatorSummary(finalRun, outcomes)
		}
		if err := writeCoordinatorJSON(payload); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return coordinatorRunStatusError(finalRun, outcomes)
	}
	if completed == 0 {
		fmt.Fprintf(os.Stdout, "No ready project work. Run %s is %s.\n", finalRun.ID, finalRun.Status)
	} else {
		fmt.Fprintf(os.Stdout, "\nProject run %s is now %s.\n", finalRun.ID, finalRun.Status)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return coordinatorRunStatusError(finalRun, outcomes)
}

type coordinatorOutcomeSummary struct {
	OutcomesCount int                    `json:"outcomes_count"`
	RunID         string                 `json:"run_id"`
	Status        projectcoord.RunStatus `json:"status"`
}

func coordinatorSummary(run projectcoord.Run, outcomes []coordinatorRunOutcome) coordinatorOutcomeSummary {
	return coordinatorOutcomeSummary{OutcomesCount: len(outcomes), RunID: run.ID, Status: run.Status}
}

func coordinatorWorkerFailure(ctx context.Context, err error) projectcoord.Failure {
	if errors.Is(err, projectcoord.ErrOperationUncertain) {
		return projectcoord.Failure{Code: projectcoord.FailureOperationUncertain, Summary: "A writing tool's outcome needs confirmation before recovery.", SuggestedAction: "Inspect the operation and its external result, then explicitly resolve applied or not_applied before recovering the item."}
	}
	var usageBoundary *transport.UsageObserverError
	if errors.Is(err, projectcoord.ErrUsageUnresolved) || (errors.As(err, &usageBoundary) && usageBoundary != nil && (usageBoundary.Code == "provider_usage_unresolved" || errors.Is(usageBoundary.Err, principal.ErrUsageUnknown) || errors.Is(usageBoundary.Err, projectcoord.ErrUsageUnresolved))) {
		return coordinatorUnsettledUsageFailure()
	}
	if errors.Is(err, principal.ErrInactive) || errors.Is(err, principal.ErrLeaseLost) {
		return projectcoord.Failure{Code: projectcoord.FailureWorkerInterrupted, Summary: "Persistent job paused, retired, or lost its execution lease before this assignment finished.", SuggestedAction: "Resume the active job and settle any unknown provider usage or operation outcome before retrying within the existing attempt budget."}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return projectcoord.Failure{
			Code:            projectcoord.FailureWorkerInterrupted,
			Summary:         "Worker stopped before this work item finished.",
			SuggestedAction: "The unfinished item can be reclaimed within its configured attempt budget; persisted files and usage are retained.",
		}
	}
	if ctx.Err() == nil && coordinatorProviderStreamInterrupted(err) {
		return projectcoord.Failure{
			Code:            projectcoord.FailureProviderStreamInterrupted,
			Summary:         "Provider stream closed before its completion marker was received.",
			SuggestedAction: "Retry unfinished work within the persisted attempt and budget limits; preserve earlier delivery files and evidence.",
		}
	}
	return coordinatorFailureFromText(err.Error())
}

func coordinatorUnsettledUsageFailure() projectcoord.Failure {
	return projectcoord.Failure{Code: "provider_usage_unresolved", Summary: "A previous provider request has unsettled usage; no replacement request was dispatched.", SuggestedAction: "Inspect the recorded provider request HTTP status and reconcile its authoritative usage receipt before explicitly recovering this work item."}
}

// This is deliberately narrower than network-error or HTTP-status recovery.
// Some gateways send a completion-boundary failure in a successful SSE stream
// but label its JSON envelope invalid_request_error. That label alone, generic
// disconnected text, EOF, and actual client HTTP errors are not retry signals.
func coordinatorProviderStreamInterrupted(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var status *transport.HTTPStatusError
	if errors.As(err, &status) && status.StatusCode >= 400 && status.StatusCode < 500 {
		return false
	}
	completionBoundary := func(message string) bool {
		switch strings.ToLower(strings.TrimSpace(message)) {
		case "stream closed before response.completed",
			"stream disconnected before completion: stream closed before response.completed",
			"stream error: stream disconnected before completion: stream closed before response.completed":
			return true
		}
		return false
	}
	type providerErrorEnvelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	parseEnvelope := func(text string) (providerErrorEnvelope, bool) {
		var envelope providerErrorEnvelope
		parseErr := json.Unmarshal([]byte(text), &envelope)
		return envelope, parseErr == nil && envelope.Type == "error"
	}
	allowedEnvelopeType := func(kind string) bool {
		return kind == "invalid_request_error" || kind == "api_error" || kind == "stream_error"
	}
	permanentReason := func(reason string) bool {
		switch reason {
		case "authentication_error", "permission_error", "invalid_api_key", "context_length_exceeded":
			return true
		}
		return false
	}
	var messages []string
	// A permanent outer reason must take precedence over an interrupted inner
	// stream. Check the complete chain before accepting any completion marker.
	for current := err; current != nil; current = errors.Unwrap(current) {
		text := strings.TrimSpace(current.Error())
		lower := strings.ToLower(text)
		if strings.HasPrefix(lower, "anthropic 4") || strings.HasPrefix(lower, "openai 4") || strings.HasPrefix(lower, "http 4") {
			return false
		}
		reason := lower
		if colon := strings.IndexByte(reason, ':'); colon >= 0 {
			reason = strings.TrimSpace(reason[:colon])
		}
		if permanentReason(reason) {
			return false
		}
		// Anthropic's plain error formatter can append the explicit type as
		// "message (authentication_error)". Match only that terminal type code.
		if start := strings.LastIndexByte(lower, '('); start >= 0 && strings.HasSuffix(lower, ")") && permanentReason(lower[start+1:len(lower)-1]) {
			return false
		}
		if envelope, ok := parseEnvelope(text); ok {
			if envelope.Error.Code != "" || envelope.Error.Param != "" || !allowedEnvelopeType(envelope.Error.Type) {
				return false
			}
		}
		messages = append(messages, text)
	}
	for _, text := range messages {
		if completionBoundary(text) {
			return true
		}
		if envelope, ok := parseEnvelope(text); ok && completionBoundary(envelope.Error.Message) {
			return true
		}
	}
	return false
}

func coordinatorRunStatusError(run projectcoord.Run, outcomes []coordinatorRunOutcome) error {
	incomplete := run.Status == projectcoord.RunBlocked || run.Status == projectcoord.RunCancelled
	for _, outcome := range outcomes {
		incomplete = incomplete || outcome.Status == string(projectcoord.ItemFailed) || outcome.Status == string(projectcoord.ItemBlocked) || outcome.Status == string(projectcoord.ItemCancelled)
	}
	if !incomplete {
		return nil
	}
	return &exitcode.IncompleteError{Reason: "project_incomplete", Detail: fmt.Sprintf("Project run %s is %s; inspect its recorded worker failures and acceptance evidence.", run.ID, run.Status)}
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
	if run.MaxBudgetUSD > 0 || run.SpentUSD > 0 {
		fmt.Fprintf(w, "Estimated spend: $%.6f / $%.6f (0 cap = unlimited)\n", run.SpentUSD, run.MaxBudgetUSD)
	}
	if !run.DeadlineAt.IsZero() {
		fmt.Fprintf(w, "Deadline:  %s\n", run.DeadlineAt.Format(time.RFC3339))
	}
	fmt.Fprintf(w, "Host acceptance checks: %d\n", len(run.CheckCommands))
	readiness := projectcoord.HostAcceptanceReadiness(run)
	fmt.Fprintf(w, "Host acceptance configuration: %s\n", readiness.Status)
	fmt.Fprintln(w, readiness.Summary)
	if readiness.ConfigurationHint != "" {
		fmt.Fprintf(w, "Configure on the host: %s\n", readiness.ConfigurationHint)
	}
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

type coordinatorStrings []string

func (v *coordinatorStrings) String() string         { return strings.Join(*v, "; ") }
func (v *coordinatorStrings) Set(value string) error { *v = append(*v, value); return nil }

func coordinatorDeadline(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	deadline, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("--deadline must be RFC3339: %w", err)
	}
	return deadline, nil
}

func recordCoordinatorFailure(store *projectcoord.Store, workspace, runID string, claimed projectcoord.WorkItem, worker string, failure projectcoord.Failure, output string) (projectcoord.WorkItem, error) {
	run, err := store.Get(workspace, runID)
	if err != nil {
		return projectcoord.WorkItem{}, err
	}
	for _, item := range run.Items {
		if item.ID == claimed.ID && item.Status == projectcoord.ItemFailed && item.Failure != nil && (item.Failure.Code == "task_budget_exceeded" || item.Failure.Code == "task_deadline_exceeded" || item.Failure.Code == projectcoord.FailureOperationUncertain) {
			return item, nil
		}
	}
	// A cancelled parent or missing stream marker can hide the dispatch
	// guard's typed error. The durable physical ledger is authoritative:
	// unfinished consumption must never become worker_interrupted auto-ready.
	if failure.Code != projectcoord.FailureOperationUncertain {
		for _, request := range projectcoord.UnresolvedUsageRequests(run) {
			if request.ItemID == claimed.ID {
				failure = coordinatorUnsettledUsageFailure()
				break
			}
		}
	}
	return store.Fail(workspace, runID, claimed.ID, worker, claimed.Lease.Token, failure, output)
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
