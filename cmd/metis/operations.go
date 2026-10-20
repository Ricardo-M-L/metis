package main

// This is the host management projection used by Desktop. It deliberately does
// not return execution definitions, prompts, tool results or claim credentials.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

const operationsViewLimit = 200

type operationsSnapshot struct {
	SchemaVersion int              `json:"schema_version"`
	GeneratedAt   time.Time        `json:"generated_at"`
	Workspace     string           `json:"workspace"`
	Jobs          []map[string]any `json:"jobs"`
	Handoffs      []map[string]any `json:"handoffs"`
	Runs          []map[string]any `json:"runs"`
	Learning      []map[string]any `json:"learning"`
	Warnings      []string         `json:"warnings"`
}

type operationsAction struct {
	Action          string `json:"action"`
	ID              string `json:"id,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Name            string `json:"name,omitempty"`
	ExpectedVersion string `json:"expected_version,omitempty"`
	ExpectedEpoch   uint64 `json:"expected_epoch,omitempty"`
	Note            string `json:"note,omitempty"`
}

func cmdOperations(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis operations snapshot|action --cwd DIR [--request-file FILE] --json")
	}
	fs := flag.NewFlagSet("operations "+args[0], flag.ContinueOnError)
	cwd := fs.String("cwd", "", "trusted workspace")
	request := fs.String("request-file", "", "private host action JSON outside the workspace")
	_ = fs.Bool("json", true, "emit safe JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New("unexpected operations arguments")
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	return inJobWorkspace(workspace, func() error {
		switch args[0] {
		case "snapshot":
			if *request != "" {
				return errors.New("snapshot does not accept an action")
			}
			view, err := collectOperationsSnapshot(ctx, workspace)
			if err != nil {
				return err
			}
			return writeCoordinatorJSON(view)
		case "action":
			data, _, err := coordinatorUsageReadPrivateFile(*request, workspace, 16<<10, false)
			if err != nil {
				return err
			}
			a, err := decodeOperationsAction(data)
			if err != nil {
				return err
			}
			if err = applyOperationsAction(ctx, workspace, a); err != nil {
				return err
			}
			return writeCoordinatorJSON(map[string]any{"ok": true})
		default:
			return errors.New("unknown operations command")
		}
	})
}

func collectOperationsSnapshot(ctx context.Context, workspace string) (operationsSnapshot, error) {
	view := operationsSnapshot{SchemaVersion: 1, GeneratedAt: time.Now().UTC(), Workspace: workspace,
		Jobs: []map[string]any{}, Handoffs: []map[string]any{}, Runs: []map[string]any{}, Learning: []map[string]any{}, Warnings: []string{}}
	jobs, err := persistentJobStore().List()
	if err != nil {
		return view, err
	}
	mailbox, err := openHandoffStore()
	if err != nil {
		return view, err
	}
	for _, j := range jobs {
		if j.Definition.Workspace != workspace {
			continue
		}
		if len(view.Jobs) >= operationsViewLimit {
			view.Warnings = append(view.Warnings, "job_view_limit")
			break
		}
		usage := map[string]any{"estimated_usd": j.SpentUSD, "input_tokens": j.InputTokens, "output_tokens": j.OutputTokens, "unresolved_requests": j.UnresolvedRequests}
		hint := ""
		if j.UnresolvedRequests > 0 {
			hint = "Reconcile request receipts before paid work can continue."
		}
		if j.Status == principal.Paused {
			hint = "Resume this job to allow new assignments."
		}
		view.Jobs = append(view.Jobs, map[string]any{"id": j.ID, "name": j.Name, "state": j.Status, "workspace": workspace, "session_id": j.SessionID, "budget": j.Definition.Budget, "usage": usage, "run_count": j.RunCount(), "recovery_hint": hint})
		records, listErr := mailbox.List(ctx, "jobs", "job:"+j.ID)
		if listErr != nil {
			return view, listErr
		}
		for i := len(records) - 1; i >= 0; i-- {
			if len(view.Handoffs) >= operationsViewLimit {
				break
			}
			r := records[i]
			row := map[string]any{"id": r.ID, "job_id": j.ID, "state": r.State, "created_at": r.CreatedAt}
			if r.Admission != nil {
				row["admission"] = r.Admission.SafeStatus()
			}
			if r.Business != nil {
				b := r.Business
				business := map[string]any{"state": b.State, "updated_at": b.UpdatedAt, "run_id": b.Assignment.RunID, "work_item_id": b.Assignment.WorkItemID}
				if b.Failure != nil {
					business["failure_code"] = b.Failure.Code
				}
				if !b.CompletedAt.IsZero() {
					business["completed_at"] = b.CompletedAt
				}
				if b.Acceptance != nil {
					business["evidence_sha256"] = b.Acceptance.EvidenceSHA256
					business["acceptance_passed"] = b.Acceptance.Passed
					business["acceptance_source"] = b.Acceptance.Source
				}
				row["business"] = business
			}
			view.Handoffs = append(view.Handoffs, row)
		}
	}
	store := projectCoordinatorStore(projectcoord.WithProbeContext(ctx))
	runs, err := store.List(workspace)
	if err != nil {
		return view, err
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].UpdatedAt.After(runs[j].UpdatedAt) })
	if len(runs) > operationsViewLimit {
		runs = runs[:operationsViewLimit]
		view.Warnings = append(view.Warnings, "run_view_limit")
	}
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			return view, err
		}
		ops := []map[string]any{}
		for _, op := range run.Operations {
			if op.Status == projectcoord.OperationCompleted {
				continue
			}
			if len(ops) >= operationsViewLimit {
				break
			}
			ops = append(ops, map[string]any{"id": op.ID, "item_id": op.ItemID, "tool_name": op.ToolName, "status": op.Status, "recovery_hint": "Inspect the external result; use coordinator operation resolve and explicit recover after trusted confirmation."})
		}
		requests := []map[string]any{}
		unknownCost := 0
		for _, u := range run.UsageRequests {
			if u.ActualUSD == nil {
				unknownCost++
			}
		}
		for _, u := range projectcoord.UnresolvedUsageRequests(run) {
			if len(requests) >= operationsViewLimit {
				break
			}
			requests = append(requests, map[string]any{"client_request_id": u.ClientRequestID, "provider": u.Provider, "model": u.Model, "status": u.Status, "usage_reported": u.UsageReported, "estimated_usd": u.EstimatedUSD, "actual_usd": u.ActualUSD, "recovery_hint": "Import an authoritative request receipt or configure its billing adapter; missing amounts remain unknown."})
		}
		storage, err := store.StorageUsage(workspace, run.ID)
		if err != nil {
			return view, err
		}
		view.Runs = append(view.Runs, map[string]any{"id": run.ID, "status": run.Status, "updated_at": run.UpdatedAt, "item_count": len(run.Items), "storage": storage, "operations": ops, "usage_requests": requests, "estimated_usd": run.SpentUSD, "actual_cost_unknown_requests": unknownCost})
	}
	cfg, _, err := config.Load()
	if err != nil {
		return view, err
	}
	for _, kind := range []string{"skill", "workflow"} {
		root, err := learningControllerArtifactRoot(cfg, kind)
		if err != nil {
			return view, err
		}
		states, err := learning.NewStore(root).List()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return view, err
		}
		for _, s := range states {
			if len(view.Learning) >= operationsViewLimit {
				view.Warnings = append(view.Warnings, "learning_view_limit")
				break
			}
			row := map[string]any{"kind": kind, "name": s.Name, "active_version": s.ActiveVersion, "baseline_version": s.BaselineVersion, "activation_epoch": s.ActivationEpoch, "revision": s.Revision, "suspended": s.Suspended}
			if active, ok := s.Versions[s.ActiveVersion]; ok {
				row["rollback_version"] = active.Parent
			}
			if s.Quality != nil {
				q := s.Quality
				failures, completed := 0, 0
				for _, r := range q.Records {
					if r.Outcome != nil {
						completed++
						if !r.Outcome.Passed {
							failures++
						}
					}
				}
				row["quality"] = map[string]any{"paused": q.Paused, "paused_reason": q.PausedReason, "feedback_count": completed, "failure_count": failures, "policy": q.Policy}
			}
			view.Learning = append(view.Learning, row)
		}
	}
	return view, nil
}

func decodeOperationsAction(data []byte) (operationsAction, error) {
	var a operationsAction
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return a, errors.New("invalid management action")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return a, errors.New("management action requires one JSON object")
	}
	if len(a.Note) > 512 || strings.ContainsRune(a.Note, 0) {
		return a, errors.New("invalid management action note")
	}
	switch a.Action {
	case "pause_job", "resume_job":
		if a.ID == "" || a.Kind != "" || a.Name != "" || a.ExpectedVersion != "" || a.ExpectedEpoch != 0 || a.Note != "" {
			return a, errors.New("job action accepts only its ID")
		}
	case "rollback_learning", "resume_learning":
		if (a.Kind != "skill" && a.Kind != "workflow") || learning.ValidateName(a.Name) != nil || len(a.ExpectedVersion) != 64 || a.ExpectedEpoch == 0 || strings.TrimSpace(a.Note) == "" || a.ID != "" {
			return a, errors.New("learning action requires kind, name, current version and epoch, and a review note")
		}
	default:
		return a, errors.New("unsupported management action")
	}
	return a, nil
}

func applyOperationsAction(ctx context.Context, workspace string, a operationsAction) error {
	if a.Action == "pause_job" || a.Action == "resume_job" {
		j, err := persistentJobStore().Get(a.ID)
		if err != nil {
			return err
		}
		if j.Definition.Workspace != workspace {
			return errors.New("job is outside this workspace")
		}
		status := principal.Paused
		if a.Action == "resume_job" {
			status = principal.Active
		}
		_, err = persistentJobStore().SetStatus(a.ID, status)
		return err
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	root, err := learningControllerArtifactRoot(cfg, a.Kind)
	if err != nil {
		return err
	}
	s := learning.NewStore(filepath.Clean(root))
	if a.Action == "rollback_learning" {
		return s.RollbackCurrent(ctx, a.Name, a.ExpectedVersion, a.ExpectedEpoch, a.Note)
	}
	if a.Action == "resume_learning" {
		return s.ResumeQualityCurrent(ctx, a.Name, a.ExpectedVersion, a.ExpectedEpoch, a.Note)
	}
	return fmt.Errorf("unsupported management action")
}
