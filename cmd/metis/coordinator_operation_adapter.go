package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// coordinatorOperationAdapter belongs to exactly one work-item claim. Writes
// are serialized even when a tool advertises concurrency-safe execution (MCP
// tools can do that while publishing remote side effects). Reads still fan out.
type coordinatorOperationAdapter struct {
	store     *projectcoord.Store
	workspace string
	runID     string
	itemID    string
	worker    string
	token     string
	cancel    context.CancelCauseFunc
	writes    sync.Mutex
	sequence  map[coordinatorOperationSignature]int
}

type coordinatorOperationSignature struct {
	tool   string
	digest [sha256.Size]byte
}

func coordinatorOperationContext(ctx context.Context, store *projectcoord.Store, workspace, runID string, item projectcoord.WorkItem, worker string) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	token := ""
	if item.Lease != nil {
		token = item.Lease.Token
	}
	adapter := &coordinatorOperationAdapter{store: store, workspace: workspace, runID: runID, itemID: item.ID, worker: worker, token: token, cancel: cancel}
	return pubtool.WithOperationExecutor(ctx, adapter), cancel
}

func (a *coordinatorOperationAdapter) Execute(ctx context.Context, tool pubtool.Tool, input map[string]any, invoke func(context.Context) (*pubtool.Result, error)) (*pubtool.Result, error) {
	if tool.Name() == "Bash" {
		for _, flag := range []string{"run_in_background", "await_completion", "required_for_completion"} {
			if enabled, _ := input[flag].(bool); enabled {
				// Bash's await flags also select background execution. Refuse
				// before preparing or spawning anything: a job-start acknowledgement
				// cannot confirm an external effect or survive a worker restart.
				return &pubtool.Result{IsError: true, Output: "This durable worker requires Bash to finish in the foreground. The command was not executed. Omit run_in_background, await_completion and required_for_completion, or set them to false."}, nil
			}
		}
	}
	if !coordinatorGuardedOperation(tool, input) {
		return invoke(ctx)
	}
	a.writes.Lock()
	defer a.writes.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var identityInput any = canonicalOperationInput(tool, input)
	if tool.Name() == "Bash" {
		// Bash gets a child's cwd from context, otherwise it inherits the
		// process cwd. Keep that execution scope in the digest without storing
		// any raw path or arguments in the journal.
		cwd := agent.CwdFromContext(ctx)
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				return nil, a.stopUncertain("", errors.New("writing tool execution directory could not be identified before execution"))
			}
		}
		identityInput = struct {
			Input        any    `json:"input"`
			EffectiveCwd string `json:"effective_cwd"`
		}{Input: identityInput, EffectiveCwd: cwd}
	}
	encoded, err := json.Marshal(identityInput)
	if err != nil {
		return nil, a.stopUncertain("", errors.New("writing tool arguments could not be encoded before execution"))
	}
	digest := sha256.Sum256(encoded)
	if a.sequence == nil {
		a.sequence = make(map[coordinatorOperationSignature]int)
	}
	signature := coordinatorOperationSignature{tool: tool.Name(), digest: digest}
	a.sequence[signature]++
	// One claim may deliberately perform the same action more than once. A
	// replacement claim starts this per-signature sequence from one and replays
	// the corresponding previously confirmed occurrence instead of resending it.
	op, err := a.store.BeginOperation(a.workspace, a.runID, a.itemID, a.worker, a.token, projectcoord.OperationInput{ToolName: tool.Name(), InputSHA256: hex.EncodeToString(digest[:]), Occurrence: a.sequence[signature]})
	if err != nil {
		// A stale claim or existing unknown write must stop the worker before
		// it can reformulate its arguments and attempt another side effect.
		return nil, a.stopUncertain("", err)
	}
	if op.Status == projectcoord.OperationCompleted {
		var result pubtool.Result
		decoder := json.NewDecoder(bytes.NewReader(op.ResultJSON))
		// Receipt identifiers in Meta/Presentation may exceed float64's exact
		// integer range. Keep their JSON spelling intact during durable replay.
		decoder.UseNumber()
		if err := decoder.Decode(&result); err != nil || result.IsError {
			return nil, a.stopUncertain(op.ID, errors.New("confirmed operation result could not be replayed"))
		}
		return &result, nil
	}
	if op.Status != projectcoord.OperationStarted {
		return nil, a.stopUncertain(op.ID, errors.New("writing operation is not durably prepared"))
	}
	if err := ctx.Err(); err != nil {
		return nil, a.markUncertain(op.ID, "The worker was interrupted after preparing the write and before receiving a confirmed result.", err)
	}
	result, callErr := invoke(ctx)
	backgroundResult := false
	if result != nil {
		kind, _ := result.Presentation["kind"].(string)
		backgroundResult = kind == "background_job"
	}
	if callErr != nil || result == nil || result.IsError || ctx.Err() != nil || backgroundResult {
		cause := callErr
		if cause == nil {
			switch {
			case ctx.Err() != nil:
				cause = ctx.Err()
			case result == nil:
				cause = errors.New("writing tool returned no result")
			case backgroundResult:
				cause = errors.New("writing tool returned an unfinished background job instead of a confirmed result")
			default:
				cause = errors.New("writing tool reported an error; its external outcome is unknown")
			}
		}
		return nil, a.markUncertain(op.ID, "The writing tool did not return a confirmed successful result; inspect its external outcome before retrying.", cause)
	}
	fullResult, err := json.Marshal(result)
	if err != nil {
		return nil, a.markUncertain(op.ID, "The writing tool succeeded but its complete result could not be encoded for durable replay.", errors.New("writing tool result could not be encoded"))
	}
	if _, err := a.store.FinishOperation(a.workspace, a.runID, a.itemID, a.worker, a.token, op.ID, fullResult); err != nil {
		return nil, a.markUncertain(op.ID, "The writing tool returned success but its result could not be durably confirmed.", err)
	}
	return result, nil
}

