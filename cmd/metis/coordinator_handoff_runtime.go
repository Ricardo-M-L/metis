package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

// configureCoordinatorHandoff is called after restoring the principal's
// runtime and before Run. Business workers receive only their exact assigned
// envelope. Other principal workers may drain notifications, while structured
// tasks remain available to the host dispatcher.
func configureCoordinatorHandoff(rt *runtime, run projectcoord.Run, item projectcoord.WorkItem) error {
	if item.PrincipalBinding == nil {
		if item.Handoff != nil {
			return errors.New("coordinator handoff requires a frozen principal binding")
		}
		return nil
	}
	if rt == nil || rt.loop == nil {
		return errors.New("coordinator handoff runtime is unavailable")
	}
	binding := item.PrincipalBinding
	if err := principal.ValidateBinding(binding, run.Workspace); err != nil {
		return fmt.Errorf("coordinator handoff principal: %w", err)
	}
	job, err := persistentJobStore().Get(binding.JobID)
	if err != nil {
		return err
	}
	if job.ConfigSHA256 != binding.ConfigSHA256 || rt.sessionID != job.SessionID {
		return errors.New("coordinator handoff runtime does not own the frozen principal transcript")
	}
	store, err := openHandoffStore()
	if err != nil {
		return err
	}
	inbox, err := agent.NewDurablePeerInbox(store, "jobs", "job:"+binding.JobID)
	if err != nil {
		return err
	}
	if item.Handoff == nil {
		inbox.NotificationsOnly = true
		rt.loop.DurablePeer = inbox
		return nil
	}
	h := item.Handoff
	if item.Phase != projectcoord.PhaseImplementation || item.ID != "handoff-"+h.MessageID {
		return errors.New("coordinator handoff execution item identity mismatch")
	}
	expected := handoff.BusinessAssignment{
		JobID: binding.JobID, Workspace: run.Workspace, RunID: run.ID,
		WorkItemID: item.ID, VerificationItemID: item.ID + "-verify", ConfigSHA256: binding.ConfigSHA256,
	}
	record, err := store.Get(context.Background(), h.MessageID)
	if err != nil {
		return fmt.Errorf("read coordinator handoff assignment: %w", err)
	}
	if record.ID != h.MessageID || record.Scope != "jobs" || record.To != inbox.Recipient || record.Task == nil || record.Task.ID != h.TaskID || record.ContentSHA256 != h.ContentSHA256 || record.Business == nil || record.Business.Assignment != expected {
		return errors.New("coordinator handoff does not match its durable business assignment")
	}
	// The envelope digest freezes the sender's contract, and the graph stores
	// the host-observed file hashes. Both references must describe the same
	// complete input set before the model receives either representation.
	if len(record.Task.Artifacts) != len(h.InputArtifacts) || len(record.Task.Deliverables) != len(item.Deliverables) {
		return errors.New("coordinator handoff artifact contract mismatch")
	}
	inputs := make(map[string]string, len(record.Task.Artifacts))
	for _, artifact := range record.Task.Artifacts {
		path := filepath.Clean(artifact.Path)
		if _, exists := inputs[path]; exists {
			return errors.New("coordinator handoff repeats an input artifact")
		}
		inputs[path] = artifact.SHA256
	}
	for _, artifact := range h.InputArtifacts {
		if inputs[artifact.Path] != artifact.SHA256 {
			return errors.New("coordinator handoff input artifact hash mismatch")
		}
		delete(inputs, artifact.Path)
	}
	if len(inputs) != 0 {
		return errors.New("coordinator handoff input artifact set mismatch")
	}
	outputs := make(map[string]bool, len(record.Task.Deliverables))
	for _, path := range record.Task.Deliverables {
		outputs[path] = true
	}
	for _, path := range item.Deliverables {
		if !outputs[path] {
			return errors.New("coordinator handoff deliverable set mismatch")
		}
		delete(outputs, path)
	}
	if len(outputs) != 0 {
		return errors.New("coordinator handoff deliverable set mismatch")
	}
	inbox.MessageID = h.MessageID
	inbox.Assignment = &expected
	inbox.ContentSHA256 = h.ContentSHA256
	rt.loop.DurablePeer = inbox
	return nil
}
