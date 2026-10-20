package projectcoord

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"strings"
	"time"
)

type EventInput struct {
	Source               string
	ID                   string
	PrincipalBinding     *principal.Binding
	AgentProfile         string
	VerifierAgentProfile string
	Subject              string
	Prompt               string
	Deliverables         []string
}

// EnqueueEvent atomically deduplicates an external event and schedules work plus verification.
func (s *Store) EnqueueEvent(workspace, runID string, input EventInput) (WorkItem, error) {
	input.Source, input.ID = strings.TrimSpace(input.Source), strings.TrimSpace(input.ID)
	if input.Source == "" || input.ID == "" || len(input.Source) > 128 || len(input.ID) > 256 {
		return WorkItem{}, errors.New("event source and ID are required (maximum 128/256 bytes)")
	}
	sum := sha256.Sum256([]byte(input.Source + "\x00" + input.ID))
	id := "event-" + hex.EncodeToString(sum[:16])
	var enqueued WorkItem
	updated, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		for _, item := range run.Items {
			if item.ID == id {
				if !equalPrincipalBindings(item.PrincipalBinding, input.PrincipalBinding) || item.EventSource != input.Source || item.EventID != input.ID || item.AgentProfile != input.AgentProfile || item.Subject != strings.TrimSpace(input.Subject) || item.Prompt != strings.TrimSpace(input.Prompt) || strings.Join(item.Deliverables, "\x00") != strings.Join(input.Deliverables, "\x00") {
					return false, errors.New("event ID already exists with a different payload")
				}
				verifier, err := run.item(id + "-verify")
				if err != nil || verifier.AgentProfile != input.VerifierAgentProfile {
					return false, errors.New("event ID already exists with a different verifier profile")
				}
				enqueued = cloneItem(item)
				return false, nil
			}
		}
		if run.Status == RunCancelled {
			return false, errors.New("cannot enqueue an event on a cancelled project")
		}
		items, err := materializeItems([]WorkItemInput{
			{ID: id, Phase: PhaseImplementation, Subject: input.Subject, Prompt: input.Prompt, AgentProfile: input.AgentProfile, PrincipalBinding: input.PrincipalBinding, Deliverables: input.Deliverables},
			{ID: id + "-verify", Phase: PhaseVerification, Subject: "Verify: " + input.Subject, Prompt: "Independently verify the event work, its deliverables and the project goal. Report concrete evidence and blockers.", DependsOn: []string{id}, AgentProfile: input.VerifierAgentProfile},
		}, s.now())
		if err != nil {
			return false, err
		}
		if err := principal.ValidateBinding(items[0].PrincipalBinding, run.Workspace); err != nil {
			return false, err
		}
		items[0].EventSource, items[0].EventID = input.Source, input.ID
		candidate := append(append([]WorkItem(nil), run.Items...), items...)
		if err := validateGraph(candidate); err != nil {
			return false, err
		}
		run.Items = candidate
		run.CompletedAt = time.Time{}
		run.UpdatedAt = s.now()
		run.addEvent(s.now(), "event_enqueued", id, fmt.Sprintf("%s / %s", input.Source, input.ID), input.AgentProfile)
		enqueued = cloneItem(items[0])
		return true, nil
	})
	if err == nil {
		if item, itemErr := updated.item(enqueued.ID); itemErr == nil {
			enqueued = cloneItem(*item)
		}
	}
	return enqueued, err
}

func equalPrincipalBindings(a, b *principal.Binding) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(aa, bb)
}
