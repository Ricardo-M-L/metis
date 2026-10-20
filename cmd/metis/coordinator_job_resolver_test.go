package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/principal"
)

func TestProjectPrincipalResolverKeepsFrozenIdentityAndWorkspace(t *testing.T) {
	job, _, _, _ := newPersistentJobCLIFixture(t, 0)
	binding, err := resolveProjectPrincipalBinding(context.Background(), job.Definition.Workspace, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.JobID != job.ID || binding.ConfigSHA256 != job.ConfigSHA256 || binding.Definition.Profile.SystemPrompt != job.Definition.Profile.SystemPrompt {
		t.Fatal("tool resolver changed the saved job identity or frozen role")
	}
	if _, err := resolveProjectPrincipalBinding(context.Background(), t.TempDir(), job.ID); err == nil {
		t.Fatal("tool resolver accepted an unrelated workspace")
	}
	if _, err := persistentJobStore().SetStatus(job.ID, principal.Paused); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveProjectPrincipalBinding(context.Background(), job.Definition.Workspace, job.ID); !errors.Is(err, principal.ErrInactive) {
		t.Fatalf("paused job admission: %v", err)
	}
}

func TestProjectPrincipalResolverCancellationStopsAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolveProjectPrincipalBinding(ctx, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resolver did not stop before filesystem admission: %v", err)
	}
}
