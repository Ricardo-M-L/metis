package projectcoord

import (
	"errors"
	"github.com/Ricardo-M-L/metis/internal/execution"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"strings"
	"testing"
	"time"
)

func projectTestBinding(t *testing.T, workspace string) principal.Binding {
	t.Helper()
	workspace, err := execution.CanonicalWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	s := principal.NewStore(t.TempDir())
	j, err := s.Create("job", principal.Definition{Workspace: workspace, Provider: "test", ProviderConfigSHA256: strings.Repeat("a", 64), Profile: principal.Profile{Name: "role", Model: "model", PermissionMode: "plan", Source: "project", Tools: []string{"Read"}}})
	if err != nil {
		t.Fatal(err)
	}
	return j.Binding()
}

func TestPausedPrincipalDoesNotBlockAnotherReadyWorker(t *testing.T) {
	workspace := t.TempDir()
	b := projectTestBinding(t, workspace)
	s := NewStore(t.TempDir(), WithPrincipalCheck(func(*principal.Binding) error { return principal.ErrInactive }))
	run, err := s.Create(CreateInput{Workspace: workspace, Goal: "mixed", Items: []WorkItemInput{{ID: "paused", Subject: "paused", Prompt: "paused", AgentProfile: "role", PrincipalBinding: &b}, {ID: "ordinary", Subject: "ordinary", Prompt: "ordinary"}}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "ordinary-worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != "ordinary" {
		t.Fatal("paused principal blocked independent work")
	}
	loaded, err := s.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Items[0].Attempts != 0 || loaded.Revision <= run.Revision {
		t.Fatal("paused attempts or journal revision changed incorrectly")
	}
}

func TestPrincipalBindingCannotBeAlteredByCallerOrCrossWorkspace(t *testing.T) {
	workspace := t.TempDir()
	s := NewStore(t.TempDir())
	b := projectTestBinding(t, workspace)
	run, err := s.Create(CreateInput{Workspace: workspace, Goal: "goal", Items: []WorkItemInput{{ID: "work", Subject: "work", Prompt: "work", AgentProfile: "role", PrincipalBinding: &b}}})
	if err != nil {
		t.Fatal(err)
	}
	b.Definition.Profile.Tools[0] = "Write"
	run.Items[0].PrincipalBinding.Definition.Profile.Tools[0] = "Edit"
	loaded, err := s.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != stateVersion || loaded.Items[0].PrincipalBinding.Definition.Profile.Tools[0] != "Read" {
		t.Fatal("caller mutated frozen principal task")
	}
	b = projectTestBinding(t, t.TempDir())
	if _, err = s.Create(CreateInput{Workspace: workspace, Goal: "cross", Items: []WorkItemInput{{Subject: "work", Prompt: "work", AgentProfile: "role", PrincipalBinding: &b}}}); err == nil {
		t.Fatal("task uses another principal workspace")
	}
}

func TestPrincipalGuardDoesNotBurnPausedJobAttempts(t *testing.T) {
	workspace := t.TempDir()
	b := projectTestBinding(t, workspace)
	s := NewStore(t.TempDir(), WithPrincipalCheck(func(*principal.Binding) error { return principal.ErrInactive }))
	run, err := s.Create(CreateInput{Workspace: workspace, Goal: "goal", Items: []WorkItemInput{{ID: "work", Subject: "work", Prompt: "work", AgentProfile: "role", PrincipalBinding: &b}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "w", Lease: time.Minute}); !errors.Is(err, principal.ErrInactive) {
		t.Fatal(err)
	}
	loaded, err := s.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Items[0].Attempts != 0 || loaded.Items[0].Lease != nil {
		t.Fatal("paused job consumed a recovery attempt")
	}
}

func TestUnstartedClaimCanBeReleasedButReservedProviderWorkCannot(t *testing.T) {
	s := NewStore(t.TempDir())
	workspace := t.TempDir()
	run, err := s.Create(CreateInput{Workspace: workspace, Goal: "goal", Items: []WorkItemInput{{ID: "work", Subject: "work", Prompt: "work"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "w", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleaseUnstartedClaim(workspace, run.ID, item.ID, "w", item.Lease.Token); err != nil {
		t.Fatal(err)
	}
	item, err = s.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "w", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if item.Attempts != 1 {
		t.Fatal("unstarted release consumed an attempt")
	}
	_, err = s.BeginUsageRequest(workspace, run.ID, BeginUsageRequestInput{ClientRequestID: strings.Repeat("c", 32), ItemID: item.ID, Worker: "w", ClaimToken: item.Lease.Token, Provider: "test", Model: "model", EndpointSHA256: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64), Prices: UsagePriceSnapshot{Source: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleaseUnstartedClaim(workspace, run.ID, item.ID, "w", item.Lease.Token); err == nil {
		t.Fatal("provider dispatch reservation erased")
	}
}