// canonicalOperationInput removes only verified presentation/time-budget
// fields from the two built-in code tools. The actual call retains the original
// input. Command/code bytes, language, scope and unknown tool fields remain
// exact: equivalent shell spellings cannot be proved equivalent automatically.
func canonicalOperationInput(tool pubtool.Tool, input map[string]any) map[string]any {
	var ignored map[string]bool
	switch tool.Name() {
	case "Bash":
		ignored = map[string]bool{"description": true, "timeout_ms": true}
	case "RunCode":
		ignored = map[string]bool{"timeout": true}
	default:
		return input
	}
	canonical := make(map[string]any, len(input))
	for field, value := range input {
		if tool.Name() == "Bash" && (field == "run_in_background" || field == "await_completion" || field == "required_for_completion") {
			// True is refused before dispatch. An explicitly false flag and
			// the omitted default both select the same foreground action.
			if enabled, ok := value.(bool); ok && !enabled {
				continue
			}
		}
		if !ignored[field] {
			canonical[field] = value
		}
	}
	return canonical
}

func coordinatorGuardedOperation(tool pubtool.Tool, input map[string]any) bool {
	switch tool.Name() {
	case "Agent", "Fork", "Ralph":
		// Execution containers propagate this executor to their child calls.
		// Caching the container could suppress unfinished child work.
		return false
	case "Bash", "RunCode":
		// Their read-only heuristics are not a proof that arbitrary code cannot
		// publish externally or mutate state (find -exec, awk system, etc.).
		return true
	default:
		return !pubtool.IsReadOnly(tool, input)
	}
}

func (a *coordinatorOperationAdapter) markUncertain(id, note string, cause error) error {
	_, markErr := a.store.MarkOperationUncertain(a.workspace, a.runID, a.itemID, a.worker, a.token, id, note)
	return a.stopUncertain(id, errors.Join(cause, markErr))
}

func (a *coordinatorOperationAdapter) stopUncertain(id string, cause error) error {
	held := &projectcoord.OperationUncertainError{ItemID: a.itemID, OperationID: id, Status: projectcoord.OperationUncertain}
	err := errors.Join(held, cause)
	if a.cancel != nil {
		a.cancel(err)
	}
	return fmt.Errorf("durable writing operation stopped: %w", err)
}
